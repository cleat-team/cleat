package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#4177 found that AwaitPromise left replay at its own pending record on every later replay, because the outcome of a completed await is
// recorded at the NEXT step and the pending row stays. Its issue named four siblings that reach a pending record the same way and measured
// none of them: AwaitChild, AwaitAllChildren, SetScope's retry (and AwaitAnyChild, which cleat#3391 fixed). This measures each through a REAL
// event_history, because the defect lives between the replay and the store's conflict clause (insertEventSQL reopens only await_child,
// await_promise and await_all_children): a history written by hand at the shape the code is assumed to produce is the mistake the issue
// records, and a store that is correct only for the shape you wrote hides it.
//
// Each site is woken the way a worker wakes it: load the recorded history, replay it, go fresh past the end. The follow-up is one durable
// `call` recorded after the await, and what is counted is how many times it really happens (a StartChild that starts another child, here).
type pendingResumeSite struct {
	name string
	// await runs the site once on s and reports whether it suspended.
	await func(ctx context.Context, s *execSession) (suspended bool)
	// finish makes the awaited thing available, so the next wake gets past the await.
	finish func()
	// effects reports how many times the site's own effect really happened (nil: not measured).
	effects func() int
}

func TestAPendingResumeLeavesNothingAfterItToRerun(t *testing.T) {
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
			for i, mk := range []func(*Engine) pendingResumeSite{awaitChildSite, awaitAllChildrenSite, setScopeRetrySite} {
				runPendingResumeSite(t, d.dialect, db, i, mk)
			}
		})
	}
}

func awaitChildSite(eng *Engine) pendingResumeSite {
	done := false
	eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) {
		if done {
			return ChildOutcome{Completed: true, Result: `{"ok":true}`}, nil
		}
		return ChildOutcome{}, nil
	}}
	return pendingResumeSite{
		name: "AwaitChild",
		await: func(ctx context.Context, s *execSession) bool {
			res := s.AwaitChild(ctx, nil, "child-run-1", 0, 0)
			return s.suspendErr != nil || res == packAwaitChildResultSuspend()
		},
		finish: func() { done = true },
	}
}

func awaitAllChildrenSite(eng *Engine) pendingResumeSite {
	done := false
	eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) {
		if done {
			return ChildOutcome{Completed: true, Result: `{"ok":true}`}, nil
		}
		return ChildOutcome{}, nil
	}}
	return pendingResumeSite{
		name: "AwaitAllChildren",
		await: func(ctx context.Context, s *execSession) bool {
			s.AwaitAllChildren(ctx, nil, `["child-run-1"]`, 0, 0)
			return s.suspendErr != nil
		},
		finish: func() { done = true },
	}
}

// scopeKeyDouble is a concurrency-key store whose key is held by somebody else until released.
type scopeKeyDouble struct {
	held     bool
	acquires int // acquisitions that SUCCEEDED
}

func (k *scopeKeyDouble) AcquireConcurrencyKey(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	if k.held {
		return false, nil
	}
	k.acquires++
	return true, nil
}
func (k *scopeKeyDouble) ReleaseConcurrencyKey(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

func setScopeRetrySite(eng *Engine) pendingResumeSite {
	k := &scopeKeyDouble{held: true}
	eng.concurrencyKeyStore = k
	return pendingResumeSite{
		name: "SetScope retry",
		await: func(ctx context.Context, s *execSession) bool {
			s.SetScope(ctx, nil, "account", "acct-1", 0, 0)
			return s.suspendErr != nil
		},
		finish:  func() { k.held = false },
		effects: func() int { return k.acquires },
	}
}

func runPendingResumeSite(t *testing.T, dialect testutil.Dialect, db *sql.DB, n int, mk func(*Engine) pendingResumeSite) {
	t.Helper()
	wfID := "pending-resume-" + string(dialect) + "-" + time.Now().Format("150405.000000") + string(rune('a'+n))
	seedWorkflowInstance(t, db, dialect, wfID)
	store := storeFor(t, dialect, db)
	eng := NewEngine(nil, nil, WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID), WithWorkflowStore(store), WithDB(db))
	site := mk(eng)
	ctx := context.Background()
	followUps := 0

	wake := func() (suspended bool) {
		history, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("%s: load history: %v", site.name, err)
		}
		s := &execSession{
			engine: eng, workflowID: wfID, nowMs: time.Now().UnixMilli(),
			isReplay: len(history) > 0, history: history,
			deferrals: map[string]string{}, queryState: map[string]string{},
		}
		if site.await(ctx, s) {
			return true
		}
		// Where the await left the session is where the guest carries on: replay the follow-up if recorded, else it really happens.
		if s.isReplay && s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType != EventTypeCall {
				t.Fatalf("%s: replay divergence: step %d is %q, want the follow-up call", site.name, rec.Step, rec.EventType)
			}
			s.advanceReplayStep(ctx, &rec)
			return false
		}
		s.exitReplay()
		followUps++
		s.recordEvent(EventRecord{Step: s.stepCount, EventType: EventTypeCall, Service: "svc", Op: "after-the-await", Response: `{}`})
		return false
	}

	for i := 1; i <= 3; i++ {
		if !wake() {
			t.Fatalf("PRECONDITION FAILED: %s wake %d went past the await while what it waits for was not ready", site.name, i)
		}
	}
	site.finish()
	for i := 1; i <= 5; i++ {
		if wake() {
			t.Fatalf("PRECONDITION FAILED: %s wake %d after the wait finished suspended again", site.name, i)
		}
	}
	if followUps != 1 {
		t.Errorf("%s: the operation after the await really happened %d times across 5 wakes after it finished, want exactly 1 (cleat#4177)", site.name, followUps)
	}
	if site.effects != nil {
		if got := site.effects(); got != 1 {
			t.Errorf("%s: the awaited effect succeeded %d times across the wakes, want exactly 1 (a replay re-acquired it)", site.name, got)
		}
	}
}
