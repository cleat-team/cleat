package engine

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// BatchFlusher batches per-step event INSERTs into single database
// transactions.  Events are submitted via Submit; a single goroutine running
// Run collects them into batches and flushes each batch on timer expiry or
// when the batch reaches the configured maximum size.
//
// Fields that must be encrypted or base64-encoded are pre-computed by
// PrepareEntry before submission, so the batch-flusher goroutine does minimal
// work per entry.
type BatchFlusher struct {
	db                       *sql.DB
	ch                       chan batchEntry
	tenantID                 string
	maxWait                  time.Duration
	maxBatch                 int
	encryptSensitivePayloads bool
	encryption               *PayloadEncryption
}

// NewBatchFlusher creates a BatchFlusher.  The internal channel capacity is
// 2 * maxBatch to absorb brief bursts without blocking callers.
func NewBatchFlusher(db *sql.DB, tenantID string, maxWait time.Duration, maxBatch int, encrypt bool, enc *PayloadEncryption) *BatchFlusher {
	return &BatchFlusher{
		db:                       db,
		ch:                       make(chan batchEntry, 2*maxBatch),
		tenantID:                 tenantID,
		maxWait:                  maxWait,
		maxBatch:                 maxBatch,
		encryptSensitivePayloads: encrypt,
		encryption:               enc,
	}
}

// PrepareEntry pre-computes all INSERT parameters for one event — exactly
// the same computation that flushEvent would do — and returns a batchEntry
// ready for Submit.  The caller must set the entry's done channel before
// submitting.
//
// Encryption, base64 encoding, and null-value wrapping are all done here so
// that the single batch-flusher goroutine does minimal work per entry.
func (bf *BatchFlusher) PrepareEntry(workflowID string, rec EventRecord, checksum string) batchEntry {
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

	// Encrypt sensitive payload fields when encryption is enabled.
	if bf.encryptSensitivePayloads && bf.encryption != nil {
		if s, err := bf.encryption.EncryptString(rec.Request); err == nil {
			requestStr = s
		}
		if s, err := bf.encryption.EncryptString(rec.Response); err == nil {
			responseStr = s
		}
		if s, err := bf.encryption.EncryptString(rec.Err); err == nil {
			errStr = s
		}
		if rec.SignalPayload != "" {
			if s, err := bf.encryption.EncryptString(rec.SignalPayload); err == nil {
				sigPayload = s
			}
		}
		if rec.ChildInput != "" {
			if s, err := bf.encryption.EncryptString(rec.ChildInput); err == nil {
				childInput = s
			}
		}
		if rec.NewInput != "" {
			if s, err := bf.encryption.EncryptString(rec.NewInput); err == nil {
				newInput = s
			}
		}
		if rec.PluginInput != "" {
			if s, err := bf.encryption.EncryptString(rec.PluginInput); err == nil {
				pluginInput = s
			}
		}
		if rec.PluginOutput != "" {
			if s, err := bf.encryption.EncryptString(rec.PluginOutput); err == nil {
				pluginOutput = s
			}
		}
		if rec.PromiseResult != "" {
			if s, err := bf.encryption.EncryptString(rec.PromiseResult); err == nil {
				promiseResult = s
			}
		}
		if rec.PromiseError != "" {
			if s, err := bf.encryption.EncryptString(rec.PromiseError); err == nil {
				promiseError = s
			}
		}
		if len(payloadJSON) > 0 {
			if encrypted, err := bf.encryption.EncryptJSON(payloadJSON); err == nil {
				payloadArg = sql.NullString{String: string(encrypted), Valid: true}
			}
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
		payloadArg, checksum, bf.tenantID,
	}

	return batchEntry{
		workflowID: workflowID,
		step:       rec.Step,
		done:       nil, // caller must set before Submit
		params:     params,
	}
}

// Submit places an entry on the batch channel.  It blocks when the channel is
// full, providing natural backpressure against fast producers.  Returns
// ctx.Err() when the context is cancelled before the entry is accepted.
func (bf *BatchFlusher) Submit(ctx context.Context, entry batchEntry) error {
	select {
	case bf.ch <- entry:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run is the main goroutine loop.  It collects submitted entries into batches
// and flushes each batch as a single database transaction.  The first entry in
// a batch starts a timer; the batch is flushed when the timer fires or when
// the batch reaches maxBatch, whichever happens first.  Run blocks until ctx
// is cancelled, at which point any remaining entries are flushed before
// returning.
//
// Use as: go flusher.Run(ctx)
func (bf *BatchFlusher) Run(ctx context.Context) {
	var batch []batchEntry
	var timer *time.Timer
	var timerCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			if len(batch) > 0 {
				bf.flush(ctx, batch)
			}
			return

		case entry, ok := <-bf.ch:
			if !ok {
				// Channel closed by owner — flush remaining and exit.
				if len(batch) > 0 {
					bf.flush(ctx, batch)
				}
				return
			}

			batch = append(batch, entry)

			// Start the max-wait timer when the first entry arrives.
			if len(batch) == 1 {
				timer = time.NewTimer(bf.maxWait)
				timerCh = timer.C
			}

			// Flush immediately when the batch reaches the size limit.
			if len(batch) >= bf.maxBatch {
				bf.flush(ctx, batch)
				batch = batch[:0]
				if timer != nil {
					timer.Stop()
					timer = nil
					timerCh = nil
				}
			}

		case <-timerCh:
			// Max wait reached — flush whatever has accumulated.
			if len(batch) > 0 {
				bf.flush(ctx, batch)
				batch = batch[:0]
			}
			timer = nil
			timerCh = nil
		}
	}
}

// flush sends a batch of entries as a single database transaction using the
// shared insertEventSQL statement.
//
// On success each entry's done channel is closed, signalling a nil error to
// the receiver.  On failure an error is sent to every entry's done channel
// because the entire transaction is rolled back.
func (bf *BatchFlusher) flush(ctx context.Context, batch []batchEntry) {
	tx, err := bf.db.BeginTx(ctx, nil)
	if err != nil {
		err = fmt.Errorf("batch flush: begin tx: %w", err)
		for i := range batch {
			if batch[i].done != nil {
				batch[i].done <- err
			}
		}
		return
	}
	defer tx.Rollback()

	// One tenant context for the whole batch. Same defect as the single-event
	// path (see setRLSOnFlushTx): without it every insert here is rejected by
	// event_history's RLS policy on the connection the worker actually uses,
	// and the rejection surfaces only as a log line. Cheap here -- one extra
	// statement amortised across up to maxBatch events.
	if bf.tenantID != "" {
		if err := setRLSOnFlushTx(ctx, tx, bf.tenantID); err != nil {
			err = fmt.Errorf("batch flush: set tenant context: %w", err)
			for i := range batch {
				if batch[i].done != nil {
					batch[i].done <- err
				}
			}
			return
		}
	}

	for i := range batch {
		entry := &batch[i]
		if _, execErr := tx.ExecContext(ctx, insertEventSQL, entry.params...); execErr != nil {
			execErr = fmt.Errorf("batch flush: insert event (workflow=%s step=%d): %w",
				entry.workflowID, entry.step, execErr)
			// The transaction is now aborted.  Notify every caller in the
			// batch so they can retry.
			for j := range batch {
				if batch[j].done != nil {
					batch[j].done <- execErr
				}
			}
			return
		}
	}

	if err := tx.Commit(); err != nil {
		err = fmt.Errorf("batch flush: commit tx: %w", err)
		for i := range batch {
			if batch[i].done != nil {
				batch[i].done <- err
			}
		}
		return
	}

	// All inserts committed — signal success by closing each done channel.
	for i := range batch {
		if batch[i].done != nil {
			close(batch[i].done)
		}
	}
}
