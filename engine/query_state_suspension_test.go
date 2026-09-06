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
