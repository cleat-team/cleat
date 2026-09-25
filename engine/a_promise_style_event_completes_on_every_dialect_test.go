package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAPromiseStyleEventCompletesOnEveryDialect is cleat#2333's regression
// test for AwaitPromise, found while checking cleat-review's point (4) on
// that issue. AwaitPromise (engine/promises.go) is the same
// suspend-then-complete same-step write AwaitChild is -- a pending
// EventTypeAwaitPromise row with everything empty, then, once the promise
// resolves, a second write to the SAME step, EventTypePromiseResolved, whose
// outcome lives in PromiseResult rather than Response.
//
// Response/Err stay empty on BOTH writes for this event, so a completion
// guard that only inspected response/error -- cleat#2333's original fix for
// AwaitChild -- would read a completed AwaitPromise row as still-pending
// forever: the DO UPDATE / ON DUPLICATE KEY UPDATE / MERGE's WHEN MATCHED
// clause would never apply, and the raw promise_result column would sit at
// its pending (NULL) value forever, out of sync with the payload blob
// (payload alone stays correct via populateFromPayload's overlay -- see
// LoadEventHistory -- but any reader of the raw column would not).
//
// This also exercises the immutability side the same way
// TestAnAwaitStyleEventCompletesOnEveryDialect does for AwaitChild: unlike
// AwaitChild, whose completed Response is guaranteed non-empty
// (GetChildResult COALESCEs/ISNULLs to at least "{}" on all three
// dialects), a hand-called resolve_promise(id, value) can legitimately pass
// a non-empty value that this test now confirms survives a stray reflush --
// and, because response/error/promise_result/promise_error are ALL empty on
// AwaitPromise's own pending write, this is the case that also needed the
// guard restricted to await_child/await_promise/await_all_children (see
// flush.go's insertEventSQL doc): without that restriction this test would
// still pass, but a call intent completed with a genuinely empty response
// would not stay immutable -- TestACompletedIntentIsNotOverwrittenByALaterAppend
// is what catches that one.
func TestAPromiseStyleEventCompletesOnEveryDialect(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()
		runPromiseStyleCompletionCase(t, testutil.DialectPostgres, db, "promise-complete-pg")
	})

	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		runPromiseStyleCompletionCase(t, testutil.DialectMSSQL, db, "promise-complete-mssql")
	})

	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		runPromiseStyleCompletionCase(t, testutil.DialectMySQL, db, "promise-complete-mysql")
	})
}

func runPromiseStyleCompletionCase(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) {
	t.Helper()
	seedWorkflowInstance(t, db, dialect, wfID)

	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(store),
		WithDB(db))

	promiseID := "promise-1"

	// Suspend: the shape AwaitPromise's "still pending" branch records.
	pending := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitPromise,
		TimestampMs: time.Now().UnixMilli(),
	}
	if err := eng.flushEvent(context.Background(), wfID, pending, ""); err != nil {
		t.Fatalf("suspend flush failed on %s: %v", dialect, err)
	}

	// Complete: same step, EventTypePromiseResolved, the shape AwaitPromise's
	// (and ResolvePromise's) resolved branch records once the promise store
	// reports "resolved".
	completed := EventRecord{
		Step:          0,
		EventType:     EventTypePromiseResolved,
		PromiseID:     promiseID,
		PromiseResult: `{"answer":42}`,
		TimestampMs:   pending.TimestampMs,
	}
	wantChecksum := computeEventChecksum(completed, "")
	if err := eng.flushEvent(context.Background(), wfID, completed, ""); err != nil {
		t.Fatalf("completing flush failed on %s: %v", dialect, err)
	}

	var n int
	if err := testutil.AdminDB(t, db, dialect).QueryRow(countEventsSQL(dialect), wfID).Scan(&n); err != nil {
		t.Fatalf("counting events on %s: %v", dialect, err)
	}
	if n != 1 {
		t.Fatalf("event_history has %d rows on %s after suspend+complete, want 1 (a completing flush must overwrite the pending row, not append a second one)", n, dialect)
	}

	gotResult := loadPromiseResult(t, store, wfID)
	if gotResult != completed.PromiseResult {
		t.Errorf("%s: stored promise_result = %q, want %q -- the completing flush did not take effect (promise_result is not in the completion guard's SET/IF/UPDATE list)", dialect, gotResult, completed.PromiseResult)
	}
	gotChecksum := readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Errorf("%s: stored checksum = %s, want %s (recomputed from the COMPLETED record) -- a stale (pending) checksum here is cleat#2333's failure mode, reached via promise_result instead of response", dialect, gotChecksum, wantChecksum)
	}
	if gotType := loadEventType(t, store, wfID); gotType != EventTypePromiseResolved {
		t.Errorf("%s: stored event_type = %q after completion, want %q -- none of the six write sites' SET/UPDATE lists assigned event_type until cleat#2333's fix, so a completed promise's row stayed 'await_promise' forever and promises.go's own replay (which branches on this column) re-checked the promise store on every replay instead of trusting history", dialect, gotType, EventTypePromiseResolved)
	}

	// Immutability: a stray re-flush of the same step with a DIFFERENT
	// promise_result must not overwrite an already-resolved promise. Unlike
	// AwaitChild's Response, PromiseResult has no COALESCE-to-non-empty
	// guarantee -- a hand-called resolve_promise(id, "") would be
	// indistinguishable from pending -- but this test's completed value is
	// non-empty, which is the common case and the one the guard is
	// specified to protect.
	again := EventRecord{
		Step:          0,
		EventType:     EventTypePromiseResolved,
		PromiseID:     promiseID,
		PromiseResult: `{"answer":"a different one"}`,
		TimestampMs:   pending.TimestampMs,
	}
	if err := eng.flushEvent(context.Background(), wfID, again, ""); err != nil {
		t.Fatalf("re-flush of a completed row failed on %s: %v", dialect, err)
	}
	gotResult = loadPromiseResult(t, store, wfID)
	if gotResult != completed.PromiseResult {
		t.Errorf("%s: a completed row was overwritten by a stray re-flush: promise_result = %q, want the original %q", dialect, gotResult, completed.PromiseResult)
	}
	gotChecksum = readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Errorf("%s: a completed row's checksum changed after a stray re-flush: %s, want the original %s", dialect, gotChecksum, wantChecksum)
	}
}

// loadPromiseResult mirrors loadAwaitResponse (an_await_style_event...test.go)
// for the promise_result column instead of response.
func loadPromiseResult(t *testing.T, store WorkflowStore, wfID string) string {
	t.Helper()
	history, err := store.LoadEventHistory(context.Background(), wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("LoadEventHistory returned %d events, want 1", len(history))
	}
	return history[0].PromiseResult
}

// loadEventType mirrors loadPromiseResult, reading the stored event_type
// instead of promise_result.
func loadEventType(t *testing.T, store WorkflowStore, wfID string) EventType {
	t.Helper()
	history, err := store.LoadEventHistory(context.Background(), wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("LoadEventHistory returned %d events, want 1", len(history))
	}
	return history[0].EventType
}

// TestAPromiseResolvedWithEmptyStringStaysImmutable is cleat-review's follow-up
// on cleat#2333, point 4: within the two-phase event types, can a COMPLETED
// row still look pending? PromiseResult has no COALESCE-to-non-empty
// guarantee the way AwaitChild's Response does (GetChildResult COALESCEs/
// ISNULLs to at least "{}" on all three dialects) -- a hand-called
// resolve_promise(id, "") stores NULL in promise_result via nullStr, which is
// bit-for-bit identical, across response/error/promise_result/promise_error,
// to AwaitPromise's own pending row.
//
// Before cleat#2333's event_type fix (this file's sibling test, and the
// SET/ON DUPLICATE/WHEN MATCHED lists in flush.go, mysql_events.go and
// mssql_events.go), this row would still read event_type = 'await_promise'
// after "completing", so the pending guard
// (event_type IN ('await_child','await_promise','await_all_children') AND
// response/error/promise_result/promise_error all NULL) would match it
// forever and a later stray reflush would silently corrupt an
// already-resolved promise whose result happens to be "".
//
// event_type transitioning to 'promise_resolved' on completion is what closes
// this: that value is not in the guard's IN (...) list, so @cleat_pending
// (mysql) / the WHERE clause (postgres) / WHEN MATCHED's guard (mssql) all
// evaluate false on any later attempt regardless of what promise_result
// holds -- empty, NULL, or real content. This test would fail without that
// transition even though it never inspects event_type directly: the reflush
// below would succeed and the checksum/promise_result assertions would catch
// the corruption.
func TestAPromiseResolvedWithEmptyStringStaysImmutable(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()
		runPromiseResolvedEmptyStringCase(t, testutil.DialectPostgres, db, "promise-empty-pg")
	})

	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		runPromiseResolvedEmptyStringCase(t, testutil.DialectMSSQL, db, "promise-empty-mssql")
	})

	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		runPromiseResolvedEmptyStringCase(t, testutil.DialectMySQL, db, "promise-empty-mysql")
	})
}

func runPromiseResolvedEmptyStringCase(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) {
	t.Helper()
	seedWorkflowInstance(t, db, dialect, wfID)

	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(store),
		WithDB(db))

	promiseID := "promise-empty-1"

	// Suspend: identical shape to the sibling test.
	pending := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitPromise,
		TimestampMs: time.Now().UnixMilli(),
	}
	if err := eng.flushEvent(context.Background(), wfID, pending, ""); err != nil {
		t.Fatalf("suspend flush failed on %s: %v", dialect, err)
	}

	// Complete with the EMPTY string. nullStr stores this as SQL NULL, same
	// as the pending row's own promise_result -- only event_type
	// distinguishes this row from still-pending.
	completed := EventRecord{
		Step:          0,
		EventType:     EventTypePromiseResolved,
		PromiseID:     promiseID,
		PromiseResult: "",
		TimestampMs:   pending.TimestampMs,
	}
	wantChecksum := computeEventChecksum(completed, "")
	if err := eng.flushEvent(context.Background(), wfID, completed, ""); err != nil {
		t.Fatalf("completing flush (empty result) failed on %s: %v", dialect, err)
	}

	if gotType := loadEventType(t, store, wfID); gotType != EventTypePromiseResolved {
		t.Fatalf("%s: stored event_type = %q after an empty-result completion, want %q -- without this transition the row is indistinguishable from pending by every other column", dialect, gotType, EventTypePromiseResolved)
	}
	gotChecksum := readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Fatalf("%s: stored checksum = %s, want %s after the empty-result completion", dialect, gotChecksum, wantChecksum)
	}

	// Reflush the same step with DIFFERENT, non-empty content. If the row
	// were still (mis)read as pending, this would silently succeed and
	// corrupt an already-resolved promise -- exactly the scenario
	// cleat-review's follow-up asked to be settled before the PR opens.
	again := EventRecord{
		Step:          0,
		EventType:     EventTypePromiseResolved,
		PromiseID:     promiseID,
		PromiseResult: `{"should":"never land"}`,
		TimestampMs:   pending.TimestampMs,
	}
	if err := eng.flushEvent(context.Background(), wfID, again, ""); err != nil {
		t.Fatalf("re-flush of a completed (empty-result) row failed on %s: %v", dialect, err)
	}

	gotResult := loadPromiseResult(t, store, wfID)
	if gotResult != "" {
		t.Errorf("%s: a completed row resolved with \"\" was overwritten by a stray re-flush: promise_result = %q, want the original empty string", dialect, gotResult)
	}
	gotChecksum = readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Errorf("%s: a completed (empty-result) row's checksum changed after a stray re-flush: %s, want the original %s", dialect, gotChecksum, wantChecksum)
	}
}

// TestAPromiseRejectedWithEmptyStringStaysImmutable mirrors
// TestAPromiseResolvedWithEmptyStringStaysImmutable for the rejection path.
// promises.go's RejectPromise (and AwaitPromise's own timeout/rejection
// branch) record EventTypePromiseRejected with the outcome in PromiseError
// rather than PromiseResult -- a hand-called reject with an empty message is
// exactly as reachable as resolving with one, and event_type transitioning
// away from 'await_promise' is what protects it for the same reason.
func TestAPromiseRejectedWithEmptyStringStaysImmutable(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()
		runPromiseRejectedEmptyStringCase(t, testutil.DialectPostgres, db, "promise-rej-empty-pg")
	})

	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		runPromiseRejectedEmptyStringCase(t, testutil.DialectMSSQL, db, "promise-rej-empty-mssql")
	})

	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		runPromiseRejectedEmptyStringCase(t, testutil.DialectMySQL, db, "promise-rej-empty-mysql")
	})
}

func runPromiseRejectedEmptyStringCase(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) {
	t.Helper()
	seedWorkflowInstance(t, db, dialect, wfID)

	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(store),
		WithDB(db))

	promiseID := "promise-rej-empty-1"

	pending := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitPromise,
		TimestampMs: time.Now().UnixMilli(),
	}
	if err := eng.flushEvent(context.Background(), wfID, pending, ""); err != nil {
		t.Fatalf("suspend flush failed on %s: %v", dialect, err)
	}

	// Reject with the EMPTY string: PromiseError == "", indistinguishable
	// from the pending row's own NULL promise_error by column content alone.
	completed := EventRecord{
		Step:         0,
		EventType:    EventTypePromiseRejected,
		PromiseID:    promiseID,
		PromiseError: "",
		TimestampMs:  pending.TimestampMs,
	}
	wantChecksum := computeEventChecksum(completed, "")
	if err := eng.flushEvent(context.Background(), wfID, completed, ""); err != nil {
		t.Fatalf("completing flush (empty rejection) failed on %s: %v", dialect, err)
	}

	if gotType := loadEventType(t, store, wfID); gotType != EventTypePromiseRejected {
		t.Fatalf("%s: stored event_type = %q after an empty-error rejection, want %q -- without this transition the row is indistinguishable from pending by every other column", dialect, gotType, EventTypePromiseRejected)
	}
	gotChecksum := readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Fatalf("%s: stored checksum = %s, want %s after the empty-error rejection", dialect, gotChecksum, wantChecksum)
	}

	// Reflush the same step with DIFFERENT, non-empty content. A misread
	// "still pending" row would silently accept this and corrupt an
	// already-rejected promise.
	again := EventRecord{
		Step:         0,
		EventType:    EventTypePromiseRejected,
		PromiseID:    promiseID,
		PromiseError: "should never land",
		TimestampMs:  pending.TimestampMs,
	}
	if err := eng.flushEvent(context.Background(), wfID, again, ""); err != nil {
		t.Fatalf("re-flush of a completed (empty-error) row failed on %s: %v", dialect, err)
	}

	history, err := store.LoadEventHistory(context.Background(), wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("LoadEventHistory returned %d events, want 1", len(history))
	}
	if history[0].PromiseError != "" {
		t.Errorf("%s: a completed row rejected with \"\" was overwritten by a stray re-flush: promise_error = %q, want the original empty string", dialect, history[0].PromiseError)
	}
	gotChecksum = readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Errorf("%s: a completed (empty-error) row's checksum changed after a stray re-flush: %s, want the original %s", dialect, gotChecksum, wantChecksum)
	}
}
