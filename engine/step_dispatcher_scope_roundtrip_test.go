package engine

// The round trip for SetScope's spec (cleat#4211), the same shape as step_dispatcher_roundtrip_test.go's for the promise calls: run the step FRESH into
// a real event_history, load it back from the store, replay it, and assert the five things that file asserts (replay consumes the history and the
// dispatcher notes no violation; the effect happened once; the answer is the original's; a second replay changes nothing; a call past the end is
// fresh work), plus the row only a real store can give.
//
// The identity check compares the scope key a replay asks for with the one the LOADED record carries, so what is held to account is each dialect's
// persist-and-load of scope_key (store_events.go), which was read from source and never run. A hand-written record carries the key by construction
// and cannot show it, and a key that did not survive a dialect would not fail an own-key row loudly: it would make EVERY replay of the call diverge,
// which is why the own-key row is asserted first.
//
// The second test is the shape that gave SetScope a multi-record history (cleat#4177): a scope held by another workflow is recorded as a failed
// attempt and the workflow suspends, and each wake records the next attempt, so history is [failed, failed, ..., acquired]. It is driven through the
// real store across separate sessions, as a worker would, because a run read from hand-written records cannot show that a failed attempt's error
// and key survive the store.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// scriptedScopeStore answers each acquisition from a script, then acquires.
type scriptedScopeStore struct {
	script   []bool // false = held by another workflow
	acquires int
}

func (p *scriptedScopeStore) AcquireConcurrencyKey(context.Context, string, string, time.Duration) (bool, error) {
	p.acquires++
	if len(p.script) > 0 {
		ok := p.script[0]
		p.script = p.script[1:]
		return ok, nil
	}
	return true, nil
}

func (p *scriptedScopeStore) ReleaseConcurrencyKey(context.Context, string, string) (bool, error) {
	return true, nil
}

var scopeRoundTripDialects = []struct {
	name    string
	dialect testutil.Dialect
	setup   func(*testing.T, *sql.DB)
}{
	{"postgres", testutil.DialectPostgres, func(t *testing.T, db *sql.DB) { testutil.SetupFullSchema(t, db, testutil.DialectPostgres) }},
	{"mssql", testutil.DialectMSSQL, func(t *testing.T, db *sql.DB) { testutil.SetupMSSQLFullSchema(t, db) }},
	{"mysql", testutil.DialectMySQL, func(t *testing.T, db *sql.DB) { testutil.SetupMySQLFullSchema(t, db) }},
}

func TestTheSetScopeSpecRoundTripsThroughARealEventHistory(t *testing.T) {
	for _, d := range scopeRoundTripDialects {
		d := d
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			d.setup(t, db)
			defer db.Close()
			roundTripSetScope(t, d.dialect, db)
		})
	}
}

func roundTripSetScope(t *testing.T, dialect testutil.Dialect, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	wfID := "round-trip-scope-" + string(dialect) + "-" + time.Now().Format("150405.000000")
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	keys := &scriptedScopeStore{}
	eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db), WithConcurrencyKeyStore(keys))
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

	const key = "vo:round-trip:scope"
	fresh := session(nil)
	freshRes := fresh.SetScope(ctx, nil, "round-trip", "scope", 0, 0)
	history := load()
	if len(history) != 1 || keys.acquires != 1 {
		t.Fatalf("PRECONDITION FAILED: the fresh run left %d events in the store and reached the key store %d times, want 1 and 1", len(history), keys.acquires)
	}
	if history[0].ScopeKey != key || history[0].Err != "" {
		t.Fatalf("the scope key did not survive %s's persist and load: stored %q, loaded %q (err %q). The identity check compares against it, so every replay of this call would diverge on this dialect", dialect, key, history[0].ScopeKey, history[0].Err)
	}

	replayOnce := func(label string) (int64, *execSession) {
		s := session(load())
		res := s.SetScope(ctx, nil, "round-trip", "scope", 0, 0)
		if s.stepCount != len(s.history) {
			t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
		}
		if len(s.stepViolations) != 0 {
			t.Errorf("%s: (a) the dispatcher noted %v", label, s.stepViolations)
		}
		if keys.acquires != 1 {
			t.Errorf("%s: (b) the key store was reached %d times after a replay, want still 1: replay did the effect again", label, keys.acquires)
		}
		if !s.scopeSet || s.scopePrefix != fresh.scopePrefix || len(s.heldScopes) != 1 || s.heldScopes[0] != key {
			t.Errorf("%s: replay did not reproduce the fresh session's scope state: set %v, prefix %q, held %v (fresh: prefix %q)", label, s.scopeSet, s.scopePrefix, s.heldScopes, fresh.scopePrefix)
		}
		return res, s
	}
	first, _ := replayOnce("first replay")
	if first != freshRes {
		t.Errorf("(c) the replay answered %#x where the original run was told %#x", first, freshRes)
	}
	if n := replayFailureCount(t, eng.Metrics); n != 0 {
		t.Errorf("(c) the replay over its own scope counted %d divergence(s), want 0: the loaded record did not match the key it was written with", n)
	}
	if second, _ := replayOnce("second replay"); second != first {
		t.Errorf("(d) a second replay answered %#x, the first answered %#x", second, first)
	}
	if h := load(); len(h) != 1 {
		t.Errorf("(d) replaying changed the stored history: %d events, want 1", len(h))
	}

	// The identity row, over the LOADED history: another scope is a divergence and leaves the record for the call it belongs to.
	other := session(load())
	before := replayFailureCount(t, eng.Metrics)
	if got := other.SetScope(ctx, nil, "round-trip", "another", 0, 0); got != 1 {
		t.Errorf("asking for another scope over the loaded record returned %#x, want 0x1 (a divergence)", got)
	}
	if other.stepCount != 0 || !other.isReplay || other.scopeSet {
		t.Errorf("a different scope consumed the loaded record or took effect: stepCount %d, isReplay %v, scopeSet %v", other.stepCount, other.isReplay, other.scopeSet)
	}
	if after := replayFailureCount(t, eng.Metrics); after != before+1 {
		t.Errorf("the divergence over a loaded record was counted %d time(s), want 1", after-before)
	}

	// (e) the frontier: past the end of what was replayed, a call is fresh work.
	s := session(load())
	s.SetScope(ctx, nil, "round-trip", "scope", 0, 0) // replays the recorded one
	s.SetScope(ctx, nil, "round-trip", "second", 0, 0)
	if len(s.stepViolations) != 0 {
		t.Errorf("(e) leaving replay at the frontier: the dispatcher noted %v", s.stepViolations)
	}
	if keys.acquires != 2 {
		t.Errorf("(e) the call made at the frontier reached the key store %d times in all, want 2", keys.acquires)
	}
	if h := load(); len(h) != 2 {
		t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
	}
}

func TestAFailedScopeRunSurvivesARealEventHistory(t *testing.T) {
	for _, d := range scopeRoundTripDialects {
		d := d
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			d.setup(t, db)
			defer db.Close()
			ctx := context.Background()
			wfID := "round-trip-scope-run-" + string(d.dialect) + "-" + time.Now().Format("150405.000000")
			seedWorkflowInstance(t, db, d.dialect, wfID)
			store := storeFor(t, d.dialect, db)
			keys := &scriptedScopeStore{script: []bool{false, false}} // held twice, then free
			eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db), WithConcurrencyKeyStore(keys))
			eng.Metrics = newReplayMetrics(t)
			session := func() *execSession {
				h, err := store.LoadEventHistory(ctx, wfID)
				if err != nil {
					t.Fatalf("load history: %v", err)
				}
				return &execSession{
					engine: eng, workflowID: wfID, nowMs: time.Now().UnixMilli(),
					isReplay: len(h) > 0, history: h,
					deferrals: map[string]string{}, queryState: map[string]string{},
				}
			}
			call := func(s *execSession) int64 { return s.SetScope(ctx, nil, "round-trip", "contended", 0, 0) }

			// Three wakes of one workflow, each in a session of its own: held, held, then free.
			for wake := 1; wake <= 3; wake++ {
				s := session()
				call(s)
				if suspended := s.suspendErr != nil; suspended != (wake < 3) {
					t.Fatalf("wake %d: suspended = %v, want %v", wake, suspended, wake < 3)
				}
				if h, _ := store.LoadEventHistory(ctx, wfID); len(h) != wake {
					t.Fatalf("PRECONDITION FAILED: after wake %d the store holds %d events, want %d", wake, len(h), wake)
				}
			}
			h, _ := store.LoadEventHistory(ctx, wfID)
			if h[0].Err == "" || h[1].Err == "" || h[2].Err != "" || h[0].ScopeKey != "vo:round-trip:contended" || h[1].ScopeKey != h[0].ScopeKey || h[2].ScopeKey != h[0].ScopeKey {
				t.Fatalf("the run did not survive %s's persist and load: %+v", d.dialect, h)
			}
			if keys.acquires != 3 {
				t.Fatalf("PRECONDITION FAILED: the key store was reached %d times, want 3", keys.acquires)
			}

			// Every later wake replays the whole run and does nothing: the defect of cleat#4177 was a replay that stopped at the first failed
			// attempt and re-acquired the key on EVERY later replay.
			for replay := 1; replay <= 2; replay++ {
				s := session()
				call(s)
				if s.stepCount != 3 || len(s.stepViolations) != 0 || s.suspendErr != nil {
					t.Errorf("replay %d: stepCount %d (want 3), violations %v, suspended %v", replay, s.stepCount, s.stepViolations, s.suspendErr != nil)
				}
				if !s.scopeSet || len(s.heldScopes) != 1 {
					t.Errorf("replay %d did not adopt the scope: set %v, held %v", replay, s.scopeSet, s.heldScopes)
				}
				if keys.acquires != 3 {
					t.Errorf("replay %d reached the key store: %d acquisitions in all, want still 3", replay, keys.acquires)
				}
			}
			if n := replayFailureCount(t, eng.Metrics); n != 0 {
				t.Errorf("replaying the run counted %d divergence(s), want 0", n)
			}
			if h, _ := store.LoadEventHistory(ctx, wfID); len(h) != 3 {
				t.Errorf("replaying changed the stored history: %d events, want 3", len(h))
			}
		})
	}
}
