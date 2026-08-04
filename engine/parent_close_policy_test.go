package engine

// IMPROVEMENT-PLAN.md 2.50. enforceParentClosePolicy is what applies a closing
// parent's policy to its children -- TERMINATE children are failed,
// REQUEST_CANCEL children get the cancellation flag, ABANDON children are left
// alone. On every dialect it discarded every error it produced, so a failure
// was structurally invisible.
//
// Before fixing the error handling: does the feature work at all? Nothing in
// the repo tested it. The only existing references assert that this SQL is
// *absent* on the fence-lost path (fence_lost_unit_test.go), which says
// nothing about whether it does the right thing when it is supposed to run.
//
// Runs against every configured backend.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestEnforceParentClosePolicy checks the contract across all three policies at
// once, so that a fix which handles one and breaks another cannot pass.
func TestEnforceParentClosePolicy(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const parentDef = "pcp-parent"
			const childDef = "pcp-child"
			for _, name := range []string{parentDef, childDef} {
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef(%s): %v", name, err)
				}
			}

			stamp := time.Now().UnixNano()
			parentID := fmt.Sprintf("pcp-parent-%d", stamp)
			if _, _, err := store.StartNewRun(ctx, parentID, parentDef, 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun(parent): %v", err)
			}

			// The parent has to be owned before it can be completed: the
			// terminal write is fenced on (assigned_to, generation).
			claimed, err := store.ClaimWorkflows(ctx, "worker-pcp", 10)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			var parent *WorkflowInstance
			for _, wf := range claimed {
				if wf.ID == parentID {
					parent = wf
				}
			}
			if parent == nil {
				t.Fatalf("the parent was not claimed; got %d workflow(s)", len(claimed))
			}

			children := map[string]string{} // policy -> child ID
			for i, policy := range []string{"TERMINATE", "REQUEST_CANCEL", "ABANDON"} {
				childID := fmt.Sprintf("pcp-child-%s-%d", policy, stamp)
				event := EventRecord{
					Step:        i + 1,
					EventType:   EventTypeChildWorkflow,
					TimestampMs: time.Now().UnixMilli(),
					ChildName:   childDef,
					ChildInput:  `{}`,
				}
				if _, err := store.StartChildWorkflowAtomic(ctx, childID, parentID, childDef, `{}`, 1, policy, event, 0); err != nil {
					t.Fatalf("StartChildWorkflowAtomic(%s): %v", policy, err)
				}
				children[policy] = childID
			}

			// Closing the parent is what triggers enforcement.
			if err := store.CompleteWorkflow(ctx, parentID, "worker-pcp", parent.Generation, `{}`, nil); err != nil {
				t.Fatalf("CompleteWorkflow(parent): %v", err)
			}

			// TERMINATE: the child must be failed, with the reason recorded.
			terminated, err := store.GetWorkflowByID(ctx, children["TERMINATE"])
			if err != nil {
				t.Fatalf("GetWorkflowByID(TERMINATE child): %v", err)
			}
			if terminated.Status != "failed" {
				t.Errorf("TERMINATE child status = %q, want %q -- the parent closed and its policy was not applied",
					terminated.Status, "failed")
			}
			if terminated.Error == "" {
				t.Errorf("TERMINATE child has no error_msg; a child failed by its parent's policy should say so")
			}

			// REQUEST_CANCEL: the child keeps running but is flagged.
			cancelled, reason, err := store.PollCancellation(ctx, children["REQUEST_CANCEL"])
			if err != nil {
				t.Fatalf("PollCancellation(REQUEST_CANCEL child): %v", err)
			}
			if !cancelled {
				t.Errorf("REQUEST_CANCEL child was not flagged for cancellation after its parent closed (reason=%q)", reason)
			}
			rc, err := store.GetWorkflowByID(ctx, children["REQUEST_CANCEL"])
			if err != nil {
				t.Fatalf("GetWorkflowByID(REQUEST_CANCEL child): %v", err)
			}
			if rc.Status == "failed" {
				t.Errorf("REQUEST_CANCEL child was failed outright; the policy asks for cancellation, not termination")
			}

			// ABANDON: untouched.
			abandoned, err := store.GetWorkflowByID(ctx, children["ABANDON"])
			if err != nil {
				t.Fatalf("GetWorkflowByID(ABANDON child): %v", err)
			}
			if abandoned.Status == "failed" {
				t.Errorf("ABANDON child was failed; its policy is to outlive the parent")
			}
			abandonedCancelled, _, err := store.PollCancellation(ctx, children["ABANDON"])
			if err != nil {
				t.Fatalf("PollCancellation(ABANDON child): %v", err)
			}
			if abandonedCancelled {
				t.Errorf("ABANDON child was flagged for cancellation; its policy is to outlive the parent")
			}
		})
	}
}
