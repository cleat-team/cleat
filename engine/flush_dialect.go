package engine

import (
	"context"
	"fmt"
)

// perStepEventFlusher is implemented by stores whose database does not speak the
// dialect insertEventSQL is written in.
//
// engine/flush.go's per-step flush is hand-written PostgreSQL: $N placeholders,
// ON CONFLICT ... DO UPDATE, set_config for the RLS context. The worker opens
// whatever *sql.DB the --db DSN produces -- PostgreSQL, MySQL or SQL Server --
// and hands it to engine.WithDB unconditionally. On the other two dialects every
// per-step flush therefore failed at parse time, and engine/lifecycle.go logged
// the failure and carried on:
//
//	mysql: Error 1064 ... near 'CONFLICT (workflow_id, step) DO UPDATE SET ...'
//	mssql: 'set_config' is not a recognized built-in function name
//
// Nothing was lost outright, because FinalizeWorkflowSegment appends the whole
// segment through the store when the segment ends. What was lost is the reason
// per-step flush exists: surviving a crash *mid*-segment. A MySQL or SQL Server
// deployment silently got the behaviour docs/durable-calls.md attributes to
// --no-per-step-flush -- "higher throughput, weaker crash safety" -- without
// setting the flag, and with no way to tell from outside. See
// TestPerStepFlushWorksOnEveryDialect, whose PostgreSQL subtest is the control.
//
// The method is unexported so this stays an internal arrangement between the
// engine and the three stores it ships with, rather than a new public extension
// point. PostgresStore deliberately does not implement it: the engine's own SQL
// is already correct there, and leaving that path untouched keeps this change
// off the primary dialect entirely.
type perStepEventFlusher interface {
	flushEventForStep(ctx context.Context, workflowID string, rec EventRecord) error
}

// flushEventForStep persists one event immediately, in MySQL's dialect.
//
// It does not touch event_count. That is deliberate and it is what makes the
// per-step flush safe to combine with the finalize append: appendEventsInTx
// increments event_count by len(recs), FinalizeWorkflowSegment appends the whole
// segment at the end, and its INSERT IGNORE drops the rows this already wrote.
// If the per-step path counted as well, every event would be counted twice and
// quota enforcement would trip at half the configured limit. PostgreSQL's raw
// per-step insert does not increment either, so this matches it.
func (s *MySQLStore) flushEventForStep(ctx context.Context, workflowID string, rec EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("flush event (mysql): begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.appendEventsInTxOpts(ctx, tx, workflowID, []EventRecord{rec}, false); err != nil {
		return fmt.Errorf("flush event (mysql): %w", err)
	}
	return tx.Commit()
}

// flushEventForStep persists one event immediately, in SQL Server's dialect.
//
// beginTxWithContext rather than a bare BeginTx: the tenant scope on SQL Server
// is session context, it is cleared when a pooled connection is recycled, and it
// has to be established inside the transaction that uses it. That is §2.71, and
// getting it wrong here would fail closed rather than leak, but it would fail.
//
// See MySQLStore.flushEventForStep for why event_count is left alone.
func (s *MSSQLStore) flushEventForStep(ctx context.Context, workflowID string, rec EventRecord) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("flush event (mssql): %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.appendEventsInTxOpts(ctx, tx, workflowID, []EventRecord{rec}, false); err != nil {
		return fmt.Errorf("flush event (mssql): %w", err)
	}
	return tx.Commit()
}
