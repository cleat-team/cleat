package engine

import (
	"context"
	"testing"
	"time"
)

// IMPROVEMENT-PLAN 3.80. releaseWorkflowResources states its own contract: it
// runs the two best-effort cleanups that follow "every commit which takes a
// workflow out of the runnable set: completion, FAILURE, termination,
// continue-as-new, and the admin actions".
//
// enforceParentClosePolicy's TERMINATE arm commits exactly such a failure --
// UPDATE workflow_instances SET status = 'failed', error_msg = 'parent workflow
// terminated' -- for every child of a closing parent, and releases nothing on
// any of the three dialects.
//
// This is the same class as 3.76 and was found the same way, by re-deriving
// 3.75's inventory rather than trusting it. It differs in two respects worth
// knowing: it is uniform across dialects rather than one dialect disagreeing
// with two, so no cross-dialect comparison could surface it -- only the
// contract could -- and it is a bulk operation, so one closing parent can strand
// a slot per child rather than one slot.
//
// The assertion is on released STATE rather than on the call, so an
// implementation that frees the slot by another route rightly passes.
func TestParentCloseTerminateReleasesChildConcurrencyKeys(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			deployConcurrencyTestWorkflows(t, store, "wf-parent", "wf-heir")

			childID, err := store.StartChildWorkflow(ctx, "wf-parent",
				"concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			// The child takes the only slot for key-child.
			acquired, err := store.AcquireConcurrencyKey(ctx, "key-child", childID, time.Hour)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey: %v", err)
			}
			if !acquired {
				t.Fatal("the first acquire must succeed, or the rest of this test is vacuous")
			}

			// Control: while the child holds it, nobody else can. Without this
			// the test cannot tell a released slot from one never held.
			taken, err := store.AcquireConcurrencyKey(ctx, "key-child", "wf-heir", time.Hour)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (contended): %v", err)
			}
			if taken {
				t.Fatal("a second workflow acquired a held key, so this test is vacuous")
			}

			// Close the parent through a path that actually enforces the close
			// policy. TerminateWorkflow does NOT -- only FinalizeWorkflowSegment
			// and adminForceResolve call enforceParentClosePolicy, which the
			// vacuity guard below caught when this test first used it: the child
			// stayed "ready" and the key assertion would have passed for the
			// wrong reason. Recorded in 3.79; it is a separate question from the
			// release gap this test is about.
			if err := store.AdminForceComplete(ctx, "wf-parent", 0, `{}`, "test"); err != nil {
				t.Fatalf("AdminForceComplete(parent): %v", err)
			}

			// Precondition for the real assertion: the policy actually fired.
			// If it did not, the key being held says nothing about releasing.
			child, err := store.GetWorkflowByID(ctx, childID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(child): %v", err)
			}
			if child.Status != "failed" {
				t.Fatalf("the close policy did not fire: child status = %q, want \"failed\"; "+
					"the key assertion below would be vacuous", child.Status)
			}

			freed, err := store.AcquireConcurrencyKey(ctx, "key-child", "wf-heir", time.Hour)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (after parent close): %v", err)
			}
			if !freed {
				t.Errorf("closing the parent failed the child but did not release its "+
					"concurrency key (child %s).\n\n"+
					"releaseWorkflowResources' contract names failure explicitly, and the "+
					"TERMINATE arm commits one for every child of the closing parent. The "+
					"slot stays held until concurrency_keys.expires_at, and every workflow "+
					"queued on that key waits out the TTL for nothing -- once per child, so "+
					"a parent with many children strands many slots at once.", childID)
			}
		})
	}
}
