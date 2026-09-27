package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAnAwaitStyleEventCompletesOnEveryDialect is cleat#2333's regression test
// at the storage layer: AwaitChild (and AwaitAnyChild, AwaitPromise) suspend by
// flushing a pending event -- response and error both empty -- and complete by
// flushing the SAME step again once the result is known, through recordEvent
// calling flushEvent twice. Unlike a DurableCall, there is no
// CompleteCallIntent-style direct UPDATE backing this: the completing flush
// IS the only place the real response ever reaches event_history.
//
// Before this fix, the completing flush was silently discarded on all three
// dialects: Postgres's ON CONFLICT WHERE clause compared a NULL response
// against the empty string (never true, so the DO UPDATE never ran), and
// MySQL's INSERT IGNORE / MSSQL's WHERE NOT EXISTS drop a re-insert
// unconditionally. The row stayed stuck at its pending checksum forever, so a
// later step's checksum -- chained, in the worker's memory, from the
// completed record -- could never be reproduced by VerifyWorkflowEvents on the
// next replay. See tests/crash's TestChecksumChainSurvivesAwaitChild for the
// end-to-end version of this against a real cleat-worker.
func TestAnAwaitStyleEventCompletesOnEveryDialect(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()
		runAwaitStyleCompletionCase(t, testutil.DialectPostgres, db, "await-complete-pg")
	})

	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		runAwaitStyleCompletionCase(t, testutil.DialectMSSQL, db, "await-complete-mssql")
	})

	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		runAwaitStyleCompletionCase(t, testutil.DialectMySQL, db, "await-complete-mysql")
	})
}

func runAwaitStyleCompletionCase(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) {
	t.Helper()
	seedWorkflowInstance(t, db, dialect, wfID)

	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(store),
		WithDB(db))

	runID := "child-run-1"

	// Suspend: the same shape AwaitChild's "child not completed" branch
	// records (children.go).
	pending := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitChild,
		RunID:       runID,
		TimestampMs: time.Now().UnixMilli(),
	}
	if err := eng.flushEvent(context.Background(), wfID, pending, ""); err != nil {
		t.Fatalf("suspend flush failed on %s: %v", dialect, err)
	}

	// Complete: same step, now with a real response -- the shape AwaitChild's
	// "out.Completed" branch records on the replay that finds the child done.
	completed := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitChild,
		RunID:       runID,
		Response:    `{"child":"done"}`,
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
		t.Errorf("%s: stored checksum = %s, want %s (recomputed from the COMPLETED record) -- "+
			"a stale (pending) checksum here is exactly cleat#2333: a later step chains from the "+
			"in-memory completed checksum, but VerifyWorkflowEvents recomputes from what is stored",
			dialect, gotChecksum, wantChecksum)
	}

	// A completed row must stay immutable: a stray re-flush of the same step
	// (a retried batch, a duplicate segment append) must not overwrite an
	// already-completed result. This is cleat#1379's corruption case,
	// unaffected by cleat#2333's widened guard because it only widened the
	// PENDING side of the condition.
	again := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitChild,
		RunID:       runID,
		Response:    `{"child":"a different answer"}`,
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

// loadAwaitResponse reads the step-0 event back through the store's own
// LoadEventHistory, so the comparison is against the decoded record -- what a
// replay actually sees -- rather than the raw (base64, possibly encrypted)
// column.
func loadAwaitResponse(t *testing.T, store WorkflowStore, wfID string) string {
	t.Helper()
	history, err := store.LoadEventHistory(context.Background(), wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("LoadEventHistory returned %d events, want 1", len(history))
	}
	return history[0].Response
}

func readAwaitChecksum(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) string {
	t.Helper()
	var q string
	switch dialect {
	case testutil.DialectMySQL:
		q = `SELECT COALESCE(checksum,'') FROM event_history WHERE workflow_id = ? AND step = 0`
	case testutil.DialectMSSQL:
		q = `SELECT COALESCE(checksum,'') FROM event_history WHERE workflow_id = @p1 AND step = 0`
	default:
		q = `SELECT COALESCE(checksum,'') FROM event_history WHERE workflow_id = $1 AND step = 0`
	}
	var checksum string
	if err := db.QueryRow(q, wfID).Scan(&checksum); err != nil {
		t.Fatalf("reading back the await checksum on %s: %v", dialect, err)
	}
	return checksum
}
