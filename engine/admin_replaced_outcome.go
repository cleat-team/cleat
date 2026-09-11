package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AdminReplacedOutcome is the terminal outcome an administrative action
// destroyed, captured before the statement that destroys it.
//
// # Why this exists
//
// AdminReReplay resets a stopped workflow to 'ready' and, in the same
// statement, sets error_msg, error_code, error_op and completed_at to NULL.
// Clearing them is correct -- the run is starting again and a stale error on a
// 'ready' row would be worse -- but until this type the audit event written in
// the same transaction recorded only {Action, Operator, Reason}. Who and why,
// and not what it destroyed.
//
// The sequence that costs is the ordinary one: a workflow fails, an operator
// reads the error and judges it transient, re-replays, and it fails
// DIFFERENTLY. The original failure is then gone, and with it the only evidence
// for whether re-replaying was the right call. It is not recoverable from the
// history either -- a terminal failure is not an event; the outcome lives in
// the row's own columns, which are exactly what the statement clears.
//
// cleat#1185.
type AdminReplacedOutcome struct {
	Status    string
	ErrorMsg  string
	ErrorCode string
	ErrorOp   string
	// CompletedAt is RFC3339, or empty when the row carried none. A string
	// rather than a time.Time because this is written into an event payload
	// that is hashed: a zero time.Time would serialise to a value, and the
	// point of every field here is to be absent when there was nothing to
	// record.
	CompletedAt string
}

// IsEmpty reports whether there was nothing to record. A workflow re-replayed
// from a state that carried no error and no completion is the ordinary case for
// a 'stopped' run, and it must not add a payload key -- see
// eventRecordToPayload's admin_action arm for why an unconditional key is not
// free.
func (o *AdminReplacedOutcome) IsEmpty() bool {
	return o == nil || (o.ErrorMsg == "" && o.ErrorCode == "" && o.ErrorOp == "" && o.CompletedAt == "")
}

// rowQueryer is the subset of *sql.Tx this needs, so the read happens inside
// the caller's transaction rather than on a fresh connection. Reading it
// outside would race the UPDATE that erases it, which is the one thing this
// must not do.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readReplacedOutcome captures the outcome about to be erased. It must be
// called BEFORE the UPDATE and inside the same transaction.
//
// PostgreSQL's RETURNING cannot serve here: it yields post-update values, so
// the four columns would all come back NULL -- the very state being recorded.
// (RETURNING OLD arrived in PostgreSQL 18; this supports older servers and two
// other dialects.)
//
// A missing row is not an error. The UPDATE that follows will match nothing and
// the caller's existing zero-row path resolves it into the right message; this
// returning nil keeps that one place in charge of saying so.
func readReplacedOutcome(ctx context.Context, q rowQueryer, d Dialect, workflowID, tenantID string) (*AdminReplacedOutcome, error) {
	query := fmt.Sprintf(
		"SELECT status, error_msg, error_code, error_op, completed_at FROM workflow_instances WHERE id = %s AND tenant_id = %s",
		d.placeholder(1), d.placeholder(2))

	var status string
	var msg, code, op sql.NullString
	var completed sql.NullTime
	err := q.QueryRowContext(ctx, query, workflowID, tenantID).Scan(&status, &msg, &code, &op, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the outcome being replaced: %w", err)
	}

	out := &AdminReplacedOutcome{
		Status:    status,
		ErrorMsg:  msg.String,
		ErrorCode: code.String,
		ErrorOp:   op.String,
	}
	if completed.Valid {
		out.CompletedAt = completed.Time.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}
