package engine

// The round trip for signal_workflow, durable_send and fetch (cleat#4152, G1b), the same properties as step_dispatcher_roundtrip_test.go and
// for the same reason: run the call FRESH into a real event_history, load that history back from the store, replay it, and assert
//
//	(a) replay ends with stepCount == len(history), nothing unread, and the dispatcher noted no violation;
//	(b) the effect happened exactly once: replaying it did not do it again (no signal delivered, no request dispatched, no fetch made);
//	(c) the answer is the one the original run got;
//	(d) a second replay changes nothing;
//	(e) a call made after the replay is a fresh step at the frontier: it records, it does its effect, and leaving replay noted nothing.
//
// The effect is counted on the OUTSIDE WORLD (g1bWorld: the signal store, the service caller, the fetcher), not on the store, because these
// calls reach a different place than the promise calls do. A durable_send dispatches on a goroutine, so a replay that wrongly sent again is
// looked for after a short bound, and a real send is waited for; both bounds are in g1bObserve.
//
// Run against each dialect that has a DSN, like the other store-backed tests; an unset DSN skips that dialect (read the skip names).

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestEveryG1bSpecRoundTripsThroughARealEventHistory(t *testing.T) {
	for _, d := range []struct {
		name    string
		dialect testutil.Dialect
		setup   func(*testing.T, *sql.DB)
	}{
		{"postgres", testutil.DialectPostgres, func(t *testing.T, db *sql.DB) { testutil.SetupFullSchema(t, db, testutil.DialectPostgres) }},
		{"mssql", testutil.DialectMSSQL, func(t *testing.T, db *sql.DB) { testutil.SetupMSSQLFullSchema(t, db) }},
		{"mysql", testutil.DialectMySQL, func(t *testing.T, db *sql.DB) { testutil.SetupMySQLFullSchema(t, db) }},
	} {
		d := d
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			d.setup(t, db)
			defer db.Close()
			for i, gc := range g1bCalls {
				gc := gc
				t.Run(gc.name, func(t *testing.T) { roundTripG1bCall(t, d.dialect, db, i, gc) })
			}
		})
	}
}

func roundTripG1bCall(t *testing.T, dialect testutil.Dialect, db *sql.DB, n int, gc g1bCall) {
	t.Helper()
	ctx := context.Background()
	wfID := "round-trip-g1b-" + string(dialect) + "-" + time.Now().Format("150405.000000") + string(rune('a'+n))
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	w := newG1bWorld()
	eng := NewEngine(nil, w, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db),
		WithSignalStore(w), WithFetcher(w))
	session := func(history []EventRecord) *execSession {
		return &execSession{
			engine: eng, workflowID: wfID, defName: "caller-def", nowMs: time.Now().UnixMilli(),
			isReplay: len(history) > 0, history: history,
			deferrals: map[string]string{}, queryState: map[string]string{},
		}
	}
	load := func() []EventRecord {
		h, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("load history: %v", err)
		}
		return h
	}
	// effects is how many times the call reached the outside world. A send is dispatched on a goroutine: after a call that should have sent,
	// wait for it (bounded); after one that should not have, wait the same short bound for a send that wrongly happens.
	effects := func(expectSend bool) int {
		if gc.name == "durable_send" {
			if expectSend {
				select {
				case <-w.sendDone:
				case <-time.After(2 * time.Second):
				}
			} else {
				time.Sleep(150 * time.Millisecond)
			}
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.deliveries) + len(w.sends) + len(w.fetches)
	}
	call := func(s *execSession) int64 {
		buf := make([]byte, g1bBufLen)
		return gc.call(context.WithValue(ctx, wasmMemBufKey{}, buf), s)
	}

	// The original run.
	fresh := session(nil)
	freshRes := call(fresh)
	history := load()
	if got := effects(true); len(history) != 1 || got != 1 {
		t.Fatalf("PRECONDITION FAILED: the fresh run left %d events in the store and reached the outside world %d times, want 1 and 1 (the round trip below is about THAT history)", len(history), got)
	}

	replayOnce := func(label string) (int64, *execSession) {
		s := session(load())
		res := call(s)
		if s.stepCount != len(s.history) {
			t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
		}
		if len(s.stepViolations) != 0 {
			t.Errorf("%s: (a) the dispatcher noted %v", label, s.stepViolations)
		}
		if got := effects(false); got != 1 {
			t.Errorf("%s: (b) the outside world was reached %d times after a replay, want still 1: replay did the effect again", label, got)
		}
		return res, s
	}
	first, _ := replayOnce("first replay")
	if first != freshRes {
		t.Errorf("(c) the replay answered %#x where the original run was told %#x", first, freshRes)
	}
	second, _ := replayOnce("second replay")
	if second != first {
		t.Errorf("(d) a second replay answered %#x, the first answered %#x", second, first)
	}
	if h := load(); len(h) != 1 {
		t.Errorf("(d) replaying changed the stored history: %d events, want 1", len(h))
	}

	// (e) the frontier: the same call again, past the end of what was replayed, is fresh work.
	s := session(load())
	call(s) // replays the recorded one
	call(s) // a new one at the frontier
	if len(s.stepViolations) != 0 {
		t.Errorf("(e) leaving replay at the frontier: the dispatcher noted %v", s.stepViolations)
	}
	if got := effects(true); got != 2 {
		t.Errorf("(e) the call made at the frontier reached the outside world %d times in all, want 2 (one original, one new)", got)
	}
	if h := load(); len(h) != 2 {
		t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
	}
}
