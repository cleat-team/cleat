package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// How far down does terminating a parent reach?
//
// TestTerminateWorkflowEnforcesParentClosePolicy proves the cascade fires at ONE
// level: a TERMINATE child of a terminated parent is failed. It says nothing
// about the level below, and the question is not idle -- upstream engines treat
// recursion as a choice and test both branches (microsoft/durabletask-go's
// Test_TerminateOrchestration_Recursive builds Root -> 5x L1 -> L2 and asserts
// the L2 activity runs iff recursion is off).
//
// THIS TEST EXISTS BECAUSE READING THE CODE GAVE AN ANSWER AND READING IS NOT
// ENOUGH. enforceParentClosePolicy's TERMINATE arm sets `status = 'failed'` on
// the child with a direct UPDATE. That does not go through FailWorkflow, and
// FailWorkflow is what calls enforceParentClosePolicy for the next level down.
// So the reading predicts the cascade stops at one level. MEASURED, IT DOES, on
// all three dialects -- which is what this test now pins.
//
// THE READING ALSO PREDICTED AN EXCEPTION THAT IS NOT TESTED HERE, and it is
// flagged rather than quietly carried: the defer-phase arm sets
// `status = 'terminating'` instead of failing the child outright, and those
// children are finalised later through a path that can cascade again. If that
// is right, depth depends on whether an intermediate workflow happened to owe
// defers, which is not a property anyone would choose. The children in this
// fixture owe no defers, so they take the direct arm and this test says nothing
// about it. Someone should build the variant; until then it is a reading, not a
// result.
//
// A prediction of exactly that shape -- three correct file:line citations, a
// chain with no branch in it -- was published in this repo's port work-lists
// about blank Idempotency-Key values and was wrong, because the chain had a hop
// nobody knew to look for (Go trims header values). So this asserts what the
// tree actually does rather than what the code appears to say.
//
// It is deliberately written to record the behaviour rather than to demand a
// particular one. Whether one-level is CORRECT is a product question: a subtree
// terminate is a recursive UPDATE per dialect, and a detached child must not
// inherit it. What is not defensible is nobody knowing which it is.
func TestTerminateCascadeDepthIsOneLevel(t *testing.T) {
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

			// THE MEASUREMENT. Recorded, not demanded: the assertion is that the
			// grandchild is untouched, which is what one-level means. If this
			// goes red because the grandchild WAS reached, the cascade is deeper
			// than one level and this test should be inverted -- not deleted.
			grandFlagged, _, err := store.PollCancellation(ctx, grandID)
			if err != nil {
				t.Fatalf("PollCancellation(grandchild): %v", err)
			}
			if grand.Status == "failed" || grand.Status == statusTerminating || grandFlagged {
				t.Errorf("the grandchild WAS reached: status=%q cancellation_requested=%v.\n\n"+
					"The cascade is deeper than one level, which is better than this test "+
					"assumed and means enforceParentClosePolicy runs again for each child "+
					"it closes. Invert this test and say so at the site -- do not delete "+
					"it, because the depth is the thing worth pinning either way.",
					grand.Status, grandFlagged)
			}
		})
	}
}
