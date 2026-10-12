package engine

import (
	"context"
	"strings"
	"testing"
)

// cleat#3245 Phase 3 step 3 piece C: adminResolveMiss (store_admin.go) and
// AdminReReplay's miss-resolve read (store_admin_rereplay.go) now read
// workflow_leases rather than workflow_instances for their zero-row-UPDATE
// diagnostic.
//
// Same shape as piece A's
// TestReapStaleInstancesReadsHeartbeatFromLeasesNotInstances: every other
// admin test seeds both tables identically, so none of them can tell "reads
// workflow_leases" apart from "reads workflow_instances" -- both agree,
// either reading passes. These tests deliberately diverge the two tables'
// generation after seeding, so the two readings disagree about what
// generation is "stored", and the error message says which table the read
// actually used.

func TestAdminResolveMissReadsGenerationFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "admin-miss-reads-leases-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "admin-miss-reads-leases-wf-1"
	seedClaimedTriple(t, db, id, defName, "worker-admin-miss-1", 5)

	// Diverge: workflow_instances.generation stays 5 (the seed), but
	// workflow_leases.generation is bumped to a distinct value. Neither
	// value is the AdminForceComplete call's requested generation below, so
	// the fenced UPDATE on workflow_instances misses regardless of which
	// table the diagnostic reads -- what distinguishes the two readings is
	// which "stored generation is N" the error message reports.
	if _, err := db.Exec(`UPDATE workflow_leases SET generation = 77 WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_leases.generation: %v", err)
	}

	err := store.AdminForceComplete(ctx, id, 999, `{}`, "operator-1")
	if err == nil {
		t.Fatal("AdminForceComplete: expected a generation-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "stored generation is 77") {
		t.Fatalf("AdminForceComplete error = %q, want it to report the workflow_leases "+
			"generation (77), not the workflow_instances one (5) -- the diagnostic read "+
			"must have used workflow_instances instead of workflow_leases", err.Error())
	}
}

func TestAdminReReplayMissReadsGenerationFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "admin-rereplay-miss-reads-leases-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "admin-rereplay-miss-reads-leases-wf-1"
	seedDeadLetteredTriple(t, db, id, defName)

	// seedDeadLetteredTriple leaves generation at 0 on both tables (dead
	// letter has no concept of a claim generation to seed otherwise).
	// Diverge workflow_leases.generation away from that so the two
	// readings disagree.
	if _, err := db.Exec(`UPDATE workflow_leases SET generation = 42 WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_leases.generation: %v", err)
	}

	err := store.AdminReReplay(ctx, id, 999, "operator-1")
	if err == nil {
		t.Fatal("AdminReReplay: expected a generation-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "stored generation is 42") {
		t.Fatalf("AdminReReplay error = %q, want it to report the workflow_leases "+
			"generation (42), not the workflow_instances one (0) -- the diagnostic read "+
			"must have used workflow_instances instead of workflow_leases", err.Error())
	}
}
