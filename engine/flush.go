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

// insertEventSQL is the shared INSERT statement for both fast and quota paths.
const insertEventSQL = `
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
	ON CONFLICT (workflow_id, step) DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error WHERE event_history.response = '' AND event_history.error IS NULL`

// flushEvent persists a single event to event_history. Each step is one INSERT
// that auto-commits; no explicit transaction is needed. The checksum chain is
// tracked in-memory (execSession.lastChecksum) to avoid a DB round-trip.
func (e *Engine) flushEvent(ctx context.Context, workflowID string, rec EventRecord, prevChecksum string) error {
	if e.db == nil || e.noPerStepFlush {
		return nil
	}

	flushStart := time.Now()
	defer func() {
		if DebugTiming {
			e.log().InfoContext(ctx, "TIMING: flushEvent db tx", "workflow_id", workflowID, "step", rec.Step, "total_ms", time.Since(flushStart).Milliseconds())
		}
	}()

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
	if e.encryptSensitivePayloads && e.encryption != nil {
		var encErr error
		if requestStr, encErr = e.encryption.EncryptString(rec.Request); encErr != nil {
			return fmt.Errorf("flush event: encrypt request: %w", encErr)
		}
		if responseStr, encErr = e.encryption.EncryptString(rec.Response); encErr != nil {
			return fmt.Errorf("flush event: encrypt response: %w", encErr)
		}
		if errStr, encErr = e.encryption.EncryptString(rec.Err); encErr != nil {
			return fmt.Errorf("flush event: encrypt err: %w", encErr)
		}
		if rec.SignalPayload != "" {
			if sigPayload, encErr = e.encryption.EncryptString(rec.SignalPayload); encErr != nil {
				return fmt.Errorf("flush event: encrypt signal_payload: %w", encErr)
			}
		}
		if rec.ChildInput != "" {
			if childInput, encErr = e.encryption.EncryptString(rec.ChildInput); encErr != nil {
				return fmt.Errorf("flush event: encrypt child_input: %w", encErr)
			}
		}
		if rec.NewInput != "" {
			if newInput, encErr = e.encryption.EncryptString(rec.NewInput); encErr != nil {
				return fmt.Errorf("flush event: encrypt new_input: %w", encErr)
			}
		}
		if rec.PluginInput != "" {
			if pluginInput, encErr = e.encryption.EncryptString(rec.PluginInput); encErr != nil {
				return fmt.Errorf("flush event: encrypt plugin_input: %w", encErr)
			}
		}
		if rec.PluginOutput != "" {
			if pluginOutput, encErr = e.encryption.EncryptString(rec.PluginOutput); encErr != nil {
				return fmt.Errorf("flush event: encrypt plugin_output: %w", encErr)
			}
		}
		if rec.PromiseResult != "" {
			if promiseResult, encErr = e.encryption.EncryptString(rec.PromiseResult); encErr != nil {
				return fmt.Errorf("flush event: encrypt promise_result: %w", encErr)
			}
		}
		if rec.PromiseError != "" {
			if promiseError, encErr = e.encryption.EncryptString(rec.PromiseError); encErr != nil {
				return fmt.Errorf("flush event: encrypt promise_error: %w", encErr)
			}
		}
		if len(payloadJSON) > 0 && e.encryption != nil {
			encrypted, encErr := e.encryption.EncryptJSON(payloadJSON)
			if encErr != nil {
				return fmt.Errorf("flush event: encrypt payload: %w", encErr)
			}
			payloadArg = sql.NullString{String: string(encrypted), Valid: true}
		}
	}

	// Quota check path: needs explicit transaction for atomic read-then-insert.
	if e.maxQuotaEvents > 0 && e.workflowStore != nil {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("flush event (quota): begin tx: %w", err)
		}
		defer tx.Rollback()
		var currentCount int
		if err := tx.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = $1`, workflowID).Scan(&currentCount); err != nil {
			return fmt.Errorf("flush event (quota): %w", err)
		}
		if currentCount >= e.maxQuotaEvents {
			return fmt.Errorf("flush event: event quota exceeded (max %d)", e.maxQuotaEvents)
		}
		_, err = tx.ExecContext(ctx, insertEventSQL, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(requestStr), nullStr(responseStr), nullStr(errStr),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(sigPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(childInput), nullStr(rec.RunID), nullStr(newInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(pluginInput), nullStr(pluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(promiseResult), nullStr(promiseError),
			payloadArg, checksum, e.tenantID)
		if err != nil {
			return fmt.Errorf("flush event (quota): %w", err)
		}
		return tx.Commit()
	}

	// Fast path: single INSERT auto-commits. No explicit BEGIN/COMMIT needed.
	_, err := e.db.ExecContext(ctx, insertEventSQL, workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(requestStr), nullStr(responseStr), nullStr(errStr),
		nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
		nullStr(rec.SignalName), nullStr(sigPayload),
		nullStr(rec.DeferDescription), nullStr(rec.DeferID),
		nullStr(rec.ChildName), nullStr(childInput), nullStr(rec.RunID), nullStr(newInput),
		nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(pluginInput), nullStr(pluginOutput), nullStr(rec.PluginError),
		nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(promiseResult), nullStr(promiseError),
		payloadArg, checksum, e.tenantID)
	if err != nil {
		return fmt.Errorf("flush event: %w", err)
	}
	return nil
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
