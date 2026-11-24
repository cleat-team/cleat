package engine

// The round trip for side_effect, its lookup and defer (cleat#4152, G1a), the same idea as step_dispatcher_roundtrip_test.go and
// step_dispatcher_g1b_roundtrip_test.go: run the call FRESH into a real event_history, load that history back from the store, replay it, and
// check what replay did. The history is the one the engine WROTE, never one built by hand (cleat#4177: a test that hand-writes the producer's
// output cannot see the producer).
//
// These three calls reach nothing outside the process: a side effect IS its recorded value, a defer registers a cleanup in the session. So
// "the effect happened once" is stated as what each one DID to the session:
//
//	side_effect          (a) replay ends at the end of history, nothing unread, no violation; (c) it answers the RECORDED value, not what the
//	                     replaying guest computed; (d) a second replay answers the same and writes nothing; (e) a call at the frontier records.
//	side_effect_recorded the lookup: replaying the recorded value answers it and consumes the record; a second replay answers the same; at the
//	                     FRONTIER it answers "not recorded", records NOTHING and leaves the session in replay (that last is what the lookup
//	                     exists for: the caller then runs its function).
//	defer                replay re-registers the cleanup under the id the original run gave it (the effect, here), a second replay changes
//	                     nothing, and a defer at the frontier is a new, recorded step with the next id.
//
// Run against each dialect that has a DSN, like the other store-backed tests; an unset DSN skips that dialect (read the skip names).

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestEveryG1aSpecRoundTripsThroughARealEventHistory(t *testing.T) {
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
			for i, name := range []string{"side_effect", "side_effect_recorded", "defer"} {
				name, i := name, i
				t.Run(name, func(t *testing.T) { roundTripG1aCall(t, d.dialect, db, i, name) })
			}
		})
	}
}

func roundTripG1aCall(t *testing.T, dialect testutil.Dialect, db *sql.DB, n int, name string) {
	t.Helper()
	ctx := context.Background()
	wfID := "round-trip-g1a-" + string(dialect) + "-" + time.Now().Format("150405.000000") + string(rune('a'+n))
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db))
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
	// wrote is what the call put in the guest's buffer.
	call := func(s *execSession, f func(context.Context, *execSession) int64) (res int64, wrote string) {
		buf := make([]byte, g1BufLen)
		res = f(context.WithValue(ctx, wasmMemBufKey{}, buf), s)
		k := 0
		for k < len(buf) && buf[k] != 0 {
			k++
		}
		return res, string(buf[:k])
	}
	sideEffect := func(ctx context.Context, s *execSession) int64 {
		return s.SideEffect(ctx, nil, "computed-now", 0, g1BufLen)
	}
	lookup := func(ctx context.Context, s *execSession) int64 { return s.SideEffectRecorded(ctx, nil, 0, g1BufLen) }
	deferCall := func(ctx context.Context, s *execSession) int64 {
		return s.DurableDefer(ctx, nil, "the-cleanup", 0, g1BufLen)
	}
	noViolations := func(label string, s *execSession) {
		t.Helper()
		if len(s.stepViolations) != 0 {
			t.Errorf("%s: the dispatcher noted %v", label, s.stepViolations)
		}
	}

	switch name {
	case "side_effect":
		fresh := session(nil)
		freshRes, freshWrote := call(fresh, sideEffect)
		history := load()
		if len(history) != 1 || history[0].EventType != EventTypeSideEffect || history[0].SideEffectResult != "computed-now" {
			t.Fatalf("PRECONDITION FAILED: the fresh run left %d events, first %v; want one side_effect recording %q (the round trip below is about THAT history)",
				len(history), history, "computed-now")
		}
		replayOnce := func(label string) (int64, string) {
			s := session(load())
			// The replaying guest would compute something else; replay must answer the RECORDED value regardless.
			res, wrote := call(s, func(ctx context.Context, s *execSession) int64 {
				return s.SideEffect(ctx, nil, "a-different-value-now", 0, g1BufLen)
			})
			if s.stepCount != len(s.history) {
				t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
			}
			noViolations(label, s)
			if h := load(); len(h) != 1 {
				t.Errorf("%s: replaying changed the stored history: %d events, want 1", label, len(h))
			}
			return res, wrote
		}
		first, firstWrote := replayOnce("first replay")
		if first != freshRes || firstWrote != freshWrote {
			t.Errorf("(c) the replay answered %#x %q where the original run was told %#x %q", first, firstWrote, freshRes, freshWrote)
		}
		if second, secondWrote := replayOnce("second replay"); second != first || secondWrote != firstWrote {
			t.Errorf("(d) a second replay answered %#x %q, the first answered %#x %q", second, secondWrote, first, firstWrote)
		}
		s := session(load())
		call(s, sideEffect) // replays the recorded one
		call(s, sideEffect) // a new one at the frontier
		noViolations("(e) frontier", s)
		if h := load(); len(h) != 2 {
			t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
		}

	case "side_effect_recorded":
		// The producer is a real side_effect; the lookup then reads what it wrote.
		call(session(nil), sideEffect)
		history := load()
		if len(history) != 1 {
			t.Fatalf("PRECONDITION FAILED: the producing side_effect left %d events, want 1", len(history))
		}
		replayOnce := func(label string) (int64, string, *execSession) {
			s := session(load())
			res, wrote := call(s, lookup)
			if s.stepCount != len(s.history) {
				t.Errorf("%s: replay ended at step %d with %d records in history; the lookup must consume the record it answers", label, s.stepCount, len(s.history))
			}
			noViolations(label, s)
			return res, wrote, s
		}
		first, firstWrote, _ := replayOnce("first replay")
		if firstWrote != "computed-now" {
			t.Errorf("the lookup answered %q, want the recorded value %q", firstWrote, "computed-now")
		}
		if second, secondWrote, _ := replayOnce("second replay"); second != first || secondWrote != firstWrote {
			t.Errorf("(d) a second replay answered %#x %q, the first answered %#x %q", second, secondWrote, first, firstWrote)
		}
		// The frontier: a lookup with nothing left to read says "not recorded", records nothing, and STAYS in replay.
		s := session(load())
		call(s, lookup) // consumes the recorded value
		res, wrote := call(s, lookup)
		if wrote != "" || res != packSimpleResult(sideEffectNotRecorded) {
			t.Errorf("(e) the lookup at the frontier answered %#x %q, want the not-recorded status %#x and nothing written", res, wrote, packSimpleResult(sideEffectNotRecorded))
		}
		if !s.isReplay {
			t.Errorf("(e) the lookup at the frontier left replay, but a lookup must not: the caller runs its function next and records with side_effect")
		}
		noViolations("(e) frontier", s)
		if h := load(); len(h) != 1 {
			t.Errorf("(e) the lookup at the frontier changed the stored history: %d events, want still 1", len(h))
		}

	case "defer":
		fresh := session(nil)
		freshRes, freshWrote := call(fresh, deferCall)
		history := load()
		if len(history) != 1 || history[0].EventType != EventTypeDefer || history[0].DeferDescription != "the-cleanup" {
			t.Fatalf("PRECONDITION FAILED: the fresh run left %d events, first %v; want one defer of %q", len(history), history, "the-cleanup")
		}
		wantDefers := g1Deferrals(fresh)
		replayOnce := func(label string) (int64, string) {
			s := session(load())
			res, wrote := call(s, deferCall)
			if s.stepCount != len(s.history) {
				t.Errorf("%s: (a) replay ended at step %d with %d records in history", label, s.stepCount, len(s.history))
			}
			noViolations(label, s)
			if got := g1Deferrals(s); got != wantDefers {
				t.Errorf("%s: (b) replay registered %s, the original run registered %s: the cleanup must be re-registered under the same id", label, got, wantDefers)
			}
			if h := load(); len(h) != 1 {
				t.Errorf("%s: replaying changed the stored history: %d events, want 1", label, len(h))
			}
			return res, wrote
		}
		first, firstWrote := replayOnce("first replay")
		if first != freshRes || firstWrote != freshWrote {
			t.Errorf("(c) the replay answered %#x %q where the original run was told %#x %q", first, firstWrote, freshRes, freshWrote)
		}
		if second, secondWrote := replayOnce("second replay"); second != first || secondWrote != firstWrote {
			t.Errorf("(d) a second replay answered %#x %q, the first answered %#x %q", second, secondWrote, first, firstWrote)
		}
		s := session(load())
		call(s, deferCall) // replays the recorded one
		_, wrote := call(s, deferCall)
		noViolations("(e) frontier", s)
		if wrote != "defer-1" {
			t.Errorf("(e) the defer at the frontier was given id %q, want the next one, defer-1", wrote)
		}
		if h := load(); len(h) != 2 {
			t.Errorf("(e) the frontier call left %d events in the store, want 2", len(h))
		}
	}
}
