package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestQueryStateSurvivesASuspension is the property the queryable-state
// mechanism exists for, stated in docs/determinism.md:
//
//	the workflow proactively records queryable state to the database at points
//	of its choosing, and that state is durable and externally readable -- via
//	GetQueryState or GET /api/workflows/:id/query?key=X -- regardless of
//	whether any worker currently has the workflow loaded. It answers "what is
//	this workflow's status" without needing anything to be running at query
//	time.
//
// A suspended workflow is exactly the case that sentence names -- no worker
// has it loaded -- and it was the one case that returned nothing.
// finalize_workflow_status wrote query_state on its 'done' and 'failed'
// branches and not on 'ready', in all three dialects, so a value reached the
// database only once the workflow had finished and its result was available
// anyway.
//
// Measured 2026-09-06 over the HTTP API: a workflow that set phase="started"
// and then slept read back "" for phase while suspended, and "finished" only
// after it completed.
//
// This runs on every registered backend deliberately. The omission was
// identical in all three procedures, which is what a shared template produces
// and what a single-dialect test would have missed twice.
func TestQueryStateSurvivesASuspension(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "qs-suspend", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: %v (wf=%v)", err, wf)
			}

			// Suspend, carrying the state the workflow published before it
			// went to sleep.
			published := map[string]string{"phase": "started", "tag": "abc"}
			farFuture := time.Now().Add(1 * time.Hour)
			if err := store.FinalizeWorkflowSegment(ctx, id, "worker-1", wf.Generation, nil, "ready", "", "", "", published, farFuture); err != nil {
				t.Fatalf("FinalizeWorkflowSegment(ready): %v", err)
			}

			for key, want := range published {
				got, err := store.GetQueryState(ctx, id, key)
				if err != nil {
					t.Fatalf("GetQueryState(%q): %v", key, err)
				}
				if got != want {
					t.Errorf("GetQueryState(%q) = %q, want %q after a SUSPENDING segment.\n"+
						"finalize_workflow_status's 'ready' branch must assign query_state, "+
						"as its 'done' and 'failed' branches do. Without it, queryable state "+
						"reaches the database only when the workflow is already finished -- "+
						"which is the one time nobody needs to query it.", key, got, want)
				}
			}
		})
	}
}

// TestQueryStateAdvancesAcrossSegments pins the half that makes the
// unconditional write safe.
//
// Query state is not a durable event: it is rebuilt by re-executing the body,
// so a resumed segment replays every SetQueryState call made before its
// suspension point and arrives at finalize with the full map. Writing it
// unconditionally therefore cannot lose an earlier segment's value -- it
// rewrites the same keys with the same or newer values.
//
// The case that would break if that reasoning were wrong is a second
// suspension whose map has FEWER keys than the first. That cannot happen from
// replay, but it can from a caller passing nil, so this asserts the observable
// rule directly: what the last segment passed is what a reader sees.
func TestQueryStateAdvancesAcrossSegments(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "qs-segments", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			claimAndSuspend := func(state map[string]string) {
				t.Helper()
				wf, err := store.ClaimWorkflow(ctx, "worker-1")
				if err != nil || wf == nil {
					t.Fatalf("ClaimWorkflow: %v (wf=%v)", err, wf)
				}
				if err := store.FinalizeWorkflowSegment(ctx, id, "worker-1", wf.Generation, nil, "ready",
					"", "", "", state, time.Now().Add(-time.Minute)); err != nil {
					t.Fatalf("FinalizeWorkflowSegment(ready): %v", err)
				}
			}

			claimAndSuspend(map[string]string{"phase": "one"})
			claimAndSuspend(map[string]string{"phase": "two", "extra": "present"})

			for key, want := range map[string]string{"phase": "two", "extra": "present"} {
				got, err := store.GetQueryState(ctx, id, key)
				if err != nil {
					t.Fatalf("GetQueryState(%q): %v", key, err)
				}
				if got != want {
					t.Errorf("after two suspending segments GetQueryState(%q) = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// TestFailWorkflowNilQueryStatePreservesThePriorSegment is cleat#2520's
// FailWorkflow half: a caller with nothing new to say (queryState == nil,
// which is what a panic recovery or a pre-flight failure -- one that never
// ran a replay -- legitimately has) must not erase what an earlier suspending
// segment published. Before this, FailWorkflow's UPDATE set
// query_state = marshalQueryState(nil), which is the literal string "{}", so
// every failed run's published state was wiped regardless of why it failed.
//
// The companion case -- FailWorkflow given an ACTUAL map -- is asserted in
// the same test so a fix that stops writing anything at all (rather than
// distinguishing nil from "nothing new") cannot pass silently.
func TestFailWorkflowNilQueryStatePreservesThePriorSegment(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "qs-fail-nil", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: %v (wf=%v)", err, wf)
			}

			published := map[string]string{"phase": "charging"}
			if err := store.FinalizeWorkflowSegment(ctx, id, "worker-1", wf.Generation, nil, "ready",
				"", "", "", published, time.Now().Add(-time.Minute)); err != nil {
				t.Fatalf("FinalizeWorkflowSegment(ready): %v", err)
			}

			wf2, err := store.ClaimWorkflow(ctx, "worker-2")
			if err != nil || wf2 == nil {
				t.Fatalf("ClaimWorkflow (segment 2): %v (wf=%v)", err, wf2)
			}
			// The nil here is the exact shape of the panic and pre-flight
			// callers in cmd/cleat-worker/setup.go -- no replay ran, so there
			// is nothing new to report.
			if err := store.FailWorkflow(ctx, id, "worker-2", wf2.Generation, "boom", "unknown", "op", nil); err != nil {
				t.Fatalf("FailWorkflow(nil): %v", err)
			}

			got, err := store.GetQueryState(ctx, id, "phase")
			if err != nil {
				t.Fatalf("GetQueryState after FailWorkflow(nil): %v", err)
			}
			if got != "charging" {
				t.Errorf("GetQueryState(phase) = %q after FailWorkflow(nil), want %q preserved from "+
					"the prior segment -- a caller with nothing new to say must not erase what was there",
					got, "charging")
			}

			// Companion: a SECOND run, failed with a real map, must still be
			// written -- nil is the only value that means "leave it alone".
			id2, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "qs-fail-real", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (companion): %v", err)
			}
			wf3, err := store.ClaimWorkflow(ctx, "worker-3")
			if err != nil || wf3 == nil {
				t.Fatalf("ClaimWorkflow (companion): %v (wf=%v)", err, wf3)
			}
			if err := store.FailWorkflow(ctx, id2, "worker-3", wf3.Generation, "boom", "unknown", "op",
				map[string]string{"phase": "failed-with-state"}); err != nil {
				t.Fatalf("FailWorkflow(real map): %v", err)
			}
			got2, err := store.GetQueryState(ctx, id2, "phase")
			if err != nil {
				t.Fatalf("GetQueryState after FailWorkflow(real map): %v", err)
			}
			if got2 != "failed-with-state" {
				t.Errorf("GetQueryState(phase) = %q after FailWorkflow with a real map, want %q written -- "+
					"a fix that stops writing anything on failure would pass the nil case above too",
					got2, "failed-with-state")
			}
		})
	}
}

// TestMoveToDeadLetterQueueNilQueryStatePreservesThePriorSegment is
// TestFailWorkflowNilQueryStatePreservesThePriorSegment's DLQ twin -- cleat#2650.
// A dead-lettered run with nothing new to report must not erase what an
// earlier successful segment finalized, and one that publishes fresh state on
// the very segment that exhausts its retries must have that state written,
// since no earlier segment will ever get another chance to persist it.
func TestMoveToDeadLetterQueueNilQueryStatePreservesThePriorSegment(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "qs-dlq-nil", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: %v (wf=%v)", err, wf)
			}

			published := map[string]string{"phase": "charging"}
			if err := store.FinalizeWorkflowSegment(ctx, id, "worker-1", wf.Generation, nil, "ready",
				"", "", "", published, time.Now().Add(-time.Minute)); err != nil {
				t.Fatalf("FinalizeWorkflowSegment(ready): %v", err)
			}

			wf2, err := store.ClaimWorkflow(ctx, "worker-2")
			if err != nil || wf2 == nil {
				t.Fatalf("ClaimWorkflow (segment 2): %v (wf=%v)", err, wf2)
			}
			// nil is the panic/pre-flight shape: no replay ran on this
			// segment, so there is nothing new to report.
			if err := store.MoveToDeadLetterQueue(ctx, id, "worker-2", wf2.Generation, "retries exhausted", "retries_exhausted", "op", nil); err != nil {
				t.Fatalf("MoveToDeadLetterQueue(nil): %v", err)
			}

			got, err := store.GetQueryState(ctx, id, "phase")
			if err != nil {
				t.Fatalf("GetQueryState after MoveToDeadLetterQueue(nil): %v", err)
			}
			if got != "charging" {
				t.Errorf("GetQueryState(phase) = %q after MoveToDeadLetterQueue(nil), want %q preserved "+
					"from the prior segment -- a caller with nothing new to say must not erase what was there",
					got, "charging")
			}

			// Companion: a run dead-lettered on the SAME segment that
			// published new state -- no earlier segment finalized, so this is
			// the only write that will ever persist it. This is cleat#2650's
			// actual defect: before the fix, MoveToDeadLetterQueue's UPDATE
			// had no query_state clause at all, so this value never reached
			// the database no matter what the caller passed.
			id2, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "qs-dlq-real", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (companion): %v", err)
			}
			wf3, err := store.ClaimWorkflow(ctx, "worker-3")
			if err != nil || wf3 == nil {
				t.Fatalf("ClaimWorkflow (companion): %v (wf=%v)", err, wf3)
			}
			if err := store.MoveToDeadLetterQueue(ctx, id2, "worker-3", wf3.Generation, "retries exhausted", "retries_exhausted", "op",
				map[string]string{"phase": "dead-lettered-with-state"}); err != nil {
				t.Fatalf("MoveToDeadLetterQueue(real map): %v", err)
			}
			got2, err := store.GetQueryState(ctx, id2, "phase")
			if err != nil {
				t.Fatalf("GetQueryState after MoveToDeadLetterQueue(real map): %v", err)
			}
			if got2 != "dead-lettered-with-state" {
				t.Errorf("GetQueryState(phase) = %q after MoveToDeadLetterQueue with a real map, want %q "+
					"written -- the final failing replay's own published state was dropped on the way to the store",
					got2, "dead-lettered-with-state")
			}
		})
	}
}
