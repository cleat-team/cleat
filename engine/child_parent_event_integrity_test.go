package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestCompletingAChildLeavesTheParentsChainVerifiable is the property a parent
// needs in order to resume after awaiting a child.
//
// finalize_workflow_status used to inject the completing child's result into
// the PARENT's await_child event row and leave the row's checksum untouched.
// computeEventChecksum hashes the whole payload, Response included, and chains
// each event onto the previous one -- so a row rewritten by another workflow
// invalidated its own checksum and every one after it. The parent's next
// segment recomputed, disagreed, and the workflow failed permanently:
//
//	checksum verification failed: verify events: workflow <id> step 1:
//	checksum mismatch (expected a0212ee0fc7b6167, got d66082e106bc2725)
//
// Measured 2026-09-06 over the HTTP API, 3 runs of 3. Watched directly, the
// row went from (checksum 5bbfe122a91ba25f, response "") to the same checksum
// with response {"tag":"cp3-4753"}.
//
// Only single AwaitChild was affected -- the predicate matched event_type
// 'await_child', which AwaitAllChildren and AwaitAnyChild do not write, which
// is why the fan-out tests passed throughout. And the singular call had no
// test: test_a_single_child_round_trips in the port suite goes through the
// fan-out workflow with n=1, so it exercises AwaitAllChildren.
//
// The window also has to be open. A child that finishes inside the parent's
// first segment is taken by AwaitChild's "already completed" path, which
// records the result itself and never suspends. This test therefore suspends
// the parent explicitly before completing the child, because a version that
// completed the child first would pass against the defect.
func TestCompletingAChildLeavesTheParentsChainVerifiable(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "cpi-parent", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (parent): %v", err)
			}
			parentWF, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || parentWF == nil {
				t.Fatalf("ClaimWorkflow (parent): %v (wf=%v)", err, parentWF)
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "abandon", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			// The parent's segment: it spawned a child and awaited it while
			// the child was still running, so the await_child event carries no
			// response. This is the shape the injection targeted.
			events := []EventRecord{
				{Step: 0, EventType: EventTypeChildWorkflow, RunID: childID, TimestampMs: time.Now().UnixMilli()},
				{Step: 1, EventType: EventTypeAwaitChild, RunID: childID, TimestampMs: time.Now().UnixMilli()},
			}
			if err := store.FinalizeWorkflowSegment(ctx, parentID, "worker-1", parentWF.Generation, events,
				"ready", "", "", "", nil, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (parent suspend): %v", err)
			}

			if err := store.VerifyWorkflowEvents(ctx, parentID); err != nil {
				t.Fatalf("the parent's chain did not verify before the child finished, "+
					"so this test cannot say anything about the child's effect: %v", err)
			}

			// Now complete the child. This is the moment the injection fired.
			childWF, err := store.ClaimWorkflow(ctx, "worker-2")
			if err != nil || childWF == nil {
				t.Fatalf("ClaimWorkflow (child): %v (wf=%v)", err, childWF)
			}
			if childWF.ID != childID {
				t.Fatalf("claimed %s, expected the child %s", childWF.ID, childID)
			}
			// FinalizeWorkflowSegment, not CompleteWorkflow. They are
			// different paths: CompleteWorkflow issues its own UPDATE and
			// never calls finalize_workflow_status, so it never ran the
			// injection -- a version of this test written against it passed
			// with the defect fully present, which is worse than no test.
			// The worker completes a workflow through FinalizeWorkflowSegment
			// (cmd/cleat-worker/setup.go), so that is the path under test.
			if err := store.FinalizeWorkflowSegment(ctx, childID, "worker-2", childWF.Generation, nil,
				"done", `{"tag":"done"}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (child done): %v", err)
			}

			if err := store.VerifyWorkflowEvents(ctx, parentID); err != nil {
				t.Errorf("completing a child invalidated the parent's event chain: %v\n"+
					"finalize_workflow_status must not write into another workflow's "+
					"event_history rows. The checksum covers the whole event payload and "+
					"chains onto the previous event, so a row rewritten by anyone but its "+
					"owner breaks that row and every one after it -- and the parent then "+
					"fails permanently on its next segment. AwaitChild's replay branch "+
					"already re-checks the child when the response is empty, so nothing "+
					"needs the injection. cleat#845", err)
			}
		})
	}
}
