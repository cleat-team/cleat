package engine

import (
	"context"
	"testing"
)

// cleat#3245 Phase 3 step 3 piece E: GetChildCount and OriginalChildRunIDs
// now read workflow_leases rather than workflow_instances.
//
// Same shape as pieces A/C/D's deliberately-diverging tests: ordinary use
// keeps the two tables in sync, so a test that only exercises normal writes
// cannot tell "reads workflow_leases" apart from "reads workflow_instances"
// -- both readings agree. These tests diverge workflow_leases directly
// (bypassing every write path that would mirror the change), so the two
// readings disagree about the outcome, and the result says which table was
// actually read.

func TestGetChildCountReadsFromLeasesNotInstances(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "child-count-reads-leases-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	parentID, _, err := store.StartNewRun(ctx, "", defName, 1, []byte(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (parent): %v", err)
	}

	if _, err := store.StartChildWorkflow(ctx, parentID, defName, `{}`, 1, "ABANDON", 0); err != nil {
		t.Fatalf("StartChildWorkflow (child1): %v", err)
	}
	child2, err := store.StartChildWorkflow(ctx, parentID, defName, `{}`, 1, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow (child2): %v", err)
	}

	// Both children are freshly created ('ready', non-terminal), so the
	// true count is 2 on both tables before divergence.
	count, err := store.GetChildCount(ctx, parentID)
	if err != nil {
		t.Fatalf("GetChildCount (pre-divergence): %v", err)
	}
	if count != 2 {
		t.Fatalf("GetChildCount (pre-divergence) = %d, want 2", count)
	}

	// Diverge workflow_leases only: orphan child2 there by clearing its
	// parent_workflow_id, while workflow_instances still carries it.
	// Reading workflow_leases now undercounts to 1; reading
	// workflow_instances would still (wrongly) report 2.
	if _, err := db.Exec(`UPDATE workflow_leases SET parent_workflow_id = NULL WHERE id = $1`, child2); err != nil {
		t.Fatalf("diverge workflow_leases.parent_workflow_id: %v", err)
	}

	count, err = store.GetChildCount(ctx, parentID)
	if err != nil {
		t.Fatalf("GetChildCount (post-divergence): %v", err)
	}
	if count != 1 {
		t.Fatalf("GetChildCount (post-divergence) = %d, want 1 -- a 2 means the read "+
			"used workflow_instances (still carrying child2's parent_workflow_id) instead of workflow_leases", count)
	}
}

func TestOriginalChildRunIDsReadsFromLeasesNotInstances(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "original-child-run-ids-reads-leases-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	parentID, _, err := store.StartNewRun(ctx, "", defName, 1, []byte(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (parent): %v", err)
	}

	childID, err := store.StartChildWorkflow(ctx, parentID, defName, `{}`, 1, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}

	// Pre-divergence: a freshly created child has continued_from NULL on
	// both tables, so it is an "original" run and the true answer includes it.
	ids, err := store.OriginalChildRunIDs(ctx, parentID)
	if err != nil {
		t.Fatalf("OriginalChildRunIDs (pre-divergence): %v", err)
	}
	if len(ids) != 1 || ids[0] != childID {
		t.Fatalf("OriginalChildRunIDs (pre-divergence) = %v, want [%s]", ids, childID)
	}

	// Diverge workflow_leases only: mark the child as continued there,
	// while workflow_instances.continued_from stays NULL. Reading
	// workflow_leases now correctly excludes it; reading workflow_instances
	// would still (wrongly) include it.
	if _, err := db.Exec(`UPDATE workflow_leases SET continued_from = 'some-earlier-run' WHERE id = $1`, childID); err != nil {
		t.Fatalf("diverge workflow_leases.continued_from: %v", err)
	}

	ids, err = store.OriginalChildRunIDs(ctx, parentID)
	if err != nil {
		t.Fatalf("OriginalChildRunIDs (post-divergence): %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("OriginalChildRunIDs (post-divergence) = %v, want [] -- a non-empty result means the read "+
			"used workflow_instances (continued_from still NULL there) instead of workflow_leases", ids)
	}
}
