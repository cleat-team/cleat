package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// cleat#1103: parent_workflow_id is written on every child spawn and was
// selected by no GetWorkflowByID on any dialect, so no client could ask whose
// child a run is.
//
// One step further along than cleat#1091. There, completed_at reached Go and
// was dropped on the floor; here the column never left the row, so the grep
// that found #1091 -- a local scanned and never assigned -- could not have
// found this. "Is this field returned" and "is this field read" are separate
// questions and a column can fail either one alone.
//
// Against real databases on every dialect, because the claim is about what a
// SELECT asks for.
func TestARunCanSayWhoseChildItIs(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "parentage", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			child, err := store.GetWorkflowByID(ctx, childID)
			if err != nil || child == nil {
				t.Fatalf("GetWorkflowByID(child): %v (nil=%v)", err, child == nil)
			}
			if child.ParentWorkflowID == nil {
				t.Fatalf("a child run reports no parent.\n\n" +
					"parent_workflow_id is written by StartChildWorkflow's INSERT and " +
					"read internally by GetChildCount and enforceParentClosePolicy. It " +
					"was in no GetWorkflowByID SELECT on any dialect, which is cleat#1103.")
			}
			if *child.ParentWorkflowID != parentID {
				t.Errorf("child names parent %q, want %q", *child.ParentWorkflowID, parentID)
			}

			// The partner assertion, and the one that makes the first mean
			// something: a field that returned the run's OWN id, or any
			// non-empty string, would satisfy "a child reports a parent". Only
			// the top-level run distinguishes "reports its parent" from
			// "reports something".
			parent, err := store.GetWorkflowByID(ctx, parentID)
			if err != nil || parent == nil {
				t.Fatalf("GetWorkflowByID(parent): %v (nil=%v)", err, parent == nil)
			}
			if parent.ParentWorkflowID != nil {
				t.Errorf("a run nobody spawned reports parent %q, want nil.\n\n"+
					"Absence is the answer for every top-level run. A non-nil here "+
					"means the column is being filled with something -- the run's own "+
					"id, or an empty string surfacing as a value -- rather than read.",
					*parent.ParentWorkflowID)
			}

			// Not ContinuedFrom. The two name opposite relations and the API
			// exposed only the other one before this change; a test that let
			// them collide would not notice if a scan crossed them.
			if child.ContinuedFrom != "" {
				t.Errorf("child reports continued_from %q; a child is not a continuation",
					child.ContinuedFrom)
			}
		})
	}
}
