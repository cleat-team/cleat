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
// In batch mode, a single drainer goroutine quickly assembles batches (with
// a maxWait timer). Completed batches are handed off to a worker pool that
// calls the pg function in parallel — since each workflow has at most one
// outstanding event, there are no ordering constraints between batches.
type AdaptiveFlusher struct {
	mu        sync.Mutex
	batchMode bool

	rateAlpha  float64
	rateEWMA   float64
	lastCount  int64
	lastSample time.Time

	enterThreshold float64
	exitThreshold  float64

	// Batch-mode
	entries    chan batchEntry  // submitted by recordEvent
	batches    chan []batchEntry // drainer → worker pool
	db         *sql.DB
	tenantID   string
	maxWait    time.Duration
	maxBatch   int
	numWorkers int

	encryptSensitivePayloads bool
	encryption               *PayloadEncryption

	// Stats
	directFlushes atomic.Int64
	batchFlushes  atomic.Int64
	batchedEvents atomic.Int64
}

// NewAdaptiveFlusher creates an AdaptiveFlusher. Zero-valued parameters get defaults.
func NewAdaptiveFlusher(db *sql.DB, tenantID string, maxWait time.Duration, maxBatch int, enterThreshold, exitThreshold float64, numWorkers int) *AdaptiveFlusher {
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
	if numWorkers <= 0 {
		numWorkers = 4
	}

	return &AdaptiveFlusher{
		db:             db,
		tenantID:       tenantID,
		rateAlpha:      0.2,
		maxWait:        maxWait,
		maxBatch:       maxBatch,
		numWorkers:     numWorkers,
		entries:        make(chan batchEntry, maxBatch*2),
		batches:        make(chan []batchEntry, numWorkers),
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

// Flush is the main entry point, called from recordEvent.
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

	select {
	case af.entries <- entry:
	case <-ctx.Done():
		done <- ctx.Err()
		return done, true
	}

	return done, true
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

// Run starts the drainer goroutine and the worker pool.
func (af *AdaptiveFlusher) Run(ctx context.Context) {
	// Start worker pool.
	var wg sync.WaitGroup
	for i := 0; i < af.numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range af.batches {
				af.flushBatch(ctx, batch)
			}
		}()
	}

	// Run drainer in the calling goroutine.
	af.drainer(ctx)

	// Drainer exited — close batches channel and wait for workers.
	close(af.batches)
	wg.Wait()
}

// drainer assembles batches from the entry channel and hands them to workers.
func (af *AdaptiveFlusher) drainer(ctx context.Context) {
	for {
		// Wait for first event.
		entry, ok := af.receive(ctx)
		if !ok {
			return
		}
		batch := []batchEntry{entry}

		// Drain with timer — assemble as many events as possible
		// within maxWait, up to maxBatch.
		timer := time.NewTimer(af.maxWait)
		draining := true
		for draining && len(batch) < af.maxBatch {
			select {
			case e, ok := <-af.entries:
				if !ok {
					draining = false
				} else {
					batch = append(batch, e)
				}
			case <-timer.C:
				draining = false
			case <-ctx.Done():
				timer.Stop()
				select {
				case af.batches <- batch:
				case <-ctx.Done():
				}
				return
			}
		}
		timer.Stop()

		// Hand batch to worker pool.
		select {
		case af.batches <- batch:
		case <-ctx.Done():
			af.flushBatch(context.Background(), batch)
			return
		}
	}
}

func (af *AdaptiveFlusher) receive(ctx context.Context) (batchEntry, bool) {
	select {
	case entry, ok := <-af.entries:
		return entry, ok
	case <-ctx.Done():
		return batchEntry{}, false
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

// flushBatch marshals the batch to JSON and calls the pg function.
// Called from worker pool goroutines — multiple batches may be in-flight.
func (af *AdaptiveFlusher) flushBatch(ctx context.Context, batch []batchEntry) {
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

// InBatchMode reports whether currently in batch mode.
func (af *AdaptiveFlusher) InBatchMode() bool {
	af.mu.Lock()
	defer af.mu.Unlock()
	return af.batchMode
}

// GetRate returns the current EWMA of steps/sec.
func (af *AdaptiveFlusher) GetRate() float64 {
	af.mu.Lock()
	defer af.mu.Unlock()
	return af.rateEWMA
}

// Stats returns (directFlushes, batchFlushes, batchedEvents).
func (af *AdaptiveFlusher) Stats() (int64, int64, int64) {
	return af.directFlushes.Load(), af.batchFlushes.Load(), af.batchedEvents.Load()
}
