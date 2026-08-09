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
//
// # Fencing (B4)
//
// Both methods take workerID and generation, the same claim identity
// CompleteWorkflow/FailWorkflow/FinalizeWorkflowSegment already fence on. A
// caller that has neither -- engine.fencingEnabled() false, e.g. an Engine
// built without going through a claim -- passes workerID = "" and
// generation = 0, which every implementation below treats as "fencing not
// requested" and skips the check, matching the unfenced behaviour this had
// before B4.
//
// Both fold the fence into the same statement that does the write, exactly
// as insertEventSQL does for the per-step flush (see that constant's doc):
// no separate round trip, and the fence and the write cannot disagree about
// the moment they applied to, because they are the same statement. The two
// call sites still need this for different reasons:
//
//   - WriteCallIntent runs BEFORE the call is dispatched. A fence check that
//     fails here is the cheap case: the call is never made, so a zombie that
//     lost its lease before writing the intent cannot duplicate a real-world
//     side effect through this path either. Its INSERT has no ON CONFLICT
//     competing for the same zero-rows-affected signal, so a fence loss is
//     unambiguous: zero rows can only mean the fence failed.
//   - CompleteCallIntent runs AFTER the call is dispatched, so a lost fence
//     here cannot un-happen the call -- but it can and must stop the zombie
//     from overwriting a pending row that a new owner may have already acted
//     on (replayed past it as ambiguous, or resolved it through
//     ResolveCallIntent). Its UPDATE's WHERE already excludes non-pending
//     rows, so zero rows affected is ambiguous between "fence lost" and
//     "not pending" -- disambiguated post-hoc, only on that rare path, by
//     intentFenceOrNotPending.
type callIntentStore interface {
	// WriteCallIntent durably records that a call is about to be dispatched.
	// It must not return until the row is committed -- durability before the
	// side effect is the entire point. Returns ErrFenceLost if workerID and
	// generation are non-zero and no longer match the claim on record.
	WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord, workerID string, generation int64) error

	// CompleteCallIntent writes the outcome over the pending row and clears
	// its pending state. It returns errIntentNotPending if no pending row
	// matched, which means something else has already resolved this step, and
	// ErrFenceLost if workerID and generation are non-zero and no longer
	// match the claim on record -- checked first, since a fence loss is a
	// more specific and more actionable diagnosis than "not pending".
	CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string, workerID string, generation int64) error
}

// errIntentNotPending is returned when a completion matches no pending row.
//
// It is an error rather than a silent no-op because the two ways it can happen
// are both worth knowing about: the intent was never written (so the call was
// dispatched without durability, which this feature exists to prevent), or
// something else completed the step (so two writers believe they own it).
var errIntentNotPending = fmt.Errorf("call intent: no pending row to complete")

// intentFenceOrNotPending disambiguates a CompleteCallIntent/ResolveCallIntent
// UPDATE that affected zero rows, across all three dialects. It is called
// only when fencing was requested (workerID != "") and the UPDATE's own
// WHERE (which already excludes non-pending rows) still matched nothing --
// the rare path; every ordinary completion returns before reaching this.
//
// hb renews the lease one more time, the same Heartbeat call
// engine/flush.go's afterFencedInsert uses for the same reason: a lost fence
// is a more specific and more actionable diagnosis than "not pending" for a
// caller deciding what to do next, so it is worth one extra round trip to
// tell the two apart rather than reporting errIntentNotPending for both.
// Returning nil here means the fence still held, so the zero-rows result was
// the pre-existing "not pending" case and the caller falls through to that.
func intentFenceOrNotPending(ctx context.Context, hb func(ctx context.Context, workflowID, workerID string, generation int64) (bool, error), workflowID, workerID string, generation int64) error {
	held, err := hb(ctx, workflowID, workerID, generation)
	if err != nil {
		return fmt.Errorf("call intent: fence check: %w", err)
	}
	if !held {
		return ErrFenceLost
	}
	return nil
}

// ---------------------------------------------------------------------------
// PostgreSQL
// ---------------------------------------------------------------------------

// writeCallIntentSQLPostgres folds the fence directly into the INSERT: see
// insertEventSQL (engine/flush.go) for why this shape -- SELECT $1, $2, ...
// WHERE (fence), no FROM, no CTE -- resolves parameter types from the INSERT
// target list without explicit casts, checked against a real PostgreSQL
// instance rather than assumed. $8 = '' is the "fencing not requested"
// escape hatch; see callIntentStore's doc.
const writeCallIntentSQLPostgres = `
	INSERT INTO event_history (workflow_id, step, event_type, service, operation, request,
		created_at, intent_at, tenant_id)
	SELECT $1, $2, $3, $4, $5, $6, now(), now(), $7
	WHERE ($8 = '' OR EXISTS (
		SELECT 1 FROM workflow_instances WHERE id = $1 AND assigned_to = $8 AND generation = $9
	))`

func (s *PostgresStore) WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord, workerID string, generation int64) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("write call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, writeCallIntentSQLPostgres,
		workflowID, rec.Step, rec.EventType, nullStr(rec.Service), nullStr(rec.Op),
		nullStr(rec.Request), s.tenantID, workerID, generation)
	if err != nil {
		return fmt.Errorf("write call intent: step %d: %w", rec.Step, err)
	}
	// No ON CONFLICT here to produce a competing zero-rows-affected signal
	// (unlike insertEventSQL), so unlike afterFencedInsert this needs no
	// disambiguation: zero rows can only mean the fence failed.
	if n, _ := res.RowsAffected(); n == 0 && workerID != "" {
		return ErrFenceLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("write call intent: commit: %w", err)
	}
	return nil
}

func (s *PostgresStore) CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string, workerID string, generation int64) error {
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
		  AND ($8 = '' OR EXISTS (
		      SELECT 1 FROM workflow_instances WHERE id = $1 AND assigned_to = $8 AND generation = $9
		  ))
	`, workflowID, rec.Step, rec.Response, nullStr(rec.Err), nullStr(string(payload)),
		checksum, s.tenantID, workerID, generation)
	if err != nil {
		return fmt.Errorf("complete call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete call intent: rows affected: %w", err)
	}
	if n == 0 {
		// This transaction changed nothing and is never going to commit --
		// every path from here returns an error. Roll back before the
		// disambiguation check below: on MySQL, the EXISTS subquery this
		// UPDATE's WHERE clause just evaluated takes a lock on the
		// workflow_instances row that InnoDB holds until the transaction
		// ends, and intentFenceOrNotPending's Heartbeat call is a second,
		// separate transaction against that same row -- called while this
		// one is still open, it deadlocks against its own lock. Measured:
		// "Error 1205 (HY000): Lock wait timeout exceeded" on
		// TestCompleteCallIntent_FenceLost/mysql before this rollback was
		// added. Postgres's plain (non-locking) subquery reads do not have
		// this problem, and SQL Server's shouldn't under READ COMMITTED
		// either, but the rollback is applied uniformly rather than only
		// where it was caught: it costs nothing on a transaction that was
		// already doomed, and this dialect's locking behavior is unverified
		// (no MSSQL instance was assigned to this stream).
		_ = tx.Rollback()
		if workerID != "" {
			if ferr := intentFenceOrNotPending(ctx, s.Heartbeat, workflowID, workerID, generation); ferr != nil {
				return ferr
			}
		}
	}
	if n != 1 {
		return fmt.Errorf("complete call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// MySQL
// ---------------------------------------------------------------------------

func (s *MySQLStore) WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord, workerID string, generation int64) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("write call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// MySQL's ? placeholders are positional, not reusable by name the way
	// Postgres's $N are (see writeCallIntentSQLPostgres) -- workflowID and
	// workerID are each passed twice, once for their original use and once
	// for the fence check's own reference to the same value.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request,
			created_at, intent_at, tenant_id)
		SELECT ?, ?, ?, ?, ?, ?, NOW(6), NOW(6), ?
		WHERE (? = '' OR EXISTS (
			SELECT 1 FROM workflow_instances WHERE id = ? AND assigned_to = ? AND generation = ?
		))
	`, workflowID, rec.Step, rec.EventType, nullStr(rec.Service), nullStr(rec.Op),
		nullStr(rec.Request), s.tenantID, workerID, workflowID, workerID, generation)
	if err != nil {
		return fmt.Errorf("write call intent: step %d: %w", rec.Step, err)
	}
	if n, _ := res.RowsAffected(); n == 0 && workerID != "" {
		return ErrFenceLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("write call intent: commit: %w", err)
	}
	return nil
}

func (s *MySQLStore) CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string, workerID string, generation int64) error {
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
		  AND (? = '' OR EXISTS (
		      SELECT 1 FROM workflow_instances WHERE id = ? AND assigned_to = ? AND generation = ?
		  ))
	`, rec.Response, nullStr(rec.Err), nullStr(string(payload)), checksum,
		workflowID, rec.Step, s.tenantID, workerID, workflowID, workerID, generation)
	if err != nil {
		return fmt.Errorf("complete call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete call intent: rows affected: %w", err)
	}
	if n == 0 {
		// This transaction changed nothing and is never going to commit --
		// every path from here returns an error. Roll back before the
		// disambiguation check below: on MySQL, the EXISTS subquery this
		// UPDATE's WHERE clause just evaluated takes a lock on the
		// workflow_instances row that InnoDB holds until the transaction
		// ends, and intentFenceOrNotPending's Heartbeat call is a second,
		// separate transaction against that same row -- called while this
		// one is still open, it deadlocks against its own lock. Measured:
		// "Error 1205 (HY000): Lock wait timeout exceeded" on
		// TestCompleteCallIntent_FenceLost/mysql before this rollback was
		// added. Postgres's plain (non-locking) subquery reads do not have
		// this problem, and SQL Server's shouldn't under READ COMMITTED
		// either, but the rollback is applied uniformly rather than only
		// where it was caught: it costs nothing on a transaction that was
		// already doomed, and this dialect's locking behavior is unverified
		// (no MSSQL instance was assigned to this stream).
		_ = tx.Rollback()
		if workerID != "" {
			if ferr := intentFenceOrNotPending(ctx, s.Heartbeat, workflowID, workerID, generation); ferr != nil {
				return ferr
			}
		}
	}
	if n != 1 {
		return fmt.Errorf("complete call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// SQL Server
// ---------------------------------------------------------------------------

func (s *MSSQLStore) WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord, workerID string, generation int64) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("write call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Every @pN below is bound exactly once, including workflowID and
	// workerID, which appear twice in the query text (@p1/@p9, @p8/@p10) --
	// this codebase has no existing example of go-mssqldb reusing one @pN
	// across two references in the same statement, and this is not the
	// place to find out; fresh parameter numbers per Go argument, the same
	// way MySQL's positional ? placeholders have to be, is what every other
	// MSSQL query in this file already does.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request,
			created_at, intent_at, tenant_id)
		SELECT @p1, @p2, @p3, @p4, @p5, @p6, SYSUTCDATETIME(), SYSUTCDATETIME(), @p7
		WHERE (@p8 = '' OR EXISTS (
			SELECT 1 FROM workflow_instances WHERE id = @p9 AND assigned_to = @p10 AND generation = @p11
		))
	`, workflowID, rec.Step, string(rec.EventType), nullStr(rec.Service), nullStr(rec.Op),
		nullStr(rec.Request), s.tenantID, workerID, workflowID, workerID, generation)
	if err != nil {
		return fmt.Errorf("write call intent: step %d: %w", rec.Step, err)
	}
	if n, _ := res.RowsAffected(); n == 0 && workerID != "" {
		return ErrFenceLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("write call intent: commit: %w", err)
	}
	return nil
}

func (s *MSSQLStore) CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string, workerID string, generation int64) error {
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
		  AND (@p8 = '' OR EXISTS (
		      SELECT 1 FROM workflow_instances WHERE id = @p9 AND assigned_to = @p10 AND generation = @p11
		  ))
	`, workflowID, rec.Step, rec.Response, nullStr(rec.Err), nullStr(string(payload)),
		checksum, s.tenantID, workerID, workflowID, workerID, generation)
	if err != nil {
		return fmt.Errorf("complete call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete call intent: rows affected: %w", err)
	}
	if n == 0 {
		// This transaction changed nothing and is never going to commit --
		// every path from here returns an error. Roll back before the
		// disambiguation check below: on MySQL, the EXISTS subquery this
		// UPDATE's WHERE clause just evaluated takes a lock on the
		// workflow_instances row that InnoDB holds until the transaction
		// ends, and intentFenceOrNotPending's Heartbeat call is a second,
		// separate transaction against that same row -- called while this
		// one is still open, it deadlocks against its own lock. Measured:
		// "Error 1205 (HY000): Lock wait timeout exceeded" on
		// TestCompleteCallIntent_FenceLost/mysql before this rollback was
		// added. Postgres's plain (non-locking) subquery reads do not have
		// this problem, and SQL Server's shouldn't under READ COMMITTED
		// either, but the rollback is applied uniformly rather than only
		// where it was caught: it costs nothing on a transaction that was
		// already doomed, and this dialect's locking behavior is unverified
		// (no MSSQL instance was assigned to this stream).
		_ = tx.Rollback()
		if workerID != "" {
			if ferr := intentFenceOrNotPending(ctx, s.Heartbeat, workflowID, workerID, generation); ferr != nil {
				return ferr
			}
		}
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
//
// Fenced the same way and for the same reason as CompleteCallIntent (B4):
// the engine performing this replay is itself just another claimant, and can
// itself stall and be reaped mid-resolution. workerID == "" skips the check,
// per callIntentStore's doc.
type callIntentResolver interface {
	ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, workerID string, generation int64) error
}

func (s *PostgresStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, workerID string, generation int64) error {
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
		  AND ($8 = '' OR EXISTS (
		      SELECT 1 FROM workflow_instances WHERE id = $1 AND assigned_to = $8 AND generation = $9
		  ))
	`, workflowID, rec.Step, rec.Response, nullStr(rec.Err), nullStr(string(payload)),
		checksum, s.tenantID, workerID, generation)
	if err != nil {
		return fmt.Errorf("resolve call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve call intent: rows affected: %w", err)
	}
	if n == 0 {
		// See CompleteCallIntent's identical rollback-before-disambiguation
		// comment: same statement shape (an UPDATE whose WHERE clause
		// contains the fence's EXISTS subquery), same MySQL self-deadlock
		// risk if intentFenceOrNotPending's Heartbeat call ran while this
		// transaction were still open.
		_ = tx.Rollback()
		if workerID != "" {
			if ferr := intentFenceOrNotPending(ctx, s.Heartbeat, workflowID, workerID, generation); ferr != nil {
				return ferr
			}
		}
	}
	if n != 1 {
		return fmt.Errorf("resolve call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

func (s *MySQLStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, workerID string, generation int64) error {
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
		  AND (? = '' OR EXISTS (
		      SELECT 1 FROM workflow_instances WHERE id = ? AND assigned_to = ? AND generation = ?
		  ))
	`, rec.Response, nullStr(rec.Err), nullStr(string(payload)), checksum,
		workflowID, rec.Step, s.tenantID, workerID, workflowID, workerID, generation)
	if err != nil {
		return fmt.Errorf("resolve call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve call intent: rows affected: %w", err)
	}
	if n == 0 {
		// See CompleteCallIntent's identical rollback-before-disambiguation
		// comment: same statement shape (an UPDATE whose WHERE clause
		// contains the fence's EXISTS subquery), same MySQL self-deadlock
		// risk if intentFenceOrNotPending's Heartbeat call ran while this
		// transaction were still open.
		_ = tx.Rollback()
		if workerID != "" {
			if ferr := intentFenceOrNotPending(ctx, s.Heartbeat, workflowID, workerID, generation); ferr != nil {
				return ferr
			}
		}
	}
	if n != 1 {
		return fmt.Errorf("resolve call intent: step %d: %w (%d rows matched)", rec.Step, errIntentNotPending, n)
	}
	return tx.Commit()
}

func (s *MSSQLStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, workerID string, generation int64) error {
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
		  AND (@p8 = '' OR EXISTS (
		      SELECT 1 FROM workflow_instances WHERE id = @p9 AND assigned_to = @p10 AND generation = @p11
		  ))
	`, workflowID, rec.Step, rec.Response, nullStr(rec.Err), nullStr(string(payload)),
		checksum, s.tenantID, workerID, workflowID, workerID, generation)
	if err != nil {
		return fmt.Errorf("resolve call intent: step %d: %w", rec.Step, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve call intent: rows affected: %w", err)
	}
	if n == 0 {
		// See CompleteCallIntent's identical rollback-before-disambiguation
		// comment: same statement shape (an UPDATE whose WHERE clause
		// contains the fence's EXISTS subquery), same MySQL self-deadlock
		// risk if intentFenceOrNotPending's Heartbeat call ran while this
		// transaction were still open.
		_ = tx.Rollback()
		if workerID != "" {
			if ferr := intentFenceOrNotPending(ctx, s.Heartbeat, workflowID, workerID, generation); ferr != nil {
				return ferr
			}
		}
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
