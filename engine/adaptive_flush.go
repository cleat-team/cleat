package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// batchEntry holds a pre-computed event to be persisted.
type batchEntry struct {
	workflowID string
	step       int
	done       chan error
	params     []interface{} // 31 values matching insertEventSQL parameter order
}

// AdaptiveFlusher tracks the recent step rate and automatically switches
// between direct (low-rate) and batched (high-rate) event persistence.
//
// In batch mode, events accumulate in a mutex-protected slice. When the
// batch reaches maxBatch or maxWait elapses, the goroutine that triggers
// the flush does the DB work directly — no channels, no worker pool.
type AdaptiveFlusher struct {
	mu        sync.Mutex
	batchMode bool

	rateAlpha  float64
	rateEWMA   float64
	lastCount  int64
	lastSample time.Time

	enterThreshold float64
	exitThreshold  float64

	// Accumulator state (batch mode only)
	events   []batchEntry
	timer    *time.Timer
	db       *sql.DB
	tenantID string
	maxWait  time.Duration
	maxBatch int

	encryptSensitivePayloads bool
	encryption               *PayloadEncryption

	// Stats
	directFlushes   atomic.Int64
	batchFlushes    atomic.Int64
	batchedEvents   atomic.Int64
	totalPrepareUs  atomic.Int64
	totalFlushUs    atomic.Int64
	totalMarshalUs  atomic.Int64
	totalDBUs       atomic.Int64
	batchSizeTotal  atomic.Int64
	batchCount      atomic.Int64
	timerFlushes    atomic.Int64
	fullFlushes     atomic.Int64
	lastReportTime  time.Time
}

func NewAdaptiveFlusher(db *sql.DB, tenantID string, maxWait time.Duration, maxBatch int, enterThreshold, exitThreshold float64, _ int) *AdaptiveFlusher {
	if maxWait <= 0 {
		maxWait = 5 * time.Millisecond
	}
	if maxBatch <= 0 {
		maxBatch = 200
	}
	if enterThreshold <= 0 {
		enterThreshold = 500.0
	}
	if exitThreshold <= 0 {
		exitThreshold = 250.0
	}

	return &AdaptiveFlusher{
		db:             db,
		tenantID:       tenantID,
		rateAlpha:      0.2,
		maxWait:        maxWait,
		maxBatch:       maxBatch,
		enterThreshold: enterThreshold,
		exitThreshold:  exitThreshold,
		lastSample:     time.Now(),
	}
}

func (af *AdaptiveFlusher) SetEncryption(encrypt bool, enc *PayloadEncryption) {
	af.mu.Lock()
	defer af.mu.Unlock()
	af.encryptSensitivePayloads = encrypt
	af.encryption = enc
}

// Flush is called from recordEvent. In direct mode it returns (nil, false)
// and the caller falls through to flushEvent. In batch mode it returns a
// done channel; the caller blocks on <-done until the batch is persisted.
func (af *AdaptiveFlusher) Flush(ctx context.Context, workflowID string, rec EventRecord, checksum string) (chan error, bool) {
	af.updateRate()

	af.mu.Lock()
	batchMode := af.batchMode
	af.mu.Unlock()

	if !batchMode {
		af.directFlushes.Add(1)
		return nil, false
	}

	t0 := time.Now()
	entry, err := af.prepareEntry(workflowID, rec, checksum)
	af.totalPrepareUs.Add(time.Since(t0).Microseconds())
	if err != nil {
		done := make(chan error, 1)
		done <- err
		return done, true
	}

	done := make(chan error, 1)
	entry.done = done

	af.mu.Lock()
	af.events = append(af.events, entry)
	n := len(af.events)

	if n == 1 {
		// First event in a new batch — start the timer.
		if af.timer == nil {
			af.timer = time.AfterFunc(af.maxWait, af.onTimer)
		} else {
			af.timer.Reset(af.maxWait)
		}
	}

	if n >= af.maxBatch {
		// Batch full — flush in a new goroutine so the next batch
		// can start accumulating while this one commits.
		batch := af.events
		af.events = nil
		if af.timer != nil {
			af.timer.Stop()
		}
		af.fullFlushes.Add(1)
		af.mu.Unlock()
		go af.flushAndNotify(ctx, batch)
		return done, true
	}

	af.mu.Unlock()
	return done, true
}

// onTimer is called when maxWait elapses without the batch filling up.
func (af *AdaptiveFlusher) onTimer() {
	af.mu.Lock()
	batch := af.events
	af.events = nil
	af.mu.Unlock()

	if len(batch) > 0 {
		af.timerFlushes.Add(1)
		go af.flushAndNotify(context.Background(), batch)
	}
}

func (af *AdaptiveFlusher) flushAndNotify(ctx context.Context, batch []batchEntry) {
	tStart := time.Now()
	events := make([]map[string]interface{}, len(batch))
	for i, entry := range batch {
		p := entry.params
		events[i] = map[string]interface{}{
			"workflow_id":       p[0],
			"step":              p[1],
			"event_type":        p[2],
			"service":           jsonNull(p[3]),
			"operation":         jsonNull(p[4]),
			"request":           jsonNull(p[5]),
			"response":          jsonNull(p[6]),
			"error":             jsonNull(p[7]),
			"duration_ms":       jsonNull(p[8]),
			"signal_names":      jsonNull(p[9]),
			"timeout_ms":        jsonNull(p[10]),
			"signal_name":       jsonNull(p[11]),
			"signal_payload":    jsonNull(p[12]),
			"defer_description": jsonNull(p[13]),
			"defer_id":          jsonNull(p[14]),
			"child_name":        jsonNull(p[15]),
			"child_input":       jsonNull(p[16]),
			"run_id":            jsonNull(p[17]),
			"new_input":         jsonNull(p[18]),
			"plugin_name":       jsonNull(p[19]),
			"plugin_func":       jsonNull(p[20]),
			"plugin_input":      jsonNull(p[21]),
			"plugin_output":     jsonNull(p[22]),
			"plugin_error":      jsonNull(p[23]),
			"promise_name":      jsonNull(p[24]),
			"promise_id":        jsonNull(p[25]),
			"promise_result":    jsonNull(p[26]),
			"promise_error":     jsonNull(p[27]),
			"payload":           jsonNull(p[28]),
			"checksum":          p[29],
			"tenant_id":         p[30],
			"created_at":        time.Now(),
		}
	}

	t0 := time.Now()
	eventsJSON, err := json.Marshal(events)
	marshalUs := time.Since(t0).Microseconds()
	af.totalMarshalUs.Add(marshalUs)
	if err != nil {
		for _, entry := range batch {
			if entry.done != nil {
				entry.done <- err
			}
		}
		return
	}

	t1 := time.Now()
	_, err = af.db.ExecContext(ctx, "SELECT batch_flush_events($1::jsonb)", string(eventsJSON))
	dbUs := time.Since(t1).Microseconds()
	af.totalDBUs.Add(dbUs)
	if err != nil {
		for _, entry := range batch {
			if entry.done != nil {
				entry.done <- err
			}
		}
		return
	}

	af.batchFlushes.Add(1)
	af.batchedEvents.Add(int64(len(batch)))
	af.batchSizeTotal.Add(int64(len(batch)))
	af.batchCount.Add(1)
	af.totalFlushUs.Add(time.Since(tStart).Microseconds())

	for _, entry := range batch {
		if entry.done != nil {
			close(entry.done)
		}
	}

	// Periodic report every ~5 seconds.
	now := time.Now()
	if now.Sub(af.lastReportTime) >= 5*time.Second {
		af.lastReportTime = now
		bc := af.batchCount.Load()
		bs := af.batchSizeTotal.Load()
		bf := af.batchFlushes.Load()
		be := af.batchedEvents.Load()
		df := af.directFlushes.Load()
		avgBatch := float64(0)
		if bc > 0 {
			avgBatch = float64(bs) / float64(bc)
		}
		avgFlush := float64(0)
		if bf > 0 {
			avgFlush = float64(af.totalFlushUs.Load()) / float64(bf)
		}
		avgMarshal := float64(0)
		if bf > 0 {
			avgMarshal = float64(af.totalMarshalUs.Load()) / float64(bf)
		}
		avgDB := float64(0)
		if bf > 0 {
			avgDB = float64(af.totalDBUs.Load()) / float64(bf)
		}
		avgPrepare := float64(0)
		if be > 0 {
			avgPrepare = float64(af.totalPrepareUs.Load()) / float64(be)
		}
		slog.Info("ADAPTIVE-STATS",
			"batchMode", af.batchMode,
			"rate", af.rateEWMA,
			"directFlushes", df,
			"batchFlushes", bf,
			"batchedEvents", be,
			"avgBatchSize", fmt.Sprintf("%.1f", avgBatch),
			"avgFlushUs", fmt.Sprintf("%.0f", avgFlush),
			"avgMarshalUs", fmt.Sprintf("%.0f", avgMarshal),
			"avgDBUs", fmt.Sprintf("%.0f", avgDB),
			"avgPrepareUs", fmt.Sprintf("%.0f", avgPrepare),
			"timerFlushes", af.timerFlushes.Load(),
			"fullFlushes", af.fullFlushes.Load(),
		)
	}
}

// Run is a no-op in this design — no background goroutines needed.
func (af *AdaptiveFlusher) Run(ctx context.Context) {
	<-ctx.Done()
	// Flush any remaining events on shutdown.
	af.mu.Lock()
	batch := af.events
	af.events = nil
	if af.timer != nil {
		af.timer.Stop()
	}
	af.mu.Unlock()
	if len(batch) > 0 {
		go af.flushAndNotify(context.Background(), batch)
	}
}

func (af *AdaptiveFlusher) updateRate() {
	af.mu.Lock()
	defer af.mu.Unlock()

	now := time.Now()
	if now.Sub(af.lastSample) < 100*time.Millisecond {
		return
	}

	current := atomic.LoadInt64(&freshStepCount)
	elapsed := now.Sub(af.lastSample).Seconds()

	var rate float64
	if elapsed > 0 && current >= af.lastCount {
		rate = float64(current-af.lastCount) / elapsed
	}
	af.lastCount = current
	af.lastSample = now

	if af.rateEWMA == 0 {
		af.rateEWMA = rate
	} else {
		af.rateEWMA = af.rateAlpha*rate + (1-af.rateAlpha)*af.rateEWMA
	}

	if !af.batchMode && af.rateEWMA >= af.enterThreshold {
		af.batchMode = true
		slog.Info("adaptive flusher entered batch mode", "rate", af.rateEWMA)
	} else if af.batchMode && af.rateEWMA < af.exitThreshold {
		af.batchMode = false
		slog.Info("adaptive flusher exited batch mode", "rate", af.rateEWMA)
	}
}

// jsonNull converts sql.Null* types to JSON-safe values (string, int64, or nil)
// so that json.Marshal produces null instead of {"String":"","Valid":false}.
func jsonNull(v interface{}) interface{} {
	switch t := v.(type) {
	case sql.NullString:
		if t.Valid {
			return t.String
		}
		return nil
	case sql.NullInt64:
		if t.Valid {
			return t.Int64
		}
		return nil
	default:
		return v
	}
}

func (af *AdaptiveFlusher) prepareEntry(workflowID string, rec EventRecord, checksum string) (batchEntry, error) {
	payloadJSON, _ := eventRecordToPayload(rec)
	payloadArg := nullStr("")
	if len(payloadJSON) > 0 {
		payloadArg = sql.NullString{String: string(payloadJSON), Valid: true}
	}

	requestStr := tryEncodeBase64(rec.Request)
	responseStr := tryEncodeBase64(rec.Response)
	errStr := rec.Err
	sigPayload := rec.SignalPayload
	childInput := rec.ChildInput
	newInput := rec.NewInput
	pluginInput := rec.PluginInput
	pluginOutput := rec.PluginOutput
	promiseResult := rec.PromiseResult
	promiseError := rec.PromiseError

	af.mu.Lock()
	encrypt := af.encryptSensitivePayloads
	enc := af.encryption
	af.mu.Unlock()

	if encrypt && enc != nil {
		var encErr error
		if requestStr, encErr = enc.EncryptString(rec.Request); encErr != nil {
			return batchEntry{}, fmt.Errorf("prepare entry: encrypt request: %w", encErr)
		}
		if responseStr, encErr = enc.EncryptString(rec.Response); encErr != nil {
			return batchEntry{}, fmt.Errorf("prepare entry: encrypt response: %w", encErr)
		}
		if errStr, encErr = enc.EncryptString(rec.Err); encErr != nil {
			return batchEntry{}, fmt.Errorf("prepare entry: encrypt err: %w", encErr)
		}
		if rec.SignalPayload != "" {
			if sigPayload, encErr = enc.EncryptString(rec.SignalPayload); encErr != nil {
				return batchEntry{}, fmt.Errorf("prepare entry: encrypt signal_payload: %w", encErr)
			}
		}
		if rec.ChildInput != "" {
			if childInput, encErr = enc.EncryptString(rec.ChildInput); encErr != nil {
				return batchEntry{}, fmt.Errorf("prepare entry: encrypt child_input: %w", encErr)
			}
		}
		if rec.NewInput != "" {
			if newInput, encErr = enc.EncryptString(rec.NewInput); encErr != nil {
				return batchEntry{}, fmt.Errorf("prepare entry: encrypt new_input: %w", encErr)
			}
		}
		if rec.PluginInput != "" {
			if pluginInput, encErr = enc.EncryptString(rec.PluginInput); encErr != nil {
				return batchEntry{}, fmt.Errorf("prepare entry: encrypt plugin_input: %w", encErr)
			}
		}
		if rec.PluginOutput != "" {
			if pluginOutput, encErr = enc.EncryptString(rec.PluginOutput); encErr != nil {
				return batchEntry{}, fmt.Errorf("prepare entry: encrypt plugin_output: %w", encErr)
			}
		}
		if rec.PromiseResult != "" {
			if promiseResult, encErr = enc.EncryptString(rec.PromiseResult); encErr != nil {
				return batchEntry{}, fmt.Errorf("prepare entry: encrypt promise_result: %w", encErr)
			}
		}
		if rec.PromiseError != "" {
			if promiseError, encErr = enc.EncryptString(rec.PromiseError); encErr != nil {
				return batchEntry{}, fmt.Errorf("prepare entry: encrypt promise_error: %w", encErr)
			}
		}
		if len(payloadJSON) > 0 && enc != nil {
			encrypted, encErr := enc.EncryptJSON(payloadJSON)
			if encErr != nil {
				return batchEntry{}, fmt.Errorf("prepare entry: encrypt payload: %w", encErr)
			}
			payloadArg = sql.NullString{String: string(encrypted), Valid: true}
		}
	}

	params := []interface{}{
		workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(requestStr), nullStr(responseStr), nullStr(errStr),
		nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
		nullStr(rec.SignalName), nullStr(sigPayload),
		nullStr(rec.DeferDescription), nullStr(rec.DeferID),
		nullStr(rec.ChildName), nullStr(childInput), nullStr(rec.RunID), nullStr(newInput),
		nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(pluginInput), nullStr(pluginOutput), nullStr(rec.PluginError),
		nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(promiseResult), nullStr(promiseError),
		payloadArg, checksum, af.tenantID,
	}
	return batchEntry{workflowID: workflowID, step: rec.Step, params: params}, nil
}

func (af *AdaptiveFlusher) InBatchMode() bool {
	af.mu.Lock()
	defer af.mu.Unlock()
	return af.batchMode
}

func (af *AdaptiveFlusher) GetRate() float64 {
	af.mu.Lock()
	defer af.mu.Unlock()
	return af.rateEWMA
}

func (af *AdaptiveFlusher) Stats() (int64, int64, int64) {
	return af.directFlushes.Load(), af.batchFlushes.Load(), af.batchedEvents.Load()
}

// FlusherConfig holds the configuration for creating AdaptiveFlusher instances.
type FlusherConfig struct {
	MaxWait        time.Duration
	MaxBatch       int
	EnterThreshold float64
	ExitThreshold  float64
}

// TenantFlusherRegistry creates and caches per-tenant AdaptiveFlusher instances.
// Each tenant gets its own rate tracking and batch accumulator, preventing
// cross-tenant interference and ensuring batch payloads carry a single tenant_id.
type TenantFlusherRegistry struct {
	mu       sync.Mutex
	flushers map[string]*AdaptiveFlusher
	db       *sql.DB
	config   FlusherConfig
	encrypt  bool
	enc      *PayloadEncryption
}

// NewTenantFlusherRegistry creates a registry that lazily provisions per-tenant
// AdaptiveFlusher instances with the given configuration.
func NewTenantFlusherRegistry(db *sql.DB, config FlusherConfig) *TenantFlusherRegistry {
	if config.MaxWait <= 0 {
		config.MaxWait = 5 * time.Millisecond
	}
	if config.MaxBatch <= 0 {
		config.MaxBatch = 200
	}
	if config.EnterThreshold <= 0 {
		config.EnterThreshold = 500.0
	}
	if config.ExitThreshold <= 0 {
		config.ExitThreshold = 250.0
	}
	return &TenantFlusherRegistry{
		flushers: make(map[string]*AdaptiveFlusher),
		db:       db,
		config:   config,
	}
}

// SetEncryption propagates encryption settings to the registry and all
// existing per-tenant flusher instances.
func (r *TenantFlusherRegistry) SetEncryption(encrypt bool, enc *PayloadEncryption) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.encrypt = encrypt
	r.enc = enc
	for _, af := range r.flushers {
		af.SetEncryption(encrypt, enc)
	}
}

// For returns the AdaptiveFlusher for the given tenant, creating one if it
// does not already exist. Safe for concurrent use.
func (r *TenantFlusherRegistry) For(tenantID string) *AdaptiveFlusher {
	r.mu.Lock()
	defer r.mu.Unlock()
	if af, ok := r.flushers[tenantID]; ok {
		return af
	}
	af := NewAdaptiveFlusher(r.db, tenantID, r.config.MaxWait, r.config.MaxBatch,
		r.config.EnterThreshold, r.config.ExitThreshold, 0)
	af.SetEncryption(r.encrypt, r.enc)
	r.flushers[tenantID] = af
	return af
}

// Remove cleans up a tenant flusher that is no longer needed.
func (r *TenantFlusherRegistry) Remove(tenantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.flushers, tenantID)
}

// Shutdown drains all per-tenant accumulators. Call before the worker exits.
func (r *TenantFlusherRegistry) Shutdown() {
	r.mu.Lock()
	flushers := make([]*AdaptiveFlusher, 0, len(r.flushers))
	for _, af := range r.flushers {
		flushers = append(flushers, af)
	}
	r.mu.Unlock()
	for _, af := range flushers {
		af.mu.Lock()
		batch := af.events
		af.events = nil
		if af.timer != nil {
			af.timer.Stop()
		}
		af.mu.Unlock()
		if len(batch) > 0 {
			af.flushAndNotify(context.Background(), batch)
		}
	}
}
