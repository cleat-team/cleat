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
	"database/sql"
	"fmt"
	"sort"
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
// instance rather than assumed. An empty $8 is the "fencing not requested"
// escape hatch; see callIntentStore's doc.
// THE THREE INTENT INSERTS BELOW DO NOT AGREE ABOUT payload_encoding, AND THAT
// IS CORRECT RATHER THAN AN OVERSIGHT. #1383 routed WriteCallIntent through
// encodeEventForStorage on PostgreSQL ONLY, matching #1380's scope, so:
//
//	postgres   binds stored.Encoding -- the request is base64 now
//	mysql      literal 0 -- still nullStr(rec.Request), genuinely plaintext
//	mssql      literal 0 -- likewise
//
// Recording a literal 0 on the postgres arm was correct when written and became
// WRONG the moment #1383 landed, in a way worse than the NULL it replaced: a
// NULL sends decodePayload to tryDecodeBase64, which guesses, whereas an explicit
// 0 over base64 bytes does not guess -- it hands the caller the base64 TEXT,
// deterministically, on every read. Falsified rather than reasoned about:
// reverting the postgres arm to the literal round-trips "true" as "dHJ1ZQ==",
// while mysql and mssql pass, which is the split above stated as a test result.
//
// The structural guard cannot see this. It asserts the column is NAMED, and it
// was. Only the behavioural round trip can tell a truthful encoding from a lie
// about one, which is why that test writes a request chosen to be valid base64.
//
// # WHY THIS COLUMN IS LOAD BEARING HERE AND NOWHERE ELSE
//
// An intent row writes no `payload` column. Every other writer populates
// `payload`, whose JSON carries request_b64/response_b64 and whose
// populateFromPayload runs AFTER the scanned columns, so those rows are shadowed
// and the column disagreeing with them is harmless. Intent rows are not
// shadowed: until CompleteCallIntent fills `payload` in, the request column is
// the only copy, and before this the reader guessed at its encoding.
//
// Measured on all three dialects, writing an intent and loading it back, when
// the request was stored raw and the reader guessed:
//
//	"true"    -> "\xb6\xbb\x9e"   "1234" -> "\xd7m\xf8"
//	"null"    -> "\x9e\xe9e"       {"a":1} -> intact
//
// JSON objects are safe because `{` and `"` are not in the base64 alphabet. A
// JSON SCALAR request is not: `true` and `null` are four characters drawn
// entirely from it. And these rows are what the ambiguity resolver reads after a
// crash, so the corruption surfaced exactly during recovery.
//
// CompleteCallIntent leaves its 0 alone on purpose: it writes rec.Response raw
// on every dialect, so plaintext stays the truthful answer for that row.
const writeCallIntentSQLPostgres = `
	INSERT INTO event_history (workflow_id, step, event_type, service, operation, request,
		created_at, intent_at, tenant_id, payload_encoding)
	SELECT $1, $2, $3, $4, $5, $6, now(), now(), $7, $10
	WHERE ($8 = '' OR EXISTS (
		SELECT 1 FROM workflow_instances WHERE id = $1 AND assigned_to = $8 AND generation = $9
	))`

func (s *PostgresStore) WriteCallIntent(ctx context.Context, workflowID string, rec EventRecord, workerID string, generation int64) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("write call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The same encoding every other writer uses. Until #1379 this path bound
	// rec.Request RAW, while every INSERT path base64-encodes it and every
	// read path applies tryDecodeBase64 -- which falls back to the raw string
	// only when decoding FAILS, so a raw request that happens to be valid
	// base64 decoded to the wrong bytes (cleat#1319, six of nine ordinary
	// short values). And it did not encrypt, so --encrypt-sensitive-payloads
	// left every write-ahead intent's request in the clear.
	stored, err := encodeEventForStorage(rec, s.encryption, s.encryptSensitivePayloads)
	if err != nil {
		return fmt.Errorf("write call intent: step %d: %w", rec.Step, err)
	}

	res, err := tx.ExecContext(ctx, writeCallIntentSQLPostgres,
		workflowID, rec.Step, rec.EventType, nullStr(rec.Service), nullStr(rec.Op),
		nullStr(stored.Request), s.tenantID, workerID, generation, stored.Encoding)
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

// WHY response IS BOUND THROUGH nullStr HERE AND IN ResolveCallIntent, on all
// three dialects. cleat#1379 part 1.
//
// It used to be bound raw, so a call that completed with an EMPTY response
// stored the empty string where every INSERT path stores NULL. That is not a
// cosmetic difference: engine/flush.go's insertEventSQL carries
//
//	ON CONFLICT (workflow_id, step) DO UPDATE
//	  SET response = EXCLUDED.response, error = EXCLUDED.error
//	  WHERE event_history.response = '' AND event_history.error IS NULL
//
// whose stated purpose is to COMPLETE a row that was written without a result
// while leaving finished rows immutable. A row written without a result has
// response NULL, and `NULL = ”` is not true -- so the clause could never fire
// for the rows it was written for. The only rows it COULD fire on were
// finished call-intent rows that completed with an empty response, which are
// exactly the rows it was meant to leave alone. The behaviour was inverted
// relative to its own comment.
//
// Measured on PostgreSQL 16 before the change, completing an intent with an
// empty response and no error and then appending the same step again with a
// different one:
//
//	after Complete (empty, no err)  response=""      checksum="chk2"  payload={... no response_b64 ...}
//	after re-append w/ response     response="eyJs…" checksum="chk2"  payload=UNCHANGED
//	VerifyWorkflowEvents -> checksum mismatch (expected chk2, got f54d6dfd…)
//
// The DO UPDATE fired, overwrote the column, and left the payload and the
// checksum alone -- so the row's displayed response disagreed with the payload
// replay reads, and the workflow was permanently unverifiable. `response` is
// not in shadowFields (it is redacted and encrypted, so it cannot be compared),
// which is why verifyShadowColumns cannot see that divergence and only the
// checksum catches it.
//
// So the firing this closes was never self-healing; it could only corrupt.
// With NULL the clause is false for these rows too, which is what
// engine/flush.go's comment has always claimed it is for every row.
func (s *PostgresStore) CompleteCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, checksum string, workerID string, generation int64) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("complete call intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// See WriteCallIntent: one encoding for every writer. The payload and the
	// checksum are the CALLER's, built from the plaintext record, and stay
	// that way -- encodePayloadForStorage encrypts what it is given rather
	// than rebuilding it, so the checksum the caller computed still matches
	// what VerifyWorkflowEvents recomputes from the decrypted row.
	stored, err := encodeEventForStorage(rec, s.encryption, s.encryptSensitivePayloads)
	if err != nil {
		return fmt.Errorf("complete call intent: step %d: %w", rec.Step, err)
	}
	storedPayload, err := encodePayloadForStorage(string(payload), s.encryption, s.encryptSensitivePayloads)
	if err != nil {
		return fmt.Errorf("complete call intent: step %d: %w", rec.Step, err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE event_history
		SET response = $3, error = $4, payload = $5, checksum = $6, intent_at = NULL
		WHERE workflow_id = $1 AND step = $2 AND tenant_id = $7
		  AND intent_at IS NOT NULL AND checksum IS NULL
		  AND ($8 = '' OR EXISTS (
		      SELECT 1 FROM workflow_instances WHERE id = $1 AND assigned_to = $8 AND generation = $9
		  ))
	`, workflowID, rec.Step, nullStr(stored.Response), nullStr(stored.Err), storedPayload,
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
			created_at, intent_at, tenant_id, payload_encoding)
		SELECT ?, ?, ?, ?, ?, ?, NOW(6), NOW(6), ?, 0
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
	`, nullStr(rec.Response), nullStr(rec.Err), nullStr(string(payload)), checksum,
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
			created_at, intent_at, tenant_id, payload_encoding)
		SELECT @p1, @p2, @p3, @p4, @p5, @p6, SYSUTCDATETIME(), SYSUTCDATETIME(), @p7, 0
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
	`, workflowID, rec.Step, nullStr(rec.Response), nullStr(rec.Err), nullStr(string(payload)),
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
//
// The later parameter is what keeps the chain verifiable, and it is not
// optional on any path that can have events above the resolved row. See
// chainRepairsAfter.
type callIntentResolver interface {
	ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, workerID string, generation int64, later []EventRecord) error
}

// chainRepairsAfter returns the events stored above step, in step order, that
// have to have their checksums rewritten when step is resolved in place.
//
// Resolving gives a pending row a checksum it did not have, and every row
// already stored above it was chained on its *absence*: previousStoredChecksum
// reads the immediately preceding row and gets "" for a pending one, which is
// exactly what VerifyWorkflowEvents does when it walks the history and resets
// on a missing checksum. Writing the resolved row's checksum without rewriting
// theirs leaves a history that fails verification at the very next row --
// reported as tampering, and fatal on a worker, which runs the verifier with
// failOnChecksumMismatch. IMPROVEMENT-PLAN 3.89.
//
// It stops at the first pending row above step, because that row is itself a
// reset point: everything above it is already chained on "" and stays correct.
//
// The common case returns nothing. A pending row is normally the last row --
// the crash that made it pending stopped the workflow there -- and then this
// is empty and the resolve is a single UPDATE, as it was before. It is the
// operator paths that break that assumption: force-fail and force-complete
// append an audit event above the pending row, and a signal delivered to a
// stopped workflow lands above it too.
func chainRepairsAfter(history []EventRecord, step int) []EventRecord {
	var above []EventRecord
	for _, rec := range history {
		if rec.Step > step {
			above = append(above, rec)
		}
	}
	// Sorted rather than assumed: the break below is only correct in step
	// order, and every caller happens to pass a LoadEventHistory result that
	// is already ordered. Depending on that silently is how the next caller
	// gets it wrong.
	sort.Slice(above, func(i, j int) bool { return above[i].Step < above[j].Step })
	for i, rec := range above {
		if rec.isPendingIntent() {
			return above[:i]
		}
	}
	return above
}

func (s *PostgresStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, workerID string, generation int64, later []EventRecord) error {
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

	// See WriteCallIntent: one encoding for every writer. The payload and the
	// checksum are the CALLER's, built from the plaintext record, and stay
	// that way -- encodePayloadForStorage encrypts what it is given rather
	// than rebuilding it, so the checksum the caller computed still matches
	// what VerifyWorkflowEvents recomputes from the decrypted row.
	stored, err := encodeEventForStorage(rec, s.encryption, s.encryptSensitivePayloads)
	if err != nil {
		return fmt.Errorf("resolve call intent: step %d: %w", rec.Step, err)
	}
	storedPayload, err := encodePayloadForStorage(string(payload), s.encryption, s.encryptSensitivePayloads)
	if err != nil {
		return fmt.Errorf("resolve call intent: step %d: %w", rec.Step, err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE event_history
		SET response = $3, error = $4, payload = $5, checksum = $6, intent_at = NULL
		WHERE workflow_id = $1 AND step = $2 AND tenant_id = $7
		  AND intent_at IS NOT NULL AND checksum IS NULL
		  AND ($8 = '' OR EXISTS (
		      SELECT 1 FROM workflow_instances WHERE id = $1 AND assigned_to = $8 AND generation = $9
		  ))
	`, workflowID, rec.Step, nullStr(stored.Response), nullStr(stored.Err), storedPayload,
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
	// In the same transaction as the resolve, so a crash between them cannot
	// leave a history that fails verification.
	if err := s.repairChainAfterResolve(ctx, tx, workflowID, checksum, later); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MySQLStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, workerID string, generation int64, later []EventRecord) error {
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
	`, nullStr(rec.Response), nullStr(rec.Err), nullStr(string(payload)), checksum,
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
	// In the same transaction as the resolve, so a crash between them cannot
	// leave a history that fails verification.
	if err := s.repairChainAfterResolve(ctx, tx, workflowID, checksum, later); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MSSQLStore) ResolveCallIntent(ctx context.Context, workflowID string, rec EventRecord, payload []byte, workerID string, generation int64, later []EventRecord) error {
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
	`, workflowID, rec.Step, nullStr(rec.Response), nullStr(rec.Err), nullStr(string(payload)),
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
	// In the same transaction as the resolve, so a crash between them cannot
	// leave a history that fails verification.
	if err := s.repairChainAfterResolve(ctx, tx, workflowID, checksum, later); err != nil {
		return err
	}
	return tx.Commit()
}

// repairChainAfterResolve rewrites the stored checksums of the events above a
// row that was just resolved in place, so the chain the verifier walks matches
// the chain the writer left behind. chain is the checksum just written to the
// resolved row; each subsequent event is re-chained onto it in step order.
//
// Three near-identical bodies rather than one, because the placeholder syntax
// is the only thing that differs and the repo keeps its SQL beside the dialect
// that speaks it. The loop is one UPDATE per row and runs only on the operator
// paths -- see chainRepairsAfter for why the ordinary case passes nothing.
func (s *PostgresStore) repairChainAfterResolve(ctx context.Context, tx *sql.Tx, workflowID, chain string, later []EventRecord) error {
	for _, rec := range later {
		chain = computeEventChecksum(rec, chain)
		if _, err := tx.ExecContext(ctx, `
			UPDATE event_history SET checksum = $1
			WHERE workflow_id = $2 AND step = $3 AND tenant_id = $4
		`, chain, workflowID, rec.Step, s.tenantID); err != nil {
			return fmt.Errorf("resolve call intent: repair chain at step %d: %w", rec.Step, err)
		}
	}
	return nil
}

func (s *MySQLStore) repairChainAfterResolve(ctx context.Context, tx *sql.Tx, workflowID, chain string, later []EventRecord) error {
	for _, rec := range later {
		chain = computeEventChecksum(rec, chain)
		if _, err := tx.ExecContext(ctx, `
			UPDATE event_history SET checksum = ?
			WHERE workflow_id = ? AND step = ? AND tenant_id = ?
		`, chain, workflowID, rec.Step, s.tenantID); err != nil {
			return fmt.Errorf("resolve call intent: repair chain at step %d: %w", rec.Step, err)
		}
	}
	return nil
}

func (s *MSSQLStore) repairChainAfterResolve(ctx context.Context, tx *sql.Tx, workflowID, chain string, later []EventRecord) error {
	for _, rec := range later {
		chain = computeEventChecksum(rec, chain)
		if _, err := tx.ExecContext(ctx, `
			UPDATE event_history SET checksum = @p1
			WHERE workflow_id = @p2 AND step = @p3 AND tenant_id = @p4
		`, chain, workflowID, rec.Step, s.tenantID); err != nil {
			return fmt.Errorf("resolve call intent: repair chain at step %d: %w", rec.Step, err)
		}
	}
	return nil
}

var (
	_ callIntentResolver = (*PostgresStore)(nil)
	_ callIntentResolver = (*MySQLStore)(nil)
	_ callIntentResolver = (*MSSQLStore)(nil)
)
