package engine

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

func (s *PostgresStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append history batch: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return fmt.Errorf("append history batch: set rls: %w", err)
	}

	if err := s.appendEventsInTx(ctx, tx, workflowID, recs); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) appendEventsInTx(ctx context.Context, tx *sql.Tx, workflowID string, recs []EventRecord) error {
	if len(recs) == 0 {
		return nil
	}

	// Seed the chain from what is already persisted, and walk the batch in
	// step order. Both matter: a workflow that suspends and resumes writes its
	// history over several calls (cmd/cleat-worker/setup.go finalizes each
	// segment with only that segment's new events), so restarting the chain at
	// each call makes VerifyWorkflowEvents report every resumed workflow as
	// corrupt.
	order := chainOrder(recs)
	prevChecksum, err := s.previousStoredChecksum(ctx, tx, workflowID, recs[order[0]].Step)
	if err != nil {
		return err
	}

	// For a single event, use direct Exec to avoid a PREPARE round-trip.
	// For multiple events, use PrepareContext to avoid re-parsing per event.
	if len(recs) == 1 {
		if err := s.appendOneEvent(ctx, tx, workflowID, recs[0], prevChecksum); err != nil {
			return err
		}
	} else {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
				duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
				defer_description, defer_id, child_name, child_input, run_id, new_input,
				plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
				promise_name, promise_id, promise_result, promise_error, payload,
				created_at, checksum, tenant_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32)
			ON CONFLICT (workflow_id, step) DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error WHERE event_history.response = '' AND event_history.error IS NULL
		`)
		if err != nil {
			return fmt.Errorf("append events in tx: prepare: %w", err)
		}
		defer stmt.Close()

		for _, i := range order {
			rec := recs[i]
			if err := s.execEventStmt(ctx, stmt, workflowID, rec, prevChecksum); err != nil {
				return err
			}
			prevChecksum = computeEventChecksum(rec, prevChecksum)
		}
	}

	// Increment event_count on workflow_instances so quota enforcement
	// has an up-to-date count.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_instances SET event_count = event_count + $1 WHERE id = $2`,
		len(recs), workflowID); err != nil {
		return fmt.Errorf("append events in tx: increment event_count: %w", err)
	}

	return nil
}

// appendOneEvent inserts a single event without a prepared statement.
// previousStoredChecksum returns the checksum of the last event already stored
// for workflowID before step, or "" when there is none.
//
// It reads the immediately preceding row rather than the last row that has a
// checksum, because that is what VerifyWorkflowEvents does: it walks the
// history in step order and resets the chain to "" whenever a row's checksum is
// missing. Seeding from anything else would agree with the table but not with
// the verifier.
func (s *PostgresStore) previousStoredChecksum(ctx context.Context, tx *sql.Tx, workflowID string, step int) (string, error) {
	var checksum sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT checksum FROM event_history
		WHERE workflow_id = $1 AND tenant_id = $2 AND step < $3
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

func (s *PostgresStore) appendOneEvent(ctx context.Context, tx *sql.Tx, workflowID string, rec EventRecord, prevChecksum string) error {
	payload, _ := eventRecordToPayload(rec)
	checksum := computeEventChecksum(rec, prevChecksum)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
			duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
			defer_description, defer_id, child_name, child_input, run_id, new_input,
			plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
			promise_name, promise_id, promise_result, promise_error, payload,
			created_at, checksum, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32)
		ON CONFLICT (workflow_id, step) DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error WHERE event_history.response = '' AND event_history.error IS NULL
	`, workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Request))), nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Response))), nullStr(rec.Err),
		nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
		nullStr(rec.SignalName), nullStr(rec.SignalPayload),
		nullStr(rec.DeferDescription), nullStr(rec.DeferID),
		nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput),
		nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(rec.PluginInput), nullStr(rec.PluginOutput), nullStr(rec.PluginError),
		nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(rec.PromiseResult), nullStr(rec.PromiseError),
		nullStr(string(payload)),
		time.UnixMilli(rec.TimestampMs),
		checksum, s.tenantID)
	if err != nil {
		return fmt.Errorf("append one event: exec step %d: %w", rec.Step, err)
	}
	return nil
}

// execEventStmt executes a prepared INSERT for a single event.
func (s *PostgresStore) execEventStmt(ctx context.Context, stmt *sql.Stmt, workflowID string, rec EventRecord, prevChecksum string) error {
	payload, _ := eventRecordToPayload(rec)
	checksum := computeEventChecksum(rec, prevChecksum)
	payloadArg := sql.NullString{String: string(payload), Valid: len(payload) > 0}
	_, err := stmt.ExecContext(ctx, workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Request))), nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Response))), nullStr(rec.Err),
		nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
		nullStr(rec.SignalName), nullStr(rec.SignalPayload),
		nullStr(rec.DeferDescription), nullStr(rec.DeferID),
		nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput),
		nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(rec.PluginInput), nullStr(rec.PluginOutput), nullStr(rec.PluginError),
		nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(rec.PromiseResult), nullStr(rec.PromiseError),
		payloadArg,
		time.UnixMilli(rec.TimestampMs),
		checksum, s.tenantID)
	if err != nil {
		return fmt.Errorf("exec event stmt: step %d: %w", rec.Step, err)
	}
	return nil
}

func (s *PostgresStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	return s.AppendEventHistoryBatch(ctx, workflowID, []EventRecord{rec})
}

func (s *PostgresStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error {
	// Load the full event history for the workflow.
	events, err := s.LoadEventHistory(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("verify events: load: %w", err)
	}

	// Try to load stored checksums from the DB. If the column doesn't exist
	// (pre-migration), this query will fail, and we skip verification.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("verify events: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT step, checksum FROM event_history
		WHERE workflow_id = $1
		ORDER BY step
	`, workflowID)
	if err != nil {
		// Column does not exist yet — skip verification (pre-migration).
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

	// The chain above certifies payload, which is the only copy the checksum
	// covers and the only one replay reads. It says nothing about the
	// duplicate columns every SQL consumer reads -- see store_event_shadow.go.
	if err := s.verifyShadowColumns(ctx, tx, workflowID); err != nil {
		return err
	}
	return tx.Commit()
}
