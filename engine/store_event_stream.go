package engine

import (
	"context"
	"database/sql"
	"fmt"
)

func (s *PostgresStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load history paginated: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       payload_encoding,
		       promise_name, promise_id, promise_result, promise_error,
		       created_at
		FROM event_history
		WHERE workflow_id = $1 AND tenant_id = $2
		ORDER BY step
		OFFSET $3 LIMIT $4
	`, workflowID, s.tenantID, offset, limit)
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
		var createdAt sql.NullTime
		var payloadEnc sql.NullInt16

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&payloadEnc,
			&promiseName, &promiseID, &promiseResult, &promiseError,
			&createdAt); err != nil {
			return nil, fmt.Errorf("scan history paginated: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = decodePayload(request.String, payloadEnc)
		rec.Response = decodePayload(response.String, payloadEnc)
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
		if createdAt.Valid {
			applyCreatedAt(&rec, createdAt.Time)
		}

		// Decrypt and redact event record.
		s.decryptAndRedactEventRecord(&rec, workflowID)

		// Retroactive redaction on read path.
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
			payloadStr := s.decryptPayloadJSON(payload.String)
			populateFromPayload(&rec, []byte(payloadStr))
		}

		history = append(history, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return history, tx.Commit()
}

func (s *PostgresStore) StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan EventRecord, <-chan error) {
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

			// One RLS-armed transaction per page, not s.db directly.
			//
			// cleat#1178: event_history has ENABLE + FORCE ROW LEVEL SECURITY
			// and its policy is `tenant_id = cleat.assert_tenant_set()`, which
			// RAISES the moment a candidate row is examined. A statement issued
			// on s.db runs outside any transaction, so nothing has set
			// cleat.tenant_id and this failed for ANY workflow that has events
			// -- a wider blast radius than cleat#1177, which needed a chain with
			// a successor.
			//
			// It was latent rather than live only because this method has no
			// production caller; store_reachability_test.go lists it in
			// storeUnreachedBaseline. Fixed rather than commented, so that
			// wiring it up is not also a bug report.
			//
			// Per PAGE rather than one transaction around the whole stream: the
			// consumer decides how fast it reads, and holding a transaction open
			// across an unbounded channel send would pin a connection for as
			// long as the reader takes. Paging already accepts that rows may
			// change between pages -- LIMIT/OFFSET over ORDER BY step says so.
			tx, err := s.beginTxWithRLS(ctx)
			if err != nil {
				errCh <- err
				return
			}

			rows, err := tx.QueryContext(ctx, `
				SELECT step, event_type, service, operation, request, response, error,
				       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
				       defer_description, defer_id, child_name, child_input, run_id, new_input,
				       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
				       payload,
				       payload_encoding,
				       promise_name, promise_id, promise_result, promise_error,
				       created_at
				FROM event_history
				WHERE workflow_id = $1
				ORDER BY step
				LIMIT $2 OFFSET $3
			`, workflowID, pageSize, offset)
			if err != nil {
				_ = tx.Rollback()
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
				var createdAt sql.NullTime
				var payloadEnc sql.NullInt16

				if err := rows.Scan(&rec.Step, &rec.EventType,
					&service, &op, &request, &response, &errMsg,
					&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
					&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
					&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
					&payload,
					&payloadEnc,
					&promiseName, &promiseID, &promiseResult, &promiseError,
					&createdAt); err != nil {
					rows.Close()
					_ = tx.Rollback()
					errCh <- err
					return
				}
				if createdAt.Valid {
					applyCreatedAt(&rec, createdAt.Time)
				}

				rec.Service = service.String
				rec.Op = op.String
				rec.Request = decodePayload(request.String, payloadEnc)
				rec.Response = decodePayload(response.String, payloadEnc)
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

				// Decrypt and redact event record.
				s.decryptAndRedactEventRecord(&rec, workflowID)

				if payload.Valid {
					payloadStr := s.decryptPayloadJSON(payload.String)
					populateFromPayload(&rec, []byte(payloadStr))
				}

				select {
				case eventCh <- rec:
				case <-ctx.Done():
					rows.Close()
					_ = tx.Rollback()
					errCh <- ctx.Err()
					return
				}
			}
			rows.Close()

			if err := rows.Err(); err != nil {
				_ = tx.Rollback()
				errCh <- err
				return
			}
			if err := tx.Commit(); err != nil {
				errCh <- err
				return
			}

			// If we got fewer rows than the page size, we're done.
			if pageCount < pageSize {
				return
			}
			offset += pageSize
		}
	}()

	return eventCh, errCh
}

func (s *PostgresStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count event history: begin: %w", err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_history WHERE workflow_id = $1 AND tenant_id = $2`, workflowID, s.tenantID).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, tx.Commit()
}
