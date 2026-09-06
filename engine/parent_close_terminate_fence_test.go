package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestTerminateClosePolicyFencesOutTheChildsWorker is the property that makes
// ParentClosePolicy TERMINATE mean anything.
//
// enforceParentClosePolicy's TERMINATE arm used to set the child's status and
// error message and nothing else. A worker's claim is (assigned_to,
// generation), finalize_workflow_status is fenced on it, and an UPDATE that
// changes only the status leaves that fence valid -- so the child's own worker
// finalized its next segment, matched, and put the child back to 'ready' and
// then on to 'done'. The error_msg survived, because the 'done' branch does not
// clear it.
//
// The result was a row asserting both things at once:
//
//	status = 'done'   error_msg = 'parent workflow terminated'
//
// Measured 2026-09-06 over the HTTP API: 4 runs of 4, every TERMINATE child ran
// to completion carrying that message. ABANDON and REQUEST_CANCEL were correct
// in the same run -- REQUEST_CANCEL set cancellation_requested and the child
// ignored it, which is cooperative cancellation working as designed.
//
// The defer-phase arm beside it always bumped the generation, for the same
// reason ExpireDeferPhases does. Only this arm lacked it, and it is the arm
// most children take: it is the one for children that owe no cleanup.
//
// The test claims the child before closing the parent, because that is the
// state the defect needs. A child nobody holds is fenced out by an UPDATE that
// does nothing special, so a version of this test that skipped the claim would
// pass against the defect.
func TestTerminateClosePolicyFencesOutTheChildsWorker(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "pcp-parent", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (parent): %v", err)
			}
			parentWF, err := store.ClaimWorkflow(ctx, "worker-parent")
			if err != nil || parentWF == nil {
				t.Fatalf("ClaimWorkflow (parent): %v (wf=%v)", err, parentWF)
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			// A worker is holding the child. Without this the defect is
			// invisible: there is no fence to leave standing.
			childWF, err := store.ClaimWorkflow(ctx, "worker-child")
			if err != nil || childWF == nil {
				t.Fatalf("ClaimWorkflow (child): %v (wf=%v)", err, childWF)
			}
			if childWF.ID != childID {
				t.Fatalf("claimed %s, expected the child %s", childWF.ID, childID)
			}

			// Closing the parent runs enforceParentClosePolicy.
			if err := store.FinalizeWorkflowSegment(ctx, parentID, "worker-parent", parentWF.Generation, nil,
				"done", `{}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (parent done): %v", err)
			}

			after, err := store.GetWorkflowByID(ctx, childID)
			if err != nil || after == nil {
				t.Fatalf("GetWorkflowByID (child): %v (wf=%v)", err, after)
			}
			if after.Status != "failed" {
				t.Fatalf("child status = %q after the parent closed, want \"failed\"", after.Status)
			}

			// The half that was actually broken: the holding worker must no
			// longer be able to write. Its finalize has to lose the fence
			// rather than quietly undo the termination.
			err = store.FinalizeWorkflowSegment(ctx, childID, "worker-child", childWF.Generation, nil,
				"ready", "", "", "", nil, time.Now().Add(time.Hour))
			if !errors.Is(err, ErrFenceLost) {
				t.Errorf("the child's holding worker finalized after termination and got %v, want ErrFenceLost.\n"+
					"TERMINATE must bump the generation and clear assigned_to, or the worker's "+
					"claim stays valid and its next segment writes the child back to a live "+
					"status -- leaving status='done' beside error_msg='parent workflow "+
					"terminated'.", err)
			}

			final, err := store.GetWorkflowByID(ctx, childID)
			if err != nil || final == nil {
				t.Fatalf("GetWorkflowByID (child, final): %v", err)
			}
			if final.Status != "failed" {
				t.Errorf("child status = %q after its old worker tried to finalize, want \"failed\"", final.Status)
			}
		})
	}
}
