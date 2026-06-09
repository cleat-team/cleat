package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// writeResult writes a string to WASM linear memory. In the normal (wazero)
// path, it writes through the api.Memory obtained from m. In the wasmtime
// path, it uses a raw byte buffer stored in the context via
// contextWithRawMemBuf, in which case m can be nil.
func (s *execSession) writeResult(ctx context.Context, m api.Module, ptr uint32, val string, maxLen uint32) (uint32, error) {
	if rawBuf, ok := ctx.Value(wasmMemBufKey{}).([]byte); ok && rawBuf != nil {
		data := []byte(val)
		if uint32(len(data)) > maxLen {
			data = data[:maxLen]
		}
		n := copy(rawBuf[ptr:], data)
		return uint32(n), nil
	}
	if m != nil {
		if mem := m.Memory(); mem != nil {
			return writeWasmString(mem, ptr, val, maxLen)
		}
	}
	return 0, nil
}

// flushEvent writes a single event to event_history in its own transaction.
// This guarantees exactly-once: if the worker crashes before the workflow
// completes, replay will find this event and return the cached response.
// Retries with [100ms, 200ms, 400ms] backoff on transient failures.
//
// NOTE: flushEvent, flushCallIntent, and completeCallEvent use Postgres-specific
// SQL syntax ($N placeholders, ON CONFLICT). This is a known portability
// constraint -- MySQL and MSSQL workers use the batch path (appendEventsInTx
// via FinalizeWorkflowSegment) which is fully dialect-abstracted.
func (e *Engine) flushEvent(ctx context.Context, workflowID string, rec EventRecord) error {
	if e.db == nil {
		return nil
	}

	var lastErr error
	backoff := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 && attempt-1 < len(backoff) {
			select {
			case <-ctx.Done():
				return fmt.Errorf("flush event: context cancelled after %d retries: %w", attempt, ctx.Err())
			case <-time.After(backoff[attempt-1]):
			}
		}

		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			lastErr = fmt.Errorf("flush event: begin tx: %w", err)
			if attempt < len(backoff) {
				continue
			}
			break
		}

		var prevChecksum string
		if rec.Step > 1 {
			tx.QueryRowContext(ctx, `SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
				workflowID, rec.Step-1).Scan(&prevChecksum)
		}
		checksum := computeEventChecksum(rec, prevChecksum)
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
		// On any encryption failure, abort the flush (fail-secure). Silently
		// storing plaintext when encryption is enabled would be a data leak.
		if e.encryptSensitivePayloads && e.encryption != nil {
			var encErr error

			if requestStr, encErr = e.encryption.EncryptString(rec.Request); encErr != nil {
				e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "request", "error", encErr)
				encryptionErrorsTotal.Inc()
				tx.Rollback()
				lastErr = fmt.Errorf("flush event: encrypt request: %w", encErr)
				if attempt < len(backoff) {
					continue
				}
				break
			}
			if responseStr, encErr = e.encryption.EncryptString(rec.Response); encErr != nil {
				e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "response", "error", encErr)
				encryptionErrorsTotal.Inc()
				tx.Rollback()
				lastErr = fmt.Errorf("flush event: encrypt response: %w", encErr)
				if attempt < len(backoff) {
					continue
				}
				break
			}
			if errStr, encErr = e.encryption.EncryptString(rec.Err); encErr != nil {
				e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "err", "error", encErr)
				encryptionErrorsTotal.Inc()
				tx.Rollback()
				lastErr = fmt.Errorf("flush event: encrypt err: %w", encErr)
				if attempt < len(backoff) {
					continue
				}
				break
			}
			if rec.SignalPayload != "" {
				if sigPayload, encErr = e.encryption.EncryptString(rec.SignalPayload); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "signal_payload", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt signal_payload: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.ChildInput != "" {
				if childInput, encErr = e.encryption.EncryptString(rec.ChildInput); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "child_input", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt child_input: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.NewInput != "" {
				if newInput, encErr = e.encryption.EncryptString(rec.NewInput); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "new_input", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt new_input: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.PluginInput != "" {
				if pluginInput, encErr = e.encryption.EncryptString(rec.PluginInput); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "plugin_input", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt plugin_input: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.PluginOutput != "" {
				if pluginOutput, encErr = e.encryption.EncryptString(rec.PluginOutput); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "plugin_output", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt plugin_output: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.PromiseResult != "" {
				if promiseResult, encErr = e.encryption.EncryptString(rec.PromiseResult); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "promise_result", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt promise_result: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.PromiseError != "" {
				if promiseError, encErr = e.encryption.EncryptString(rec.PromiseError); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "promise_error", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt promise_error: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			// Encrypt payload JSON when present.
			if len(payloadJSON) > 0 {
				var encrypted []byte
				if encrypted, encErr = e.encryption.EncryptJSON(payloadJSON); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "payload", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt payload: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
				payloadArg = sql.NullString{String: string(encrypted), Valid: true}
			}
		}

		// Quota check: read current count without incrementing.
		// Increment happens in appendEventsInTx (via FinalizeWorkflowSegment)
		// to avoid double-counting with flushEvent's own increment.
		// Note: event_count is read before appendEventsInTx increments it.
		// Within a single execution segment, multiple flushEvent calls see the
		// same count, allowing the quota to be exceeded by the segment size.
		// This is intentional: the quota is a soft backstop, not an exact cap.
		// The atomic increment in appendEventsInTx determines the final count.
		if e.maxQuotaEvents > 0 && e.workflowStore != nil {
			var currentCount int
			qErr := tx.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = $1`, workflowID).Scan(&currentCount)
			if qErr != nil {
				tx.Rollback()
				lastErr = fmt.Errorf("flush event: quota check: %w", qErr)
				if attempt < len(backoff) {
					continue
				}
				break
			}
			if currentCount >= e.maxQuotaEvents {
				tx.Rollback()
				return fmt.Errorf("flush event: event quota exceeded (max %d)", e.maxQuotaEvents)
			}
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
				duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
				defer_description, defer_id, child_name, child_input, run_id, new_input,
				plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
				promise_name, promise_id, promise_result, promise_error, payload,
				checksum, created_at, tenant_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
				$9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
				$20, $21, $22, $23, $24, $25, $26, $27, $28, $29,
				$30, NOW(), $31)
			ON CONFLICT (workflow_id, step) DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error WHERE event_history.response = '' AND event_history.error IS NULL
		`, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(requestStr), nullStr(responseStr), nullStr(errStr),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(sigPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(childInput), nullStr(rec.RunID), nullStr(newInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(pluginInput), nullStr(pluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(promiseResult), nullStr(promiseError),
			payloadArg,
			checksum, e.tenantID)
		if err != nil {
			tx.Rollback()
			lastErr = fmt.Errorf("flush event: exec: %w", err)
			if attempt < len(backoff) {
				continue
			}
			break
		}

		if err := tx.Commit(); err != nil {
			tx.Rollback()
			lastErr = fmt.Errorf("flush event: commit: %w", err)
			if attempt < len(backoff) {
				continue
			}
			break
		}

		return nil
	}

	// All retries exhausted --- log a structured error that can be alerted on.
	e.log().ErrorContext(ctx, "flushEvent retries exhausted", "workflow_id", workflowID, "tenant_id", e.tenantID, "step", rec.Step, "event_type", rec.EventType, "error", lastErr)
	return lastErr

}

// flushCallIntent inserts a pending event BEFORE the external call is
// dispatched.  This provides a durable record of intent: if the worker
// crashes after the external call succeeds but before the response is
// persisted, replay will find the pending event and return ErrAmbiguous.
func (e *Engine) flushCallIntent(ctx context.Context, workflowID string, rec EventRecord) error {
	if e.db == nil {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("flush call intent: begin tx: %w", err)
	}
	defer tx.Rollback()

	var prevChecksum string
	if rec.Step > 1 {
		tx.QueryRowContext(ctx, `SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
			workflowID, rec.Step-1).Scan(&prevChecksum)
	}
	checksum := computeEventChecksum(rec, prevChecksum)
	_, err = tx.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error, checksum)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (workflow_id, step) DO NOTHING
		`, workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(rec.Request), nullStr(rec.Response), pendingSentinel,
		checksum)
	if err != nil {
		return fmt.Errorf("flush call intent: exec: %w", err)
	}
	return tx.Commit()
}

// completeCallEvent updates a previously-flushed pending event with the
// actual call response (or error).  This transitions the event from the
// pending state to the completed state so that replay returns the cached
// response rather than ErrAmbiguous.  The checksum is recomputed from the
// full event record (which the caller stashes in rec) so that integrity
// verification remains consistent.
func (e *Engine) completeCallEvent(ctx context.Context, workflowID string, rec EventRecord, callErr string) error {
	if e.db == nil {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("complete call event: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Recompute the checksum with the actual response.
	completed := rec
	completed.Err = callErr
	var prevChecksum string
	if completed.Step > 1 {
		tx.QueryRowContext(ctx, `SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
			workflowID, completed.Step-1).Scan(&prevChecksum)
	}
	checksum := computeEventChecksum(completed, prevChecksum)

	responseStr := nullStr(rec.Response)
	errorStr := nullStr(callErr)
	if e.encryptSensitivePayloads && e.encryption != nil {
		s, err := e.encryption.EncryptString(rec.Response)
		if err != nil {
			e.log().ErrorContext(ctx, "encryption failed for response", "workflow_id", workflowID, "tenant_id", e.tenantID, "step", rec.Step, "error", err)
			encryptionErrorsTotal.Inc()
			tx.Rollback()
			return fmt.Errorf("complete call event: encrypt response: %w", err)
		}
		responseStr = nullStr(s)
		s, err = e.encryption.EncryptString(callErr)
		if err != nil {
			e.log().ErrorContext(ctx, "encryption failed for error", "workflow_id", workflowID, "tenant_id", e.tenantID, "step", rec.Step, "error", err)
			encryptionErrorsTotal.Inc()
			tx.Rollback()
			return fmt.Errorf("complete call event: encrypt error: %w", err)
		}
		errorStr = nullStr(s)
	}

	result, err := tx.ExecContext(ctx, `
			UPDATE event_history
			SET response = $1, error = $2, checksum = $6
			WHERE workflow_id = $3 AND step = $4 AND error = $5
		`, responseStr, errorStr, workflowID, rec.Step, pendingSentinel, checksum)
	if err != nil {
		return fmt.Errorf("complete call event: exec: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete call event: rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("completeCallEvent: no rows updated for workflow %s step %d — the event may have been completed by another worker", workflowID, rec.Step)
	}
	return tx.Commit()
}

// runDefers invokes registered defer functions on a fresh module instance.
// Called on non-suspend errors to ensure cleanup runs even when the workflow fails.
func (e *Engine) runDefers(ctx context.Context, wasmBytes []byte, deferrals map[string]string) {
	type defEntry struct {
		id     string
		desc   string
		stepNo int // parsed from defer ID "defer-N"
	}
	var entries []defEntry
	for id, desc := range deferrals {
		stepNo := parseDeferStepNo(id)
		entries = append(entries, defEntry{id: id, desc: desc, stepNo: stepNo})
	}
	// Sort defers in LIFO order (higher stepNo first) so the most recently
	// registered defer runs first. Uses sort.Slice for clarity.
	sort.Slice(entries, func(i, j int) bool {
		return parseDeferStepNo(entries[i].id) < parseDeferStepNo(entries[j].id)
	})
	for _, entry := range entries {
		// Use the same naming convention as invokeDefersOnTrap:
		// "cleat_defer_" + deferID so both paths find the same export.
		deferName := "cleat_defer_" + entry.id
		if wasmBytes != nil {
			_, err := e.RunDefer(ctx, wasmBytes, deferName, nil)
			if err != nil {
				// Defer failures are not propagated — cleanup runs best-effort.
			}
		}
	}
}
