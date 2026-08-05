package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func (s *PostgresStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("request cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true, cancellation_reason = $2
		WHERE id = $1
	`, workflowID, reason)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CheckCancellation checks if a workflow has been cancelled.

func (s *PostgresStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, "", fmt.Errorf("check cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	var cancelled bool
	var reason sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = $1
	`, workflowID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, tx.Commit()
}

// PollAndClaimSignal atomically checks for and claims a pending signal.

func (s *PostgresStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return "", false, err
	}

	var payload string
	err = tx.QueryRowContext(ctx, `
		DELETE FROM workflow_signals
		WHERE workflow_id = $1 AND signal_name = $2
		RETURNING payload
	`, workflowID, signalName).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, tx.Rollback()
	}
	if err != nil {
		return "", false, fmt.Errorf("poll signal: %w", err)
	}
	return decodeJSONPayload(payload), true, tx.Commit()
}

// encodeJSONPayload makes a payload acceptable to the `payload` column.
//
// All three dialects require valid JSON there and say so differently:
// PostgreSQL's column is JSONB, MySQL's is JSON, and SQL Server's is
// NVARCHAR(MAX) with a CHECK (ISJSON(payload) = 1) constraint. A caller that
// passes a bare string -- "payload-1", an opaque token, an ID -- is passing
// something none of them will store, so it is wrapped as a JSON string literal
// and decodeJSONPayload unwraps it on the way out.
//
// This lived inline in PostgresStore.DeliverSignal and nowhere else, so a
// non-JSON signal payload was accepted on PostgreSQL and rejected outright on
// the other two:
//
//	mysql: Error 3140 (22032): Invalid JSON text: "Invalid value." at position 0
//
// DeliverSignal is reachable from the worker's signal endpoint, so that was a
// live behavioural difference between the dialects, not just a test artefact.
func encodeJSONPayload(payload string) string {
	if json.Valid([]byte(payload)) {
		return payload
	}
	// Marshal rather than concatenating quotes: a payload containing a quote or
	// a backslash would otherwise produce invalid JSON and be rejected by the
	// very columns this exists to satisfy.
	//
	// Used for workflow_signals.payload (2.60c) and, since 3.19, for
	// workflow_update_requests.payload -- the sibling column with the same
	// constraint, which 2.60c did not reach.
	encoded, err := json.Marshal(payload)
	if err != nil {
		return payload
	}
	return string(encoded)
}

// decodeJSONPayload reverses what DeliverSignal does to a payload before
// writing it to the JSONB `payload` column (wrapping it in quotes if it
// isn't already valid JSON, so the column accepts it) and normalizes
// whitespace for payloads that were already JSON. Without this, callers get
// back the JSONB column's on-disk text representation verbatim, which
// differs from what was originally passed to DeliverSignal in two ways:
// PostgreSQL's jsonb text output always inserts a space after every ':' and
// ',' (so `{"data":"hello"}` round-trips as `{"data": "hello"}`), and a
// plain, non-JSON string like "payload-1" comes back as the JSON string
// literal `"payload-1"`, quotes included, rather than the original bytes.
func decodeJSONPayload(raw string) string {
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err == nil {
		return s
	}
	compacted := bytes.NewBuffer(nil)
	if err := json.Compact(compacted, []byte(raw)); err == nil {
		return compacted.String()
	}
	return raw
}

// StartNewRun creates a new workflow instance.
// If idempotencyKey is non-empty, provides exactly-once semantics: a subsequent
// call with the same key returns the existing workflow ID without creating a
// duplicate. Returns the workflow ID, whether it already existed, and any error.

func (s *PostgresStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("deliver signal: begin: %w", err)
	}
	defer tx.Rollback()

	payload = encodeJSONPayload(payload)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workflow_id, signal_name) DO UPDATE SET payload = $3, delivered_at = now()
	`, workflowID, signalName, payload, s.tenantID)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET next_wake_at = now()
		WHERE id = $1 AND status IN ('ready', 'suspended')
	`, workflowID)
	if err != nil {
		return err
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// PollSignal satisfies the SignalStore interface by checking for a delivered
// signal, without consuming it. This must be a plain read: it used to
// delegate straight to PollAndClaimSignal, whose name and doc comment both
// say it "atomically checks for AND CLAIMS" a signal (i.e. DELETEs the row)
// -- the opposite of what SignalStore's own doc comment promises for
// PollSignal ("checks for a delivered signal", no mention of consuming it).
// A second PollSignal call for the same signal would find nothing, having
// silently deleted it on the first call.
func (s *PostgresStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return "", false, err
	}

	var payload string
	err = tx.QueryRowContext(ctx, `
		SELECT payload FROM workflow_signals
		WHERE workflow_id = $1 AND signal_name = $2
	`, workflowID, signalName).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, tx.Rollback()
	}
	if err != nil {
		return "", false, fmt.Errorf("poll signal: %w", err)
	}
	return decodeJSONPayload(payload), true, tx.Commit()
}

// PollCancellation satisfies the SignalStore interface.

func (s *PostgresStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return s.CheckCancellation(ctx, workflowID)
}

// GetAllowedSignalCallers returns the allowed_signals list for a workflow.
// Returns nil when allowed_signals is NULL or the target workflow doesn't exist.

func (s *PostgresStore) GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get allowed signal callers: begin: %w", err)
	}
	defer tx.Rollback()

	var raw sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT allowed_signals FROM workflow_instances WHERE id = $1 AND tenant_id = $2`,
		workflowID, s.tenantID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, fmt.Errorf("get allowed signal callers: %w", err)
	}
	if !raw.Valid || raw.String == "" || raw.String == "null" {
		return nil, tx.Commit()
	}
	var callers []string
	if err := json.Unmarshal([]byte(raw.String), &callers); err != nil {
		return nil, fmt.Errorf("get allowed signal callers: parse: %w", err)
	}
	return callers, tx.Commit()
}

// GetQueryState returns the value for a key in the workflow's query_state JSONB.
