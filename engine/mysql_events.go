package engine

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// AppendEventHistory / AppendEventHistoryBatch
// ---------------------------------------------------------------------------

// AppendEventHistory appends a single event to the history.
func (s *MySQLStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	return s.AppendEventHistoryBatch(ctx, workflowID, []EventRecord{rec})
}

// AppendEventHistoryBatch appends multiple events atomically.
func (s *MySQLStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append history batch: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.appendEventsInTx(ctx, tx, workflowID, recs); err != nil {
		return err
	}
	return tx.Commit()
}

// appendEventsInTx inserts event records inside an already-open transaction.
// This is shared by AppendEventHistoryBatch and FinalizeWorkflowSegment so
// that both can insert events atomically alongside other operations.
func (s *MySQLStore) appendEventsInTx(ctx context.Context, tx *sql.Tx, workflowID string, recs []EventRecord) error {
	if len(recs) == 0 {
		return nil
	}

	// MySQL uses INSERT IGNORE instead of ON CONFLICT DO NOTHING.
	// The unique key on (workflow_id, step) prevents duplicates.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT IGNORE INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
			duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
			defer_description, defer_id, child_name, child_input, run_id, new_input,
			plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
			promise_name, promise_id, promise_result, promise_error, payload,
			created_at, checksum, tenant_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("append events in tx: prepare: %w", err)
	}
	defer stmt.Close()

	// Chain in step order, seeded from what is already stored -- see
	// chainOrder and PostgresStore.previousStoredChecksum for why both halves
	// are required.
	order := chainOrder(recs)
	prevChecksum, err := s.previousStoredChecksum(ctx, tx, workflowID, recs[order[0]].Step)
	if err != nil {
		return err
	}

	for _, i := range order {
		rec := recs[i]
		payload, err := eventRecordToPayload(rec)
		payloadArg := nullStr("")
		if err == nil && len(payload) > 0 {
			payloadArg = sql.NullString{String: string(payload), Valid: true}
		}
		checksum := computeEventChecksum(rec, prevChecksum)
		prevChecksum = checksum
		_, err = stmt.ExecContext(ctx, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op),
			nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Request))),
			nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Response))),
			nullStr(rec.Err),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(rec.SignalPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(rec.PluginInput), nullStr(rec.PluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(rec.PromiseResult), nullStr(rec.PromiseError),
			payloadArg,
			time.UnixMilli(rec.TimestampMs),
			checksum,
			s.tenantID)
		if err != nil {
			return fmt.Errorf("append events in tx: exec step %d: %w", rec.Step, err)
		}
	}
	// Increment event_count on workflow_instances so GetEventCount and quota
	// enforcement have an up-to-date count.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances SET event_count = event_count + ? WHERE id = ? AND tenant_id = ?
	`, len(recs), workflowID, s.tenantID); err != nil {
		return fmt.Errorf("append events in tx: increment event_count: %w", err)
	}

	return nil
}

// previousStoredChecksum returns the checksum of the last event already stored
// for workflowID before step, or "" when there is none. See
// PostgresStore.previousStoredChecksum for the reasoning.
func (s *MySQLStore) previousStoredChecksum(ctx context.Context, tx *sql.Tx, workflowID string, step int) (string, error) {
	var checksum sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT checksum FROM event_history
		WHERE workflow_id = ? AND tenant_id = ? AND step < ?
		ORDER BY step DESC LIMIT 1
	`, workflowID, s.tenantID, step).Scan(&checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("append events in tx: previous checksum: %w", err)
	}
	return checksum.String, nil
}

// ---------------------------------------------------------------------------
// LoadEventHistory
// ---------------------------------------------------------------------------

// LoadEventHistory returns the full event history for a workflow, ordered by step.
func (s *MySQLStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error,
		       CAST(UNIX_TIMESTAMP(created_at) * 1000 AS UNSIGNED) AS timestamp_ms
		FROM event_history
		WHERE workflow_id = ? AND tenant_id = ?
		ORDER BY step
	`, workflowID, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	defer rows.Close()

	var history []EventRecord
	for rows.Next() {
		var rec EventRecord
		var service, op, request, response, errMsg sql.NullString
		var durationMs, timeoutMs sql.NullInt64
		var signalNames, signalName, signalPayload sql.NullString
		var deferDesc, deferID sql.NullString
		var childName, childInput, runID, newInput sql.NullString
		var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
		var payload sql.NullString
		var promiseName, promiseID, promiseResult, promiseError sql.NullString

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError,
			&rec.TimestampMs); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = tryDecodeBase64(request.String)
		rec.Response = tryDecodeBase64(response.String)
		rec.Err = errMsg.String
		rec.DurationMs = durationMs.Int64
		rec.SignalNames = signalNames.String
		rec.TimeoutMs = timeoutMs.Int64
		rec.SignalName = signalName.String
		rec.SignalPayload = signalPayload.String
		rec.DeferDescription = deferDesc.String
		rec.DeferID = deferID.String
		rec.ChildName = childName.String
		rec.ChildInput = childInput.String
		rec.RunID = runID.String
		rec.NewInput = newInput.String
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginInput = pluginInput.String
		rec.PluginOutput = pluginOutput.String
		rec.PluginError = pluginErr.String
		rec.PromiseName = promiseName.String
		rec.PromiseID = promiseID.String
		rec.PromiseResult = promiseResult.String
		rec.PromiseError = promiseError.String

		// Retroactive redaction on read path.
		if !s.disableReadRedaction {
			rec.Request = RedactOnRead(rec.Request)
			rec.Response = RedactOnRead(rec.Response)
			rec.Err = RedactOnRead(rec.Err)
			rec.SignalPayload = RedactOnRead(rec.SignalPayload)
			rec.ChildInput = RedactOnRead(rec.ChildInput)
			rec.NewInput = RedactOnRead(rec.NewInput)
			rec.PluginInput = RedactOnRead(rec.PluginInput)
			rec.PluginOutput = RedactOnRead(rec.PluginOutput)
			rec.PromiseResult = RedactOnRead(rec.PromiseResult)
			rec.PromiseError = RedactOnRead(rec.PromiseError)
		}

		if payload.Valid {
			populateFromPayload(&rec, []byte(payload.String))
		}

		history = append(history, rec)
	}
	return history, rows.Err()
}

// LoadEventHistoryPaginated returns a page of event history for a workflow,
// with offset and limit support. Defaults limit to 1000 if limit <= 0, capped at 1000.
func (s *MySQLStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error
		FROM event_history
		WHERE workflow_id = ? AND tenant_id = ?
		ORDER BY step
		LIMIT ? OFFSET ?
	`, workflowID, s.tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load history paginated: %w", err)
	}
	defer rows.Close()

	var history []EventRecord
	for rows.Next() {
		var rec EventRecord
		var service, op, request, response, errMsg sql.NullString
		var durationMs, timeoutMs sql.NullInt64
		var signalNames, signalName, signalPayload sql.NullString
		var deferDesc, deferID sql.NullString
		var childName, childInput, runID, newInput sql.NullString
		var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
		var payload sql.NullString
		var promiseName, promiseID, promiseResult, promiseError sql.NullString

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError); err != nil {
			return nil, fmt.Errorf("scan history paginated: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = tryDecodeBase64(request.String)
		rec.Response = tryDecodeBase64(response.String)
		rec.Err = errMsg.String
		rec.DurationMs = durationMs.Int64
		rec.SignalNames = signalNames.String
		rec.TimeoutMs = timeoutMs.Int64
		rec.SignalName = signalName.String
		rec.SignalPayload = signalPayload.String
		rec.DeferDescription = deferDesc.String
		rec.DeferID = deferID.String
		rec.ChildName = childName.String
		rec.ChildInput = childInput.String
		rec.RunID = runID.String
		rec.NewInput = newInput.String
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginInput = pluginInput.String
		rec.PluginOutput = pluginOutput.String
		rec.PluginError = pluginErr.String
		rec.PromiseName = promiseName.String
		rec.PromiseID = promiseID.String
		rec.PromiseResult = promiseResult.String
		rec.PromiseError = promiseError.String

		// Retroactive redaction on read path.
		if !s.disableReadRedaction {
			rec.Request = RedactOnRead(rec.Request)
			rec.Response = RedactOnRead(rec.Response)
			rec.Err = RedactOnRead(rec.Err)
			rec.SignalPayload = RedactOnRead(rec.SignalPayload)
			rec.ChildInput = RedactOnRead(rec.ChildInput)
			rec.NewInput = RedactOnRead(rec.NewInput)
			rec.PluginInput = RedactOnRead(rec.PluginInput)
			rec.PluginOutput = RedactOnRead(rec.PluginOutput)
			rec.PromiseResult = RedactOnRead(rec.PromiseResult)
			rec.PromiseError = RedactOnRead(rec.PromiseError)
		}

		if payload.Valid {
			populateFromPayload(&rec, []byte(payload.String))
		}

		history = append(history, rec)
	}
	return history, rows.Err()
}

// StreamEventHistory loads event history for a workflow in pages, returning
// events through a channel. Events are fetched in pages of pageSize as the
// caller reads from the channel. The channel is closed when all events have
// been sent.
func (s *MySQLStore) StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan EventRecord, <-chan error) {
	eventCh := make(chan EventRecord, pageSize)
	errCh := make(chan error, 1)

	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 1000
	}

	go func() {
		defer close(eventCh)
		defer close(errCh)

		offset := 0
		for {
			if ctx.Err() != nil {
				errCh <- ctx.Err()
				return
			}

			rows, err := s.db.QueryContext(ctx, `
				SELECT step, event_type, service, operation, request, response, error,
				       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
				       defer_description, defer_id, child_name, child_input, run_id, new_input,
				       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
				       payload,
				       promise_name, promise_id, promise_result, promise_error,
				       CAST(UNIX_TIMESTAMP(created_at) * 1000 AS UNSIGNED) AS timestamp_ms
				FROM event_history
				WHERE workflow_id = ? AND tenant_id = ?
				ORDER BY step
				LIMIT ? OFFSET ?
			`, workflowID, s.tenantID, pageSize, offset)
			if err != nil {
				errCh <- err
				return
			}

			var pageCount int
			for rows.Next() {
				pageCount++
				var rec EventRecord
				var service, op, request, response, errMsg sql.NullString
				var durationMs, timeoutMs sql.NullInt64
				var signalNames, signalName, signalPayload sql.NullString
				var deferDesc, deferID sql.NullString
				var childName, childInput, runID, newInput sql.NullString
				var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
				var payload sql.NullString
				var promiseName, promiseID, promiseResult, promiseError sql.NullString

				if err := rows.Scan(&rec.Step, &rec.EventType,
					&service, &op, &request, &response, &errMsg,
					&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
					&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
					&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
					&payload,
					&promiseName, &promiseID, &promiseResult, &promiseError,
					&rec.TimestampMs); err != nil {
					rows.Close()
					errCh <- err
					return
				}

				rec.Service = service.String
				rec.Op = op.String
				rec.Request = tryDecodeBase64(request.String)
				rec.Response = tryDecodeBase64(response.String)
				rec.Err = errMsg.String
				rec.DurationMs = durationMs.Int64
				rec.SignalNames = signalNames.String
				rec.TimeoutMs = timeoutMs.Int64
				rec.SignalName = signalName.String
				rec.SignalPayload = signalPayload.String
				rec.DeferDescription = deferDesc.String
				rec.DeferID = deferID.String
				rec.ChildName = childName.String
				rec.ChildInput = childInput.String
				rec.RunID = runID.String
				rec.NewInput = newInput.String
				rec.PluginName = pluginName.String
				rec.PluginFunc = pluginFunc.String
				rec.PluginInput = pluginInput.String
				rec.PluginOutput = pluginOutput.String
				rec.PluginError = pluginErr.String
				rec.PromiseName = promiseName.String
				rec.PromiseID = promiseID.String
				rec.PromiseResult = promiseResult.String
				rec.PromiseError = promiseError.String

				// Decryption must happen BEFORE redaction: redacting ciphertext is meaningless.
				// The fields below are encrypted by flushEvent when encryption is enabled.
				// NOTE: On MySQL this block is a forward-compatibility guard only --
				// encryption is not yet supported and will never be true.
				if s.encryption != nil && s.encryptSensitivePayloads {
					if decrypted, err := s.encryption.Decrypt([]byte(rec.Request)); err == nil {
						rec.Request = string(decrypted)
					}
					if decrypted, err := s.encryption.Decrypt([]byte(rec.Response)); err == nil {
						rec.Response = string(decrypted)
					}
					if decrypted, err := s.encryption.DecryptString(rec.Err); err == nil {
						rec.Err = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.SignalPayload); err == nil {
						rec.SignalPayload = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.ChildInput); err == nil {
						rec.ChildInput = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.NewInput); err == nil {
						rec.NewInput = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.PluginInput); err == nil {
						rec.PluginInput = decrypted
					}
					if decrypted, err := s.encryption.DecryptString(rec.PluginOutput); err == nil {
						rec.PluginOutput = decrypted
					}
				}

				// Retroactive redaction on read path: ensure sensitive fields are
				// redacted even if they were stored before redaction was mandatory.
				// Redaction runs AFTER decryption (see block above) since redacting
				// ciphertext would yield meaningless "[REDACTED]" placeholders.
				if !s.disableReadRedaction {
					rec.Request = RedactOnRead(rec.Request)
					rec.Response = RedactOnRead(rec.Response)
					rec.Err = RedactOnRead(rec.Err)
					rec.SignalPayload = RedactOnRead(rec.SignalPayload)
					rec.ChildInput = RedactOnRead(rec.ChildInput)
					rec.NewInput = RedactOnRead(rec.NewInput)
					rec.PluginInput = RedactOnRead(rec.PluginInput)
					rec.PluginOutput = RedactOnRead(rec.PluginOutput)
					rec.PromiseResult = RedactOnRead(rec.PromiseResult)
					rec.PromiseError = RedactOnRead(rec.PromiseError)
				}

				if payload.Valid {
					payloadStr := payload.String
					// Decrypt payload before populateFromPayload if encryption is enabled.
					// NOTE: Forward-compatibility guard only on MySQL.
					if s.encryption != nil && s.encryptSensitivePayloads {
						if decrypted, err := s.encryption.DecryptJSON([]byte(payloadStr)); err == nil {
							payloadStr = string(decrypted)
						}
					}
					populateFromPayload(&rec, []byte(payloadStr))
				}

				select {
				case eventCh <- rec:
				case <-ctx.Done():
					rows.Close()
					errCh <- ctx.Err()
					return
				}
			}
			rows.Close()

			if err := rows.Err(); err != nil {
				errCh <- err
				return
			}

			if pageCount < pageSize {
				return
			}
			offset += pageSize
		}
	}()

	return eventCh, errCh
}

// CountEventHistory returns the total number of events for a workflow.
func (s *MySQLStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_history WHERE workflow_id = ? AND tenant_id = ?`, workflowID, s.tenantID).Scan(&count)
	return count, err
}

// ---------------------------------------------------------------------------
// VerifyWorkflowEvents
// ---------------------------------------------------------------------------

// VerifyWorkflowEvents loads all events for a workflow and verifies their
// integrity by recomputing SHA-256 checksums and comparing them against the
// stored checksums. Before the checksum column migration, it loads and
// computes checksums silently and returns nil.
func (s *MySQLStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error {
	// Load the full event history for the workflow.
	events, err := s.LoadEventHistory(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("verify events: load: %w", err)
	}

	// Try to load stored checksums from the DB. If the column doesn't exist
	// (pre-migration), this query will fail, and we skip verification.
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, checksum FROM event_history
		WHERE workflow_id = ? AND tenant_id = ?
		ORDER BY step
	`, workflowID, s.tenantID)
	if err != nil {
		// Column does not exist yet -- skip verification (pre-migration).
		return nil
	}
	defer rows.Close()

	storedChecksums := make(map[int]string)
	for rows.Next() {
		var step int
		var checksum sql.NullString
		if err := rows.Scan(&step, &checksum); err != nil {
			return fmt.Errorf("verify events: scan checksum: %w", err)
		}
		if checksum.Valid && checksum.String != "" {
			storedChecksums[step] = checksum.String
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify events: rows: %w", err)
	}

	// If no checksums are stored, verification is not possible yet.
	if len(storedChecksums) == 0 {
		return nil
	}

	// Recompute and compare checksums with chaining.
	var prevChecksum string
	for _, ev := range events {
		expected, ok := storedChecksums[ev.Step]
		if !ok || expected == "" {
			prevChecksum = "" // Missing event breaks the chain
			continue          // No stored checksum for this step (pre-migration partial data).
		}
		actual := computeEventChecksum(ev, prevChecksum)
		if actual != expected {
			return fmt.Errorf("verify events: workflow %s step %d: checksum mismatch (expected %s, got %s)",
				workflowID, ev.Step, expected, actual)
		}
		prevChecksum = expected
	}
	return nil
}
