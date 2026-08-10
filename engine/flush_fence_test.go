package engine

// B4: the per-step event flush path (engine/flush.go) carried no fencing
// token, so a worker that stalled, was reaped (generation bumped,
// assigned_to cleared, workflow reclaimed by another worker), and then woke
// up could still durably write an event_history row for a workflow it no
// longer owned -- interleaved with whatever its successor was writing.
//
// This drives the generation bump explicitly, the way
// fence_lost_integration_test.go's buildZombieWriterScenario does and
// PARALLEL-WORKSTREAMS.md warns against not doing: claim, capture the
// generation, call ReapStaleInstances with a timeout guaranteed to reclaim
// everything (no sleep, no race), then act as the stale worker. See that
// file's comment on why the timeout is negative rather than the flow being
// timed.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// rawDBOf returns the *sql.DB a WorkflowStore built by StoreBackend.Setup
// wraps, so a test can hand it to WithDB the way cmd/cleat-worker does.
func rawDBOf(t *testing.T, store WorkflowStore) *sql.DB {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		return s.db
	case *MySQLStore:
		return s.db
	case *MSSQLStore:
		return s.db
	default:
		t.Fatalf("rawDBOf: unsupported store type %T", store)
		return nil
	}
}

// TestFlushEvent_FenceLost is B4's core regression test: a stale claim must
// not be able to write a permanent event_history row.
func TestFlushEvent_FenceLost(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "flush-fence-lost")

			wfA, err := store.ClaimWorkflow(ctx, "worker-A")
			if err != nil || wfA == nil || wfA.ID != wfID {
				t.Fatalf("ClaimWorkflow (A): wf=%v err=%v", wfA, err)
			}
			staleGeneration := wfA.Generation

			// Reclaim unconditionally -- see buildZombieWriterScenario's
			// comment on why the timeout is negative rather than the setup
			// being timed.
			reaped, err := store.ReapStaleInstances(ctx, -1*time.Second)
			if err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}
			if reaped < 1 {
				t.Fatalf("ReapStaleInstances reclaimed %d instances, want >= 1 (the stalled workflow)", reaped)
			}

			eng := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkerID("worker-A"),
				WithGeneration(staleGeneration),
				WithTenantID(DefaultTenantUUID))

			rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "resp-from-zombie"}
			flushErr := eng.flushEvent(ctx, wfID, rec, "")
			if !errors.Is(flushErr, ErrFenceLost) {
				t.Fatalf("flushEvent under a lost fence: err = %v, want ErrFenceLost", flushErr)
			}

			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(hist) != 0 {
				t.Fatalf("B4 regression: the zombie's flush persisted despite a lost fence: history = %+v", hist)
			}
		})
	}
}

// TestFlushEvent_FenceHeld is the positive control for TestFlushEvent_FenceLost:
// a flush made under a claim that is still current must still succeed. Without
// this, the fence-lost test could pass for the wrong reason -- flushEvent
// broken for every caller, not specifically for a stale one.
func TestFlushEvent_FenceHeld(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "flush-fence-held")

			wf, err := store.ClaimWorkflow(ctx, "worker-live")
			if err != nil || wf == nil || wf.ID != wfID {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			eng := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkerID("worker-live"),
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID))

			rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "resp-from-live-owner"}
			if flushErr := eng.flushEvent(ctx, wfID, rec, ""); flushErr != nil {
				t.Fatalf("flushEvent under a held fence: %v, want success", flushErr)
			}

			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(hist) != 1 || hist[0].Response != "resp-from-live-owner" {
				t.Fatalf("a legitimately fenced flush did not persist: history = %+v", hist)
			}
		})
	}
}

// TestFlushEvent_FenceHeld_IdempotentReflushIsNotFenceLost pins the
// composition insertEventSQL's doc describes: the fence's WHERE EXISTS
// clause and the pre-existing
//
//	ON CONFLICT ... WHERE event_history.response = '' AND error IS NULL
//
// clause both gate the same statement, for different reasons, and a zero-row
// result from the second (the row already carries a terminal
// response/error, so the conflict resolution correctly declined to
// overwrite it) must not be reported as ErrFenceLost just because
// afterFencedInsert's disambiguation only expects zero rows when the fence
// itself failed. The fence is still held throughout this test -- only the
// idempotent-skip path is under test.
func TestFlushEvent_FenceHeld_IdempotentReflushIsNotFenceLost(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "flush-fence-idempotent")
			wf, err := store.ClaimWorkflow(ctx, "worker-live")
			if err != nil || wf == nil || wf.ID != wfID {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			eng := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkerID("worker-live"),
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID))

			rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "first-response"}
			if flushErr := eng.flushEvent(ctx, wfID, rec, ""); flushErr != nil {
				t.Fatalf("first flushEvent: %v, want success", flushErr)
			}

			// Re-flush the SAME step. event_history.response is now
			// non-empty, so the ON CONFLICT ... WHERE clause declines the
			// update -- zero rows affected, same signal a lost fence
			// produces, but for an unrelated reason. The fence is still held
			// (same worker, same generation, no reap in between).
			replay := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "second-response-should-not-land"}
			if flushErr := eng.flushEvent(ctx, wfID, replay, ""); flushErr != nil {
				t.Fatalf("idempotent re-flush under a held fence: err = %v, want nil (this is not a fence loss)", flushErr)
			}

			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(hist) != 1 || hist[0].Response != "first-response" {
				t.Fatalf("history = %+v, want the first response untouched by the re-flush", hist)
			}
		})
	}
}

// TestFlushEvent_UnfencedWhenNoClaimIdentity pins the backward-compatible
// default: an Engine built without WithWorkerID/WithGeneration (every
// existing embedder/test that constructs one directly, not through a claim)
// must keep flushing unconditionally, exactly as it did before B4.
// engine.fencingEnabled's doc explains why generation's zero value is what
// switches this off rather than a separate flag.
func TestFlushEvent_UnfencedWhenNoClaimIdentity(t *testing.T) {
	store, teardown := (&PostgresBackend{}).Setup(t)
	defer teardown()
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "flush-unfenced")

	// Deliberately no ClaimWorkflow at all: the row's assigned_to/generation
	// stay at their StartNewRun defaults (NULL / 0), which would fail any
	// fence check -- proving this path really is skipping the check, not
	// coincidentally passing it.
	eng := NewEngine(nil, nil,
		WithDB(rawDBOf(t, store)),
		WithWorkflowStore(store),
		WithTenantID(DefaultTenantUUID))

	rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "resp"}
	if err := eng.flushEvent(ctx, wfID, rec, ""); err != nil {
		t.Fatalf("flushEvent with no claim identity set: %v, want success (fencing must be opt-in)", err)
	}
}

// TestFlushEvent_FenceLost_SameWorkerReclaim is the layer-separation check
// CLAUDE.md asks for: it isolates the generation predicate specifically,
// rather than relying on assigned_to alone to carry the assertion.
//
// Every other test in this file reaps and leaves the workflow unclaimed, so
// Heartbeat's `assigned_to = $2` clause alone would already reject the
// zombie's stale worker ID -- a version of Heartbeat with the
// `AND generation = $3` clause silently dropped would still pass those
// tests, for the wrong reason. This scenario closes that gap: the SAME
// worker ID reclaims the workflow after the reap (a worker process that
// crashed and restarted with the same configured ID is not exotic), so
// assigned_to matches the zombie's stale identity again and only the
// generation number distinguishes the stale claim from the live one.
func TestFlushEvent_FenceLost_SameWorkerReclaim(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "flush-fence-reclaim")

			wf1, err := store.ClaimWorkflow(ctx, "worker-A")
			if err != nil || wf1 == nil || wf1.ID != wfID {
				t.Fatalf("ClaimWorkflow (1st): wf=%v err=%v", wf1, err)
			}
			staleGeneration := wf1.Generation

			reaped, err := store.ReapStaleInstances(ctx, -1*time.Second)
			if err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}
			if reaped < 1 {
				t.Fatalf("ReapStaleInstances reclaimed %d instances, want >= 1", reaped)
			}

			// The SAME worker ID reclaims it. assigned_to is now back to
			// "worker-A" -- identical to the zombie's own claim -- and only
			// generation has moved on.
			wf2, err := store.ClaimWorkflow(ctx, "worker-A")
			if err != nil || wf2 == nil || wf2.ID != wfID {
				t.Fatalf("ClaimWorkflow (2nd): wf=%v err=%v", wf2, err)
			}
			if wf2.Generation == staleGeneration {
				t.Fatalf("2nd claim's generation (%d) equals the 1st's (%d) -- test setup did not reproduce the scenario", wf2.Generation, staleGeneration)
			}

			eng := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkerID("worker-A"),
				WithGeneration(staleGeneration), // the 1st claim's, not the 2nd's
				WithTenantID(DefaultTenantUUID))

			rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "resp-from-zombie"}
			flushErr := eng.flushEvent(ctx, wfID, rec, "")
			if !errors.Is(flushErr, ErrFenceLost) {
				t.Fatalf("flushEvent under the 1st claim's stale generation, same worker ID reclaimed: err = %v, want ErrFenceLost", flushErr)
			}

			hist, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(hist) != 0 {
				t.Fatalf("generation predicate did not hold: history = %+v", hist)
			}
		})
	}
}
