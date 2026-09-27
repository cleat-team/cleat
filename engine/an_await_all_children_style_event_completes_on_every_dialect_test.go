package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAnAwaitAllChildrenStyleEventCompletesOnEveryDialect closes cleat#2333's
// review checklist item for AwaitAllChildren -- the third of the two-phase
// event types alongside AwaitChild and AwaitPromise, and the only one with no
// direct storage-level completion/immutability test until now. It mirrors
// TestAnAwaitStyleEventCompletesOnEveryDialect (AwaitChild) and
// TestAPromiseStyleEventCompletesOnEveryDialect (AwaitPromise): a pending
// write with everything empty, then a completing write to the SAME step, and
// an immutability check against a stray re-flush.
//
// AwaitAllChildren needs no equivalent of the event_type-transition fix
// (AwaitPromise) or nonEmptyChildError (AwaitChild's error path): its
// completing write always marshals a []ChildOutcome slice into Response
// (children.go's freshAwaitAllChildren/replayAwaitAllChildren), and
// json.Marshal of a slice is "[]" at its shortest, never "" -- so the
// completion guard's response-IS-NULL half can never be fooled by a
// genuinely-completed row here. This test's completed Response uses a
// realistic marshaled-outcomes shape to confirm that in practice, on all
// three dialects, rather than only by reading the code.
func TestAnAwaitAllChildrenStyleEventCompletesOnEveryDialect(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()
		runAwaitAllChildrenStyleCompletionCase(t, testutil.DialectPostgres, db, "await-all-complete-pg")
	})

	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		runAwaitAllChildrenStyleCompletionCase(t, testutil.DialectMSSQL, db, "await-all-complete-mssql")
	})

	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		runAwaitAllChildrenStyleCompletionCase(t, testutil.DialectMySQL, db, "await-all-complete-mysql")
	})
}

func runAwaitAllChildrenStyleCompletionCase(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) {
	t.Helper()
	seedWorkflowInstance(t, db, dialect, wfID)

	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(store),
		WithDB(db))

	// Suspend: the shape freshAwaitAllChildren's "not all done yet" branch
	// records (children.go) -- everything empty.
	pending := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitAllChildren,
		TimestampMs: time.Now().UnixMilli(),
	}
	if err := eng.flushEvent(context.Background(), wfID, pending, ""); err != nil {
		t.Fatalf("suspend flush failed on %s: %v", dialect, err)
	}

	// Complete: same step, a marshaled []ChildOutcome -- the shape
	// freshAwaitAllChildren/replayAwaitAllChildren record once every child has
	// settled.
	completed := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitAllChildren,
		Response:    `[{"run_id":"child-1","completed":true,"result":"{\"ok\":true}"},{"run_id":"child-2","completed":true,"failed":true,"error":"boom"}]`,
		TimestampMs: pending.TimestampMs,
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

	gotResponse := loadAwaitResponse(t, store, wfID)
	if gotResponse != completed.Response {
		t.Errorf("%s: stored response = %q, want %q -- the completing flush did not take effect", dialect, gotResponse, completed.Response)
	}
	gotChecksum := readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Errorf("%s: stored checksum = %s, want %s (recomputed from the COMPLETED record)", dialect, gotChecksum, wantChecksum)
	}

	// Immutability: a stray re-flush of the same step with a DIFFERENT
	// outcomes list must not overwrite an already-completed result.
	again := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitAllChildren,
		Response:    `[{"run_id":"child-1","completed":true,"result":"{\"ok\":false}"}]`,
		TimestampMs: pending.TimestampMs,
	}
	if err := eng.flushEvent(context.Background(), wfID, again, ""); err != nil {
		t.Fatalf("re-flush of a completed row failed on %s: %v", dialect, err)
	}
	gotResponse = loadAwaitResponse(t, store, wfID)
	if gotResponse != completed.Response {
		t.Errorf("%s: a completed row was overwritten by a stray re-flush: response = %q, want the original %q", dialect, gotResponse, completed.Response)
	}
	gotChecksum = readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Errorf("%s: a completed row's checksum changed after a stray re-flush: %s, want the original %s", dialect, gotChecksum, wantChecksum)
	}
}
