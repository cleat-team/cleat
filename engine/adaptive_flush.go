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
	directFlushes atomic.Int64
	batchFlushes  atomic.Int64
	batchedEvents atomic.Int64
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

	entry, err := af.prepareEntry(workflowID, rec, checksum)
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
		go af.flushAndNotify(context.Background(), batch)
	}
}

func (af *AdaptiveFlusher) flushAndNotify(ctx context.Context, batch []batchEntry) {
	events := make([]map[string]interface{}, len(batch))
	for i, entry := range batch {
		p := entry.params
		events[i] = map[string]interface{}{
			"workflow_id":       p[0],
			"step":              p[1],
			"event_type":        p[2],
			"service":           p[3],
			"operation":         p[4],
			"request":           p[5],
			"response":          p[6],
			"error":             p[7],
			"duration_ms":       p[8],
			"signal_names":      p[9],
			"timeout_ms":        p[10],
			"signal_name":       p[11],
			"signal_payload":    p[12],
			"defer_description": p[13],
			"defer_id":          p[14],
			"child_name":        p[15],
			"child_input":       p[16],
			"run_id":            p[17],
			"new_input":         p[18],
			"plugin_name":       p[19],
			"plugin_func":       p[20],
			"plugin_input":      p[21],
			"plugin_output":     p[22],
			"plugin_error":      p[23],
			"promise_name":      p[24],
			"promise_id":        p[25],
			"promise_result":    p[26],
			"promise_error":     p[27],
			"payload":           p[28],
			"checksum":          p[29],
			"tenant_id":         p[30],
			"created_at":        time.Now(),
		}
	}

	eventsJSON, err := json.Marshal(events)
	if err != nil {
		for _, entry := range batch {
			if entry.done != nil {
				entry.done <- err
			}
		}
		return
	}

	_, err = af.db.ExecContext(ctx, "SELECT batch_flush_events($1::jsonb)", string(eventsJSON))
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

	for _, entry := range batch {
		if entry.done != nil {
			close(entry.done)
		}
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
