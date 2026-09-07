package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// TestAPolicyTerminatedChildRecordsWhenItCompleted asserts that a child closed
// by its parent's ParentClosePolicy TERMINATE gets a completed_at, like every
// other terminal write in the tree.
//
// # The defect
//
// The first TERMINATE arm of enforceParentClosePolicy sets status='failed' --
// terminal, and where the row stays -- and set no completed_at, in all three
// dialects identically. Measured on postgres before the fix, through the real
// store:
//
//	terminated child:              status="failed"  completed_at=<nil>
//	parent (ordinary finalize):    status="done"    completed_at=2026-09-07 01:40:20 +0000 UTC
//
// So a workflow that was definitively over read as never having completed. That
// is not a cosmetic gap: cleat#847 proposes deriving a child's observed status
// from completed_at, and this arm is the one MOST children take -- it is the
// arm for children that owe no cleanup.
//
// Noticed and deliberately deferred in cleat#848, which fixed the same arm's
// missing generation bump and said in its own body that this belonged with
// #847 rather than with a fence fix.
//
// # Why the gap is exactly here and nowhere else
//
// Swept from both directions by WS-3 over the whole repo, and the two agree on
// membership as well as on count: 30-32 terminal writes, of which three lacked
// completed_at, and they are this one function mirrored across the dialects.
// The loose pass deliberately covered `.sql` as well as `.go`, because this
// repo's §1.1 story is a defect inside a stored procedure that no Go-only scan
// would have seen -- all 8 procedure-side terminal writes set it. So this is
// three lines, not a class.
//
// # This test reads the column directly
//
// completed_at is not a field of WorkflowInstance, so GetWorkflowByID cannot
// see it and a test written through the store API would assert nothing about
// the thing that was broken. It goes to the raw handle instead, which is also
// why it carries its own placeholder switch.
func TestAPolicyTerminatedChildRecordsWhenItCompleted(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "pcp-completed-at-parent", DefaultTenantUUID, 0)
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

			// Closing the parent runs enforceParentClosePolicy. The child owes
			// no cleanup, so it takes the first TERMINATE arm -- the one this
			// test is about. A child that owed a defer phase would take the
			// second arm and go to 'terminating', which is NOT terminal and
			// correctly has no completed_at yet.
			if err := store.FinalizeWorkflowSegment(ctx, parentID, "worker-parent", parentWF.Generation, nil,
				"done", `{}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (parent done): %v", err)
			}

			childStatus, childCompletedAt := statusAndCompletedAt(t, store, childID)
			if childStatus != "failed" {
				t.Fatalf("child status = %q after the parent closed, want \"failed\" -- "+
					"this test asserts nothing unless the child actually took the TERMINATE arm",
					childStatus)
			}
			if childCompletedAt == nil {
				t.Errorf("a child terminated by its parent's close policy has status=%q and "+
					"completed_at=NULL.\n\n"+
					"It is terminal, so nothing will ever set it: the row stays this way. Anything "+
					"deriving a workflow's observed outcome from completed_at (cleat#847) reads "+
					"this as never having completed, which is exactly backwards for a workflow "+
					"that was terminated. Every other terminal write in the tree -- 30 in Go and 8 "+
					"in stored procedures -- sets it.", childStatus)
			}

			// The parent is the control. It reached its terminal status by the
			// ordinary finalize path, which always set completed_at, so a
			// version of this test that could not read the column at all would
			// fail here rather than passing vacuously.
			parentStatus, parentCompletedAt := statusAndCompletedAt(t, store, parentID)
			if parentStatus != "done" {
				t.Fatalf("parent status = %q, want \"done\"", parentStatus)
			}
			if parentCompletedAt == nil {
				t.Fatalf("the parent completed through the ordinary finalize path and has no " +
					"completed_at either, so this test is not reading the column it thinks it is")
			}
		})
	}
}

// statusAndCompletedAt reads the two columns straight from the row.
// completed_at is not exposed on WorkflowInstance, so there is no store method
// that could answer this.
func statusAndCompletedAt(t *testing.T, store WorkflowStore, workflowID string) (string, *time.Time) {
	t.Helper()
	db := rawDBOf(t, store)

	var ph string
	switch store.(type) {
	case *PostgresStore:
		ph = "$1"
	case *MySQLStore:
		ph = "?"
	case *MSSQLStore:
		ph = "@p1"
	default:
		t.Fatalf("statusAndCompletedAt: unsupported store type %T", store)
	}

	var status string
	var completedAt *time.Time
	err := db.QueryRow(
		`SELECT status, completed_at FROM workflow_instances WHERE id = `+ph, workflowID,
	).Scan(&status, &completedAt)
	if err == sql.ErrNoRows {
		t.Fatalf("no workflow_instances row for %s", workflowID)
	}
	if err != nil {
		t.Fatalf("reading status/completed_at for %s: %v", workflowID, err)
	}
	return status, completedAt
}
