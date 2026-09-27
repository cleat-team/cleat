package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestChildOutcomeForSettledStatusNeverReturnsAnEmptyError is cleat#2333's
// other empty-outcome residual, found extending cleat-review's promise_result
// "" question to AwaitChild's error path. admin_ops.go's ForceFail validates
// workflowID, generation, operator and errorCode, but never errorMsg, so an
// operator can force-fail a child workflow with an empty message --
// unlike GetChildResult's `result`, which every dialect already COALESCEs to
// "{}", `error_msg` was read with no equivalent guarantee, and
// childOutcomeForSettledStatus returned Error == "" for it.
//
// That reaches AwaitChild's completing write (children.go) as Err == "",
// which nullStr stores as SQL NULL -- column-identical, on BOTH response and
// error, to AwaitChild's own pending row. Unlike AwaitPromise, AwaitChild's
// event_type never transitions between pending and complete (both write
// EventTypeAwaitChild), so there is no event_type escape hatch: a later stray
// reflush of the same step would read the row as still pending and pass
// every dialect's completion guard, reopening cleat#1379's corruption case.
//
// Fixed by nonEmptyChildError (status_vocabulary.go), which this test
// exercises directly against childOutcomeForSettledStatus rather than
// through a live database, since the function is pure.
func TestChildOutcomeForSettledStatusNeverReturnsAnEmptyError(t *testing.T) {
	// statusDone carries no error at all; statusTerminated/statusCancelled
	// already guarantee non-emptiness via their "[TERMINATED] "/"[CANCELLED] "
	// prefix regardless of errMsg. Only these two ever return the raw,
	// unprefixed errMsg.
	for _, status := range []string{statusFailed, statusDeadLettered} {
		t.Run(status, func(t *testing.T) {
			outcome, ok := childOutcomeForSettledStatus(status, "", sql.NullString{})
			if !ok {
				t.Fatalf("childOutcomeForSettledStatus(%q, ...) ok = false, want true", status)
			}
			if !outcome.Failed {
				t.Fatalf("childOutcomeForSettledStatus(%q, ...): Failed = false, want true", status)
			}
			if outcome.Error == "" {
				t.Errorf("childOutcomeForSettledStatus(%q, ...) with an empty errMsg: Error = %q, want a non-empty sentinel -- "+
					"an empty Error here reaches AwaitChild's completing write as Err == \"\", which is column-identical "+
					"to AwaitChild's own pending row and has no event_type escape hatch", status, outcome.Error)
			}
		})
	}
}

// TestAnAwaitChildErroredCompletionStaysImmutable mirrors
// TestAnAwaitStyleEventCompletesOnEveryDialect, exercising the ERROR half of
// the completion guard (`error IS NULL`) rather than the response half --
// cleat-review's checklist for cleat#2333 asked for an "errored completion"
// case alongside the empty-outcome ones, and until now nothing exercised Err
// specifically; every existing regression test used Response.
func TestAnAwaitChildErroredCompletionStaysImmutable(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()
		runAwaitChildErroredCompletionCase(t, testutil.DialectPostgres, db, "await-error-pg")
	})

	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		runAwaitChildErroredCompletionCase(t, testutil.DialectMSSQL, db, "await-error-mssql")
	})

	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		runAwaitChildErroredCompletionCase(t, testutil.DialectMySQL, db, "await-error-mysql")
	})
}

func runAwaitChildErroredCompletionCase(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) {
	t.Helper()
	seedWorkflowInstance(t, db, dialect, wfID)

	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(store),
		WithDB(db))

	runID := "child-run-errored-1"

	pending := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitChild,
		RunID:       runID,
		TimestampMs: time.Now().UnixMilli(),
	}
	if err := eng.flushEvent(context.Background(), wfID, pending, ""); err != nil {
		t.Fatalf("suspend flush failed on %s: %v", dialect, err)
	}

	// Complete with an ERROR, not a response -- the shape children.go records
	// when out.Failed (or err != nil). nonEmptyChildError (status_vocabulary.go)
	// is what guarantees this is never "" from the real caller; this test
	// uses realistic non-empty content, matching that guarantee, to prove the
	// completion guard's error-side IS NULL check actually gates on it.
	completed := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitChild,
		RunID:       runID,
		Err:         "child failed: payment declined",
		TimestampMs: pending.TimestampMs,
	}
	wantChecksum := computeEventChecksum(completed, "")
	if err := eng.flushEvent(context.Background(), wfID, completed, ""); err != nil {
		t.Fatalf("completing (errored) flush failed on %s: %v", dialect, err)
	}

	history, err := store.LoadEventHistory(context.Background(), wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("LoadEventHistory returned %d events on %s, want 1", len(history), dialect)
	}
	if history[0].Err != completed.Err {
		t.Errorf("%s: stored error = %q, want %q -- the completing flush did not take effect", dialect, history[0].Err, completed.Err)
	}
	gotChecksum := readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Errorf("%s: stored checksum = %s, want %s (recomputed from the COMPLETED record)", dialect, gotChecksum, wantChecksum)
	}

	// Immutability: a stray re-flush with DIFFERENT error content must not
	// overwrite an already-errored completion.
	again := EventRecord{
		Step:        0,
		EventType:   EventTypeAwaitChild,
		RunID:       runID,
		Err:         "should never land",
		TimestampMs: pending.TimestampMs,
	}
	if err := eng.flushEvent(context.Background(), wfID, again, ""); err != nil {
		t.Fatalf("re-flush of a completed (errored) row failed on %s: %v", dialect, err)
	}
	history, err = store.LoadEventHistory(context.Background(), wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("LoadEventHistory returned %d events on %s after reflush, want 1", len(history), dialect)
	}
	if history[0].Err != completed.Err {
		t.Errorf("%s: a completed (errored) row was overwritten by a stray re-flush: error = %q, want the original %q", dialect, history[0].Err, completed.Err)
	}
	gotChecksum = readAwaitChecksum(t, dialect, testutil.AdminDB(t, db, dialect), wfID)
	if gotChecksum != wantChecksum {
		t.Errorf("%s: a completed (errored) row's checksum changed after a stray re-flush: %s, want the original %s", dialect, gotChecksum, wantChecksum)
	}
}
