package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Terminating a parent reaches its whole subtree, not just its children.
//
// IT DID NOT, AND THE HISTORY IS THE POINT. enforceParentClosePolicy's plain arm
// closes a TERMINATE child with a bulk
// `UPDATE ... SET status='failed' WHERE parent_workflow_id = $1`. That never goes
// through FailWorkflow -- and FailWorkflow's post-commit is what enforces the
// close policy on the closed workflow's OWN children. So the recursion point was
// bypassed by the mechanism doing the closing, and a grandchild of a terminated
// root kept running with no parent (IMPROVEMENT-PLAN 3.410, cleat#1108).
//
// The defer arm never had the bug, which is what exposed it: a child owing a
// defer phase is finalised by FinalizeDeferPhase, which DOES enforce the policy.
// So the depth of a terminate depended on whether a workflow in the middle
// happened to have deferred work -- invisible to whoever pressed terminate, and
// changing the day that workflow gained or lost a defer (3.411). Two arms of one
// policy disagreeing, with nothing comparing them.
//
// The rule the fix encodes: CLOSING A WORKFLOW MUST DO WHAT FailWorkflow DOES
// AFTER ITS COMMIT -- release its resources, then enforce its close policy. The
// bulk UPDATE cannot BE FailWorkflow (that fences on `assigned_to` and
// `generation`, and the cascade deliberately closes children it does not own and
// breaks their fence so the holder cannot overwrite the termination), so the
// post-commit half is applied to each child instead. See
// cascadeIntoClosedChildren.
//
// This file previously asserted the one-level behaviour and said, in its own
// failure message, to invert it rather than delete it if the grandchild was ever
// reached. That is what happened.
func TestTerminateCascadeReachesEveryDescendant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const def = "tcd-wf"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			stamp := time.Now().UnixNano()
			rootID := fmt.Sprintf("tcd-root-%d", stamp)
			childID := fmt.Sprintf("tcd-child-%d", stamp)
			grandID := fmt.Sprintf("tcd-grand-%d", stamp)

			if _, _, err := store.StartNewRun(ctx, rootID, def, 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun(root): %v", err)
			}
			spawn := func(childID, parentID string, step int) {
				t.Helper()
				event := EventRecord{
					Step: step, EventType: EventTypeChildWorkflow,
					TimestampMs: time.Now().UnixMilli(),
					ChildName:   def, ChildInput: `{}`,
				}
				if _, err := store.StartChildWorkflowAtomic(
					ctx, childID, parentID, def, `{}`, 1, "TERMINATE", event, 0); err != nil {
					t.Fatalf("StartChildWorkflowAtomic(%s under %s): %v", childID, parentID, err)
				}
			}
			spawn(childID, rootID, 1)
			spawn(grandID, childID, 1)

			// CONTROL 1: the tree exists and is shaped the way this test thinks.
			//
			// Without this, "the grandchild was not failed" is also satisfied by
			// a grandchild that was never created, or one parented to the root
			// instead of the child -- in which case the test would be measuring
			// its own fixture. This is the shape that made `attempts=1` a
			// vacuous assertion in cleat-ports#115.
			for _, want := range []struct {
				id, parent string
			}{{rootID, ""}, {childID, rootID}, {grandID, childID}} {
				got, err := store.GetWorkflowByID(ctx, want.id)
				if err != nil {
					t.Fatalf("GetWorkflowByID(%s): %v -- the fixture was not built", want.id, err)
				}
				gotParent := ""
				if got.ParentWorkflowID != nil {
					gotParent = *got.ParentWorkflowID
				}
				if gotParent != want.parent {
					t.Fatalf("%s has parent %q, want %q -- the tree is not three levels "+
						"and every assertion below would be about a different shape",
						want.id, gotParent, want.parent)
				}
				if got.Status == "failed" || got.Status == "done" {
					t.Fatalf("%s is already %q before the terminate", want.id, got.Status)
				}
			}

			if err := store.TerminateWorkflow(ctx, rootID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow(root): %v", err)
			}

			root, err := store.GetWorkflowByID(ctx, rootID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(root): %v", err)
			}
			if root.Status != "terminated" {
				t.Fatalf("root status = %q, want \"terminated\"", root.Status)
			}

			child, err := store.GetWorkflowByID(ctx, childID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(child): %v", err)
			}
			grand, err := store.GetWorkflowByID(ctx, grandID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(grandchild): %v", err)
			}

			// CONTROL 2, and it is the one that makes the result below mean
			// anything. If the cascade did not fire AT ALL, the grandchild would
			// also be untouched and this test would report "one level" while
			// measuring zero.
			if child.Status != "failed" && child.Status != statusTerminating {
				t.Fatalf("the level-1 child is %q, so the cascade did not fire at all "+
					"and this test cannot say anything about depth. See "+
					"TestTerminateWorkflowEnforcesParentClosePolicy, which owns that case.",
					child.Status)
			}

			// THE MEASUREMENT. The grandchild must be closed too: it is a
			// TERMINATE child of a workflow that was just terminated, and
			// nothing about it differs from the level above.
			grandFlagged, _, err := store.PollCancellation(ctx, grandID)
			if err != nil {
				t.Fatalf("PollCancellation(grandchild): %v", err)
			}
			if grand.Status != "failed" && grand.Status != statusTerminating && !grandFlagged {
				t.Errorf("the grandchild is %q and unflagged: the cascade stopped at one "+
					"level and it is running with no parent.\n\n"+
					"Closing a workflow has to do what FailWorkflow does after its commit -- "+
					"release its resources AND enforce its close policy on its own children. "+
					"A bulk UPDATE that only does the first leaves the recursion point "+
					"bypassed by the mechanism doing the closing. See "+
					"cascadeIntoClosedChildren and cleat#1108.",
					grand.Status)
			}
		})
	}
}
