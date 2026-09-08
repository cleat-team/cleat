package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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

// ConsumeSignal satisfies the SignalStore interface.
//
// This replaces PollAndClaimSignal, which read and deleted in one step and had
// no caller at all -- the whole reason a signal was never consumed and an
// await loop could re-read the same payload forever (IMPROVEMENT-PLAN 3.215).
// Consumption is now a separate call the await path makes AFTER its
// signal_received event is durable, which is what makes delivery at-least-once
// rather than at-most-once.
//
// workflowID is not needed to identify the row -- id alone does that -- but it
// is in the signature because ShardedStore routes on it, and it is in the
// WHERE clause so a caller cannot delete another workflow's delivery by
// guessing an id.
func (s *PostgresStore) ConsumeSignal(ctx context.Context, workflowID string, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return err
	}

	// The consumed counter, bumped in the same statement batch as the delete.
	//
	// finalize wakes a segment that CONSUMED something and still has rows
	// waiting, because a segment that consumed once can consume again --
	// progress is what separates a burst worth draining from an unrelated
	// pending signal that would spin (cleat#953). Bumped here rather than
	// through a new store method, because ConsumeSignal already writes.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM workflow_signals WHERE id = $1 AND workflow_id = $2
	`, id, workflowID); err != nil {
		return fmt.Errorf("consume signal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances SET signal_consumed_seq = signal_consumed_seq + 1
		WHERE id = $1
	`, workflowID); err != nil {
		return fmt.Errorf("consume signal: bump consumed counter: %w", err)
	}
	return tx.Commit()
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
	// A plain INSERT. It carried ON CONFLICT (workflow_id, signal_name) DO
	// UPDATE until 3.215, which discarded the earlier payload with no error --
	// so a workflow collecting one approval per reviewer saw only the last.
	// The conflict is gone because the key is gone: the table's primary key is
	// now a surrogate id and every delivery is its own row.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		VALUES ($1, $2, $3, $4)
	`, workflowID, signalName, payload, s.tenantID)
	if err != nil {
		return err
	}

	// Two writes, two different windows, and neither replaces the other.
	//
	// next_wake_at wakes a workflow that is ALREADY suspended, and its
	// status filter is why: 'running' is deliberately excluded, because a
	// claimed workflow's row is about to be overwritten by finalize anyway.
	//
	// signal_seq covers the window that leaves -- a delivery arriving while
	// the workflow is awake. The worker captured this value when it claimed;
	// finalize compares and schedules an immediate wake if it moved. Bumped
	// unconditionally, in this transaction, so the counter and the delivery
	// become visible together: a reader that can see the row can see the
	// bump (cleat#953).
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET signal_seq = signal_seq + 1,
		    next_wake_at = CASE WHEN status IN ('ready', 'suspended') THEN now() ELSE next_wake_at END
		WHERE id = $1
	`, workflowID)
	if err != nil {
		return err
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// PollSignal satisfies the SignalStore interface by returning the oldest
// unconsumed delivery with this name, without consuming it.
//
// ORDER BY id LIMIT 1 is the whole of the FIFO guarantee, and it needs the
// surrogate key to mean anything: before 3.215 there was at most one row per
// (workflow_id, signal_name), so "which one" was not a question the query
// could be asked.
//
// It must stay a plain read. It used to delegate straight to
// PollAndClaimSignal, whose name and doc comment both say it "atomically
// checks for AND CLAIMS" a signal (i.e. DELETEs the row) -- the opposite of
// what SignalStore's own doc comment promises here. Consumption is
// ConsumeSignal, called separately once the event is durable.
func (s *PostgresStore) PollSignal(ctx context.Context, workflowID, signalName string) (SignalDelivery, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SignalDelivery{}, false, err
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return SignalDelivery{}, false, err
	}

	var id int64
	var payload string
	var deliveredAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT id, payload, delivered_at FROM workflow_signals
		WHERE workflow_id = $1 AND signal_name = $2
		ORDER BY id
		LIMIT 1
	`, workflowID, signalName).Scan(&id, &payload, &deliveredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SignalDelivery{}, false, tx.Rollback()
	}
	if err != nil {
		return SignalDelivery{}, false, fmt.Errorf("poll signal: %w", err)
	}
	return SignalDelivery{
		ID:            id,
		Payload:       decodeJSONPayload(payload),
		DeliveredAtMs: deliveredAt.UnixMilli(),
	}, true, tx.Commit()
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

// ErrWorkflowNotFound is returned by SetAllowedSignalCallers when no workflow
// with the given id is visible to the calling store's tenant.
//
// It deliberately does not distinguish "no such workflow" from "another
// tenant's workflow". Splitting them would make the endpoint an existence
// oracle: a caller could enumerate ids and learn which ones belong to someone
// else from the difference in the error. The getter has the same property by
// construction -- it returns nil for both -- and this keeps the writer honest
// about the same boundary.
var ErrWorkflowNotFound = errors.New("workflow not found")

// ErrRoutingRuleNotFound is returned by RemoveRoutingRule when no rule with
// that ID exists.
//
// cleat#946 had two halves. #948 fixed the first -- ShardedStore routed the
// removal by rule ID while the rows are placed by workflow name, so it deleted
// from the wrong shard -- and this is the second: the removal reported success
// either way, because no implementation checked rows-affected. A DELETE that
// matches nothing succeeds, so the handler answered
// `200 {"status":"removed"}` for a rule that never existed, and an operator
// tearing down a canary was told it was gone.
var ErrRoutingRuleNotFound = errors.New("routing rule not found")

// SetAllowedSignalCallers replaces the allowed_signals list for a workflow.
//
// The write side of GetAllowedSignalCallers above. Until this existed, nothing
// in the product could write workflow_instances.allowed_signals -- no store
// method, no API, no CLI, no SDK -- while --require-signal-auth consulted it
// and denied every signal when it was empty. IMPROVEMENT-PLAN 3.15.
//
// An empty list writes NULL rather than "[]", so that a cleared list reads back
// as the getter's nil rather than as an empty non-null array. The two mean the
// same thing to signalCallerAllowed, but only one of them survives the round
// trip unchanged, and a setter whose output the getter renormalises is a
// setter whose tests can pass while the column holds something else.
func (s *PostgresStore) SetAllowedSignalCallers(ctx context.Context, workflowID string, callers []string) error {
	encoded, err := encodeAllowedSignals(callers)
	if err != nil {
		return fmt.Errorf("set allowed signal callers: %w", err)
	}

	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("set allowed signal callers: begin: %w", err)
	}
	defer tx.Rollback()

	// tenant_id in the predicate as well as RLS underneath it. On PostgreSQL
	// the policy would be enough; carrying it here keeps the three dialects'
	// statements the same shape, and MySQL has no policy to fall back on.
	res, err := tx.ExecContext(ctx,
		`UPDATE workflow_instances SET allowed_signals = $1 WHERE id = $2 AND tenant_id = $3`,
		encoded, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("set allowed signal callers: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set allowed signal callers: rows affected: %w", err)
	}
	if n == 0 {
		// This is what makes a cross-tenant write honest, not just harmless.
		// Under RLS the other tenant's row is invisible, so the UPDATE is not
		// refused -- it matches nothing and succeeds. Without this check the
		// caller is told the grant landed. Falsified: removing it turns
		// TestSetAllowedSignalCallersRejectsAnotherTenantsWorkflow/postgres red
		// with "tenant B ... was told it succeeded".
		return ErrWorkflowNotFound
	}
	return tx.Commit()
}

// encodeAllowedSignals renders a caller list for the allowed_signals column.
//
// Returns an invalid sql.NullString (SQL NULL) for an empty list; otherwise a
// JSON array, which is what GetAllowedSignalCallers unmarshals and what SQL
// Server's ck_workflow_instances_allowed_signals ISJSON check requires.
func encodeAllowedSignals(callers []string) (sql.NullString, error) {
	if len(callers) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(callers)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode allowed signals: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// GetQueryState returns the value for a key in the workflow's query_state JSONB.
