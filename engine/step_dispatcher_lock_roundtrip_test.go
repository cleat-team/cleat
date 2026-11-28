package engine

// The round trip for the two lock specs (cleat#4211), the same shape as step_dispatcher_roundtrip_test.go's for the promise calls: run the step FRESH into
// a real event_history, load it back from the store, replay it, and assert
//
//	(a) replay ends with stepCount == len(history) and the dispatcher noted no violation;
//	(b) the effect happened exactly once: replaying it did not take or give the lock again;
//	(c) the answer is the one the original run got;
//	(d) a second replay changes nothing;
//	(e) a call made after the replay is a fresh step at the frontier;
//
// and the row that only a real store can give. The identity check compares the lock key a replay asks for with the one the LOADED record carries,
// so what is being held to account is each dialect's persist-and-load of lock_key (store_events.go), which is read from source and was never run.
// A hand-written record cannot show it: it would carry the key by construction. A key that did not survive a dialect would not fail the own-key row
// loudly, it would make EVERY replay of that call diverge, which is why the own-key row is asserted first.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

type lockCall struct {
	name string
	call func(s *execSession, key string) int64
	// effects is how many times the call reached the lock store.
	effects func(p *recordingLockStore) int
}

var lockCalls = []lockCall{
	{"acquire_lock", func(s *execSession, key string) int64 { return s.AcquireLock(context.Background(), nil, key, 1000) },
		func(p *recordingLockStore) int { return p.acquires }},
	{"release_lock", func(s *execSession, key string) int64 { return s.ReleaseLock(context.Background(), nil, key) },
		func(p *recordingLockStore) int { return p.releases }},
}

func TestEveryLockSpecRoundTripsThroughARealEventHistory(t *testing.T) {
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
			for i, lc := range lockCalls {
				lc := lc
				t.Run(lc.name, func(t *testing.T) { roundTripLockCall(t, d.dialect, db, i, lc) })
			}
		})
	}
}

func roundTripLockCall(t *testing.T, dialect testutil.Dialect, db *sql.DB, n int, lc lockCall) {
	t.Helper()
	ctx := context.Background()
	wfID := "round-trip-lock-" + string(dialect) + "-" + time.Now().Format("150405.000000") + string(rune('a'+n))
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	locks := &recordingLockStore{}
	eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db), WithConcurrencyKeyStore(locks))
	eng.Metrics = newReplayMetrics(t)
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

	const key = "round-trip-lock"
	fresh := session(nil)
	freshRes := lc.call(fresh, key)
	history := load()
	if len(history) != 1 || lc.effects(locks) != 1 {
		t.Fatalf("PRECONDITION FAILED: the fresh run left %d events in the store and reached the lock store %d times, want 1 and 1", len(history), lc.effects(locks))
	}
	if history[0].LockKey != key {
		t.Fatalf("the lock key did not survive %s's persist and load: stored %q, loaded %q. The identity check compares against it, so every replay of this call would diverge on this dialect", dialect, key, history[0].LockKey)
	}

	replayOnce := func(label string) (int64, *execSession) {
		s := session(load())
		s.engine.Metrics = eng.Metrics
		res := lc.call(s, key)
		if s.stepCount != len(s.history) {
			t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
		}
		if len(s.stepViolations) != 0 {
			t.Errorf("%s: (a) the dispatcher noted %v", label, s.stepViolations)
		}
		if lc.effects(locks) != 1 {
			t.Errorf("%s: (b) the lock store was reached %d times after a replay, want still 1: replay did the effect again", label, lc.effects(locks))
		}
		return res, s
	}
	first, _ := replayOnce("first replay")
	if first != freshRes {
		t.Errorf("(c) the replay answered %#x where the original run was told %#x", first, freshRes)
	}
	if n := replayFailureCount(t, eng.Metrics); n != 0 {
		t.Errorf("(c) the replay over its own key counted %d divergence(s), want 0: the loaded record did not match the key it was written with", n)
	}
	second, _ := replayOnce("second replay")
	if second != first {
		t.Errorf("(d) a second replay answered %#x, the first answered %#x", second, first)
	}
	if h := load(); len(h) != 1 {
		t.Errorf("(d) replaying changed the stored history: %d events, want 1", len(h))
	}

	// The identity row, over the LOADED history: another lock is a divergence and leaves the record for the call it belongs to.
	other := session(load())
	other.engine.Metrics = eng.Metrics
	before := replayFailureCount(t, eng.Metrics)
	if got := lc.call(other, "some-other-lock"); got != 1 {
		t.Errorf("asking for another lock over the loaded record returned %#x, want 0x1 (a divergence)", got)
	}
	if other.stepCount != 0 || !other.isReplay {
		t.Errorf("a different key consumed the loaded record: stepCount %d, isReplay %v", other.stepCount, other.isReplay)
	}
	if after := replayFailureCount(t, eng.Metrics); after != before+1 {
		t.Errorf("the divergence over a loaded record was counted %d time(s), want 1", after-before)
	}

	// (e) the frontier: the same call again, past the end of what was replayed, is fresh work.
	s := session(load())
	lc.call(s, key) // replays the recorded one
	lc.call(s, key) // a new one at the frontier
	if len(s.stepViolations) != 0 {
		t.Errorf("(e) leaving replay at the frontier: the dispatcher noted %v", s.stepViolations)
	}
	if lc.effects(locks) != 2 {
		t.Errorf("(e) the call made at the frontier reached the lock store %d times in all, want 2 (one original, one new)", lc.effects(locks))
	}
	if h := load(); len(h) != 2 {
		t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
	}
}
