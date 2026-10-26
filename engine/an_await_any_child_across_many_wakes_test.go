package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAnAwaitAnyChildThatWaitsAcrossManyWakesDoesNotRestartWhatFollowsIt is
// cleat#3391. A parent awaits a child with AwaitAnyChild and then does one
// more durable thing -- in the report, StartChild -- and the child takes several
// parent wakes to finish. Before the fix, every wake after the child completed
// re-ran the parent from the await, so that next durable thing really happened
// again on each one.
//
// It drives the engine's AwaitAnyChild through repeated wakes against a REAL
// event_history, because the defect lives in the interaction between two
// halves that no in-memory test connects: the replay advances past an empty
// suspend record and re-checks at the NEXT step, and the database then
// declines to overwrite that step's row (insertEventSQL's ON CONFLICT only
// reopens await_child, await_promise and await_all_children), so the
// completed result is silently dropped and the next wake replays the same
// empty rows and finds the child done all over again.
//
// The "next durable thing" is stood in for by one `call` event, recorded
// when the session is past replay. What the test counts is how many times
// that happens, which is what the guest observes as StartChild really
// starting a child.
func TestAnAwaitAnyChildThatWaitsAcrossManyWakesDoesNotRestartWhatFollowsIt(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()
		runAwaitAnyChildAcrossWakes(t, testutil.DialectPostgres, db, "await-any-wakes-pg")
	})
	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		runAwaitAnyChildAcrossWakes(t, testutil.DialectMSSQL, db, "await-any-wakes-mssql")
	})
	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		runAwaitAnyChildAcrossWakes(t, testutil.DialectMySQL, db, "await-any-wakes-mysql")
	})
}

func runAwaitAnyChildAcrossWakes(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) {
	t.Helper()
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(store),
		WithDB(db))

	const childRun = "child-run-1"
	childDone := false
	eng.childWfStore = &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			if childDone {
				return ChildOutcome{Completed: true, Result: `{"ok":true}`}, nil
			}
			return ChildOutcome{}, nil
		},
	}

	ctx := context.Background()
	followUps := 0 // how many times the thing AFTER the await really happened

	// wake runs the parent once from its recorded history, the way a worker
	// does on every wake: replay what is recorded, go fresh past the frontier.
	wake := func() (suspended bool, awaitResult string) {
		t.Helper()
		history, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("load history: %v", err)
		}
		s := &execSession{
			engine:     eng,
			workflowID: wfID,
			nowMs:      time.Now().UnixMilli(),
			isReplay:   len(history) > 0,
			history:    history,
			deferrals:  map[string]string{},
			queryState: map[string]string{},
		}
		// A real module writes the result into guest memory; none exists here,
		// so the result is read back from the session's own history instead.
		res := s.AwaitAnyChild(ctx, nil, `["`+childRun+`"]`, 0, 0)
		if s.suspendErr != nil || res == packAwaitChildResultSuspend() {
			return true, ""
		}
		// Where AwaitAnyChild left the session is where the guest carries on.
		for i := len(s.history) - 1; i >= 0; i-- {
			if s.history[i].EventType == EventTypeAwaitAnyChild && s.history[i].Response != "" {
				awaitResult = s.history[i].Response
				break
			}
		}
		// The next durable operation: replay it if it is recorded, otherwise it
		// really happens (StartChild, in the report).
		if s.isReplay && s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType != EventTypeCall {
				t.Fatalf("replay divergence: step %d is %q, want the follow-up call", rec.Step, rec.EventType)
			}
			s.advanceReplayStep(ctx, &rec)
			return false, awaitResult
		}
		s.exitReplay()
		followUps++
		s.recordEvent(EventRecord{
			Step:      s.stepCount,
			EventType: EventTypeCall,
			Service:   "svc",
			Op:        "after-the-await",
			Response:  `{}`,
		})
		return false, awaitResult
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := testutil.AdminDB(t, db, dialect).QueryRow(countEventsSQL(dialect), wfID).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n
	}

	// The child stays unfinished across FOUR wakes.
	var afterFirst int
	for i := 1; i <= 4; i++ {
		suspended, _ := wake()
		if !suspended {
			t.Fatalf("wake %d: the parent went past AwaitAnyChild while its child was still running", i)
		}
		if i == 1 {
			afterFirst = rows()
		}
	}
	// A parent polled forever must not grow its history by a row per wake.
	if got := rows(); got > afterFirst+1 {
		t.Errorf("event_history grew from %d rows to %d across three more wakes of a still-running child; "+
			"a suspended await should not add a row per wake", afterFirst, got)
	}

	// The child finishes. The parent wakes several more times.
	childDone = true
	for i := 1; i <= 5; i++ {
		suspended, got := wake()
		if suspended {
			t.Fatalf("wake %d after the child finished: the parent suspended again", i)
		}
		if got == "" {
			t.Fatalf("wake %d after the child finished: AwaitAnyChild's result is not in the session history", i)
		}
	}

	if followUps != 1 {
		t.Errorf("the operation after AwaitAnyChild really happened %d times across 5 wakes after the child "+
			"finished, want exactly 1. Each extra one is a StartChild that starts another child "+
			"(cleat#3391): the completed result was dropped, so every wake re-ran the parent from the await.", followUps)
	}
}
