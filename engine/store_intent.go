package engine

// Write-ahead call intent: the store half.
//
// IMPROVEMENT-PLAN 1.4 phase D; design in docs/durable-call-intent-design.md §5.
//
// A durable call dispatches the external request and then records the outcome.
// A crash in between loses the outcome, and replay makes the call again -- a
// duplicated real-world side effect, produced silently. These two methods let
// the engine write down that a call is *about* to happen, before it happens, so
// that replay can tell "this never ran" from "this may have run".
//
// An event is PENDING iff intent_at IS NOT NULL AND checksum IS NULL. Both
// columns are set by WriteCallIntent and cleared by CompleteCallIntent in the
// same statement that writes the outcome, so they cannot disagree with it.
//
// # Why not a sentinel in the error column
//
// The deleted implementation (flushCallIntent) wrote a sentinel string into
// event_history.error. Every completion path upserts with
// ON CONFLICT ... DO UPDATE ... WHERE response = <empty> AND error IS NULL, so a
// row whose error held the sentinel could never be completed: the completion
// was a silent no-op, the row stayed pending forever, and every later replay
// reported [AMBIGUOUS]. Keeping `error` meaning only "the call failed" removes
// that by construction.
//
// # Why checksum is NULL while pending
//
// A pending row is incomplete -- the response has not been written -- so there
// is nothing stable to checksum. VerifyWorkflowEvents already skips rows with
// no checksum, so a pending row is passed over rather than reported as corrupt.
// That is the second defect of the deleted implementation, which computed the
// checksum over a record whose Err was empty and then stored the sentinel in
// the error column, so the workflow failed verification in exactly the crash
// window the feature exists to handle.
//
// # Interaction with the finalize append
//
// FinalizeWorkflowSegment appends the whole segment through appendEventsInTx at
// the end, including steps this path already wrote. That is safe: the upsert
// updates only rows whose response is empty and whose error IS NULL, and a
// completed intent row has one or the other set, so the append is a no-op for
// it. The checksum it would have computed is the same one written here, because
// it is computed from the same record, so the chain agrees either way.

import (
	"context"
	"fmt"
)

// callIntentStore is implemented by the three shipped stores. It is unexported
// for the same reason perStepEventFlusher is: this is an arrangement between
// the engine and its own stores, not a new public extension point.
type callIntentStore interface {
	// WriteCallIntent durably records that a call is about to be dispatched.
	// It must not return until the row is committed -- durability before the
	// side effect is the entire point.
	WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord) error

	// CompleteCallIntent writes the outcome over the pending row and clears
	// its pending state. It returns errIntentNotPending if no pending row
	// matched, which means something else has already resolved this step.
	CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string) error
}

// errIntentNotPending is returned when a completion matches no pending row.
//
// It is an error rather than a silent no-op because the two ways it can happen
// are both worth knowing about: the intent was never written (so the call was
// dispatched without durability, which this feature exists to prevent), or
// something else completed the step (so two writers believe they own it).
var errIntentNotPending = fmt.Errorf("call intent: no pending row to complete")

// ---------------------------------------------------------------------------
// PostgreSQL
// ---------------------------------------------------------------------------

func (s *PostgresStore) WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("write call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request,
			created_at, intent_at, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, now(), now(), $7)
	`, workflowID, rec.Step, rec.EventType, nullStr(rec.Service), nullStr(rec.Op),
		nullStr(rec.Request), s.tenantID); err != nil {
		return fmt.Errorf("write call intent: step %d: %w", rec.Step, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("write call intent: commit: %w", err)
	}
	return nil
}

func (s *PostgresStore) CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("complete call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE event_history
		SET response = $3, error = $4, payload = $5, checksum = $6, intent_at = NULL
		WHERE workflow_id = $1 AND step = $2 AND tenant_id = $7
		  AND intent_at IS NOT NULL AND checksum IS NULL
	`, workflowID, rec.Step, rec.Response, nullStr(rec.Err), nullStr(string(payload)),
		checksum, s.tenantID)
	if err != nil {
		return fmt.Errorf("complete call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete call intent: rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("complete call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// MySQL
// ---------------------------------------------------------------------------

func (s *MySQLStore) WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("write call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request,
			created_at, intent_at, tenant_id)
		VALUES (?, ?, ?, ?, ?, ?, NOW(6), NOW(6), ?)
	`, workflowID, rec.Step, rec.EventType, nullStr(rec.Service), nullStr(rec.Op),
		nullStr(rec.Request), s.tenantID); err != nil {
		return fmt.Errorf("write call intent: step %d: %w", rec.Step, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("write call intent: commit: %w", err)
	}
	return nil
}

func (s *MySQLStore) CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("complete call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE event_history
		SET response = ?, error = ?, payload = ?, checksum = ?, intent_at = NULL
		WHERE workflow_id = ? AND step = ? AND tenant_id = ?
		  AND intent_at IS NOT NULL AND checksum IS NULL
	`, rec.Response, nullStr(rec.Err), nullStr(string(payload)), checksum,
		workflowID, rec.Step, s.tenantID)
	if err != nil {
		return fmt.Errorf("complete call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete call intent: rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("complete call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// SQL Server
// ---------------------------------------------------------------------------

func (s *MSSQLStore) WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("write call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request,
			created_at, intent_at, tenant_id)
		VALUES (@p1, @p2, @p3, @p4, @p5, @p6, SYSUTCDATETIME(), SYSUTCDATETIME(), @p7)
	`, workflowID, rec.Step, string(rec.EventType), nullStr(rec.Service), nullStr(rec.Op),
		nullStr(rec.Request), s.tenantID); err != nil {
		return fmt.Errorf("write call intent: step %d: %w", rec.Step, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("write call intent: commit: %w", err)
	}
	return nil
}

func (s *MSSQLStore) CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("complete call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE event_history
		SET response = @p3, error = @p4, payload = @p5, checksum = @p6, intent_at = NULL
		WHERE workflow_id = @p1 AND step = @p2 AND tenant_id = @p7
		  AND intent_at IS NOT NULL AND checksum IS NULL
	`, workflowID, rec.Step, rec.Response, nullStr(rec.Err), nullStr(string(payload)),
		checksum, s.tenantID)
	if err != nil {
		return fmt.Errorf("complete call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete call intent: rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("complete call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

// assertion that all three stores satisfy the interface, checked at compile
// time rather than at the first crash that needed it.
var (
	_ callIntentStore = (*PostgresStore)(nil)
	_ callIntentStore = (*MySQLStore)(nil)
	_ callIntentStore = (*MSSQLStore)(nil)
)

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// ResolveCallIntent completes a pending intent row with an outcome obtained
// from somewhere other than the original call -- today, an AmbiguityResolver
// that asked the service what happened. See IMPROVEMENT-PLAN 1.4 phase E.
//
// It differs from CompleteCallIntent in where the checksum comes from, and that
// is the whole reason it exists. CompleteCallIntent is called by the worker
// that made the call, which is holding the chain in s.lastChecksum. A
// resolution happens during replay, where the session is reading history rather
// than building it and has no chain in hand -- so the previous checksum is read
// from the row before this one, inside the same transaction as the update.
//
// That is safe here in a way it was not for the deleted flushCallIntent, whose
// third defect was reading the chain from the database: everything before a
// pending row has been persisted by definition, because the crash that created
// the row happened after them. There is nothing in flight to disagree with.
//
// Persisting matters more than it might look. Without it every replay would ask
// the resolver again, and a service that answered differently the second time
// -- or was unreachable -- would make the same step resolve one way on one
// replay and another way on the next. Replay determinism is the constraint this
// whole stream is held to, so a resolution is written down once and read back
// from then on.
type callIntentResolver interface {
	ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte) error
}

func (s *PostgresStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("resolve call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	prev, err := s.previousStoredChecksum(ctx, tx, workflowID, rec.Step)
	if err != nil {
		return fmt.Errorf("resolve call intent: previous checksum: %w", err)
	}
	checksum := computeEventChecksum(rec, prev)

	res, err := tx.ExecContext(ctx, `
		UPDATE event_history
		SET response = $3, error = $4, payload = $5, checksum = $6, intent_at = NULL
		WHERE workflow_id = $1 AND step = $2 AND tenant_id = $7
		  AND intent_at IS NOT NULL AND checksum IS NULL
	`, workflowID, rec.Step, rec.Response, nullStr(rec.Err), nullStr(string(payload)),
		checksum, s.tenantID)
	if err != nil {
		return fmt.Errorf("resolve call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve call intent: rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("resolve call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

func (s *MySQLStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("resolve call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	prev, err := s.previousStoredChecksum(ctx, tx, workflowID, rec.Step)
	if err != nil {
		return fmt.Errorf("resolve call intent: previous checksum: %w", err)
	}
	checksum := computeEventChecksum(rec, prev)

	res, err := tx.ExecContext(ctx, `
		UPDATE event_history
		SET response = ?, error = ?, payload = ?, checksum = ?, intent_at = NULL
		WHERE workflow_id = ? AND step = ? AND tenant_id = ?
		  AND intent_at IS NOT NULL AND checksum IS NULL
	`, rec.Response, nullStr(rec.Err), nullStr(string(payload)), checksum,
		workflowID, rec.Step, s.tenantID)
	if err != nil {
		return fmt.Errorf("resolve call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve call intent: rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("resolve call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

func (s *MSSQLStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("resolve call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	prev, err := s.previousStoredChecksum(ctx, tx, workflowID, rec.Step)
	if err != nil {
		return fmt.Errorf("resolve call intent: previous checksum: %w", err)
	}
	checksum := computeEventChecksum(rec, prev)

	res, err := tx.ExecContext(ctx, `
		UPDATE event_history
		SET response = @p3, error = @p4, payload = @p5, checksum = @p6, intent_at = NULL
		WHERE workflow_id = @p1 AND step = @p2 AND tenant_id = @p7
		  AND intent_at IS NOT NULL AND checksum IS NULL
	`, workflowID, rec.Step, rec.Response, nullStr(rec.Err), nullStr(string(payload)),
		checksum, s.tenantID)
	if err != nil {
		return fmt.Errorf("resolve call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve call intent: rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("resolve call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

var (
	_ callIntentResolver = (*PostgresStore)(nil)
	_ callIntentResolver = (*MySQLStore)(nil)
	_ callIntentResolver = (*MSSQLStore)(nil)
)
