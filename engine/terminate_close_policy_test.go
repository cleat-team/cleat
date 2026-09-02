package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// IMPROVEMENT-PLAN 3.79. TerminateWorkflow is a terminal transition and did not
// enforce the parent close policy, so terminating a parent left its TERMINATE
// children running -- while force-completing the very same parent failed them.
//
// The sibling test TestEnforceParentClosePolicy covers the policy itself, and
// closes its parent with CompleteWorkflow. This one exists because the policy
// working on one closing path says nothing about another: the gap here was a
// missing call, not a broken policy.
//
// Found by TestParentCloseTerminateReleasesChildConcurrencyKeys' vacuity guard
// (3.80), which used TerminateWorkflow as the obvious way to close a parent and
// caught the child sitting at "ready".
func TestTerminateWorkflowEnforcesParentClosePolicy(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const parentDef = "tcp-parent"
			const childDef = "tcp-child"
			for _, name := range []string{parentDef, childDef} {
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef(%s): %v", name, err)
				}
			}

			stamp := time.Now().UnixNano()
			parentID := fmt.Sprintf("tcp-parent-%d", stamp)
			if _, _, err := store.StartNewRun(ctx, parentID, parentDef, 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun(parent): %v", err)
			}

			children := map[string]string{}
			for i, policy := range []string{"TERMINATE", "REQUEST_CANCEL", "ABANDON"} {
				childID := fmt.Sprintf("tcp-child-%s-%d", policy, stamp)
				event := EventRecord{
					Step: i + 1, EventType: EventTypeChildWorkflow,
					TimestampMs: time.Now().UnixMilli(),
					ChildName:   childDef, ChildInput: `{}`,
				}
				if _, err := store.StartChildWorkflowAtomic(ctx, childID, parentID, childDef, `{}`, 1, policy, event, 0); err != nil {
					t.Fatalf("StartChildWorkflowAtomic(%s): %v", policy, err)
				}
				children[policy] = childID
			}

			// Unlike CompleteWorkflow, this needs no claim: terminate is an
			// operator verb on a workflow nobody owns, which is exactly why it
			// was easy to miss that it skipped the policy.
			if err := store.TerminateWorkflow(ctx, parentID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow(parent): %v", err)
			}

			// Precondition: the parent really did close, so a child left
			// running below cannot be blamed on the terminate not happening.
			parent, err := store.GetWorkflowByID(ctx, parentID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(parent): %v", err)
			}
			if parent.Status != "terminated" {
				t.Fatalf("parent status = %q, want \"terminated\"; the assertions below would be vacuous", parent.Status)
			}

			terminated, err := store.GetWorkflowByID(ctx, children["TERMINATE"])
			if err != nil {
				t.Fatalf("GetWorkflowByID(TERMINATE child): %v", err)
			}
			if terminated.Status != "failed" {
				t.Errorf("TERMINATE child status = %q, want \"failed\".\n\n"+
					"Terminating a parent left its child running. Every other terminal "+
					"path enforces the close policy -- FinalizeWorkflowSegment for "+
					"done/failed, and adminForceResolve, which is an operator verb on an "+
					"unclaimed workflow exactly like terminate. A child outliving the "+
					"parent that owns it is the orphan the policy exists to prevent.",
					terminated.Status)
			}

			flagged, reason, err := store.PollCancellation(ctx, children["REQUEST_CANCEL"])
			if err != nil {
				t.Fatalf("PollCancellation(REQUEST_CANCEL child): %v", err)
			}
			if !flagged {
				t.Errorf("REQUEST_CANCEL child was not flagged for cancellation after the parent was terminated (reason=%q)", reason)
			}
			rc, err := store.GetWorkflowByID(ctx, children["REQUEST_CANCEL"])
			if err != nil {
				t.Fatalf("GetWorkflowByID(REQUEST_CANCEL child): %v", err)
			}
			if rc.Status == "failed" {
				t.Error("REQUEST_CANCEL child was failed outright; the policy asks for cancellation, not termination")
			}

			// ABANDON is the control: if terminate cascaded to everything, this
			// test would pass for the wrong reason.
			abandoned, err := store.GetWorkflowByID(ctx, children["ABANDON"])
			if err != nil {
				t.Fatalf("GetWorkflowByID(ABANDON child): %v", err)
			}
			abandonFlagged, _, err := store.PollCancellation(ctx, children["ABANDON"])
			if err != nil {
				t.Fatalf("PollCancellation(ABANDON child): %v", err)
			}
			if abandoned.Status == "failed" || abandonFlagged {
				t.Errorf("ABANDON child was touched: status=%q cancellation_requested=%v",
					abandoned.Status, abandonFlagged)
			}
		})
	}
}
