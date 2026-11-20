package engine

// The round trip, over every real spec on the dispatcher (cleat#4152, design section 3): run the step FRESH into a real event_history, load
// that history back from the store, replay it, and assert
//
//	(a) replay ends with stepCount == len(history), nothing unread, and the dispatcher noted no violation;
//	(b) the effect happened exactly once: replaying it did not do it again;
//	(c) the answer is the one the original run got -- EXCEPT for a call whose fresh side can report a store failure the event does not record
//	    (see roundTripExceptions);
//	(d) a second replay changes nothing;
//	(e) a call made after the replay is a fresh step at the frontier: it records, it counts, and leaving replay noted nothing.
//
// The history is read from the REAL store and not written by hand. cleat#4177's issue records why: the store tests had hand-written the
// completing record at the shape the code did not produce, so the property held of a history the engine never wrote.
//
// Run against each dialect that has a DSN, like the other store-backed tests; an unset DSN skips that dialect (read the skip names).

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// countingPromiseStore counts the writes that reach the store: the effects of the three promise calls.
type countingPromiseStore struct {
	mockPromiseStore
	writes int32
}

func (c *countingPromiseStore) CreatePromise(ctx context.Context, w, n, id string) error {
	atomic.AddInt32(&c.writes, 1)
	return c.mockPromiseStore.CreatePromise(ctx, w, n, id)
}
func (c *countingPromiseStore) ResolvePromise(ctx context.Context, id, r string) error {
	atomic.AddInt32(&c.writes, 1)
	return c.mockPromiseStore.ResolvePromise(ctx, id, r)
}
func (c *countingPromiseStore) RejectPromise(ctx context.Context, id, e string) error {
	atomic.AddInt32(&c.writes, 1)
	return c.mockPromiseStore.RejectPromise(ctx, id, e)
}

// roundTripExceptions names the calls for which property (c) cannot hold when the store write fails, and why. This is not exercised here
// (the store does not fail in the round trip); it is on the list so that the next reader of (c) finds the gap named. The characterization
// rows <call>/store_error and <call>/store_error_then_replay measure it, and it is filed as its own issue, cleat#4203 (WS-cleat-2's reading, measured here).
var roundTripExceptions = map[string]string{
	"create_promise":  "a failed promise-store write is reported to the guest but not recorded; replay answers the recorded success",
	"resolve_promise": "same",
	"reject_promise":  "same",
}

func TestEveryPromiseSpecRoundTripsThroughARealEventHistory(t *testing.T) {
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
			for i, pc := range promiseCalls {
				pc := pc
				t.Run(pc.name, func(t *testing.T) { roundTripPromiseCall(t, d.dialect, db, i, pc) })
			}
		})
	}
}

func roundTripPromiseCall(t *testing.T, dialect testutil.Dialect, db *sql.DB, n int, pc promiseCall) {
	t.Helper()
	ctx := context.Background()
	wfID := "round-trip-" + string(dialect) + "-" + time.Now().Format("150405.000000") + string(rune('a'+n))
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	ps := &countingPromiseStore{}
	eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db), WithPromiseStore(ps))
	session := func(history []EventRecord) *execSession {
		return &execSession{
			engine: eng, workflowID: wfID, nowMs: time.Now().UnixMilli(),
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

	// The original run.
	fresh := session(nil)
	freshRes := pc.call(fresh)
	history := load()
	if len(history) != 1 || ps.writes != 1 {
		t.Fatalf("PRECONDITION FAILED: the fresh run left %d events in the store and made %d store writes, want 1 and 1 (the round trip below is about THAT history)", len(history), ps.writes)
	}

	replayOnce := func(label string) (int64, *execSession) {
		s := session(load())
		res := pc.call(s)
		if s.stepCount != len(s.history) {
			t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
		}
		if len(s.stepViolations) != 0 {
			t.Errorf("%s: (a) the dispatcher noted %v", label, s.stepViolations)
		}
		if ps.writes != 1 {
			t.Errorf("%s: (b) the store saw %d writes after a replay, want still 1: replay did the effect again", label, ps.writes)
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
	pc.call(s) // replays the recorded one
	pc.call(s) // a new one at the frontier
	if len(s.stepViolations) != 0 {
		t.Errorf("(e) leaving replay at the frontier: the dispatcher noted %v", s.stepViolations)
	}
	if ps.writes != 2 {
		t.Errorf("(e) the call made at the frontier made %d store writes in all, want 2 (one original, one new)", ps.writes)
	}
	if h := load(); len(h) != 2 {
		t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
	}
	if _, ok := roundTripExceptions[pc.name]; !ok {
		t.Errorf("%s has no entry in roundTripExceptions; every real spec says whether property (c) can hold when its store write fails", pc.name)
	}
}
