package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#3245 Phase 3 step 3 piece A: ReapStaleInstances, ReapStaleInstancesExcept
// and ListStaleHolders now read their candidate set from workflow_leases, not
// workflow_instances.
//
// Every other test in this tree seeds both tables identically (that is the
// whole point of the dual-write pieces 1/2/3/6b this one builds on), which
// means none of them can tell "reads workflow_leases" apart from "reads
// workflow_instances" -- both agree, so either reading would pass. These
// tests deliberately DIVERGE the two tables' heartbeat_at after seeding, so
// the two readings disagree about whether the row is stale, and the result
// says which table the scan actually used.
//
// Reuses seedClaimedTriple (piece 4a's file, already merged) and
// retryAndAdminLeaseTestStore (piece 4c's file, already merged).

func TestReapStaleInstancesReadsHeartbeatFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "stale-scan-reads-leases-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "stale-scan-reads-leases-wf-1"
	seedClaimedTriple(t, db, id, defName, "worker-stale-scan-1", 1)

	// Diverge: workflow_instances looks FRESH (would not be reaped if read),
	// workflow_leases looks STALE (would be reaped if read). If the scan
	// still reads workflow_instances, this row survives; if it reads
	// workflow_leases, it does not.
	if _, err := db.Exec(`UPDATE workflow_instances SET heartbeat_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_instances.heartbeat_at fresh: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_leases SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_leases.heartbeat_at stale: %v", err)
	}

	n, err := store.ReapStaleInstances(ctx, 30*time.Second, 0)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d, want 1 -- it must have read the STALE "+
			"workflow_leases.heartbeat_at, not the fresh workflow_instances one", n)
	}

	var leaseStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_leases WHERE id = $1`, id).Scan(&leaseStatus); err != nil {
		t.Fatalf("read workflow_leases.status: %v", err)
	}
	if leaseStatus != "ready" {
		t.Errorf("workflow_leases.status = %q, want %q (reclaimed)", leaseStatus, "ready")
	}
}

// TestReapStaleInstancesIgnoresAStaleInstancesHeartbeatWhenLeasesIsFresh is
// the mirror image and the sharper assertion: a row that LOOKS stale on
// workflow_instances but is FRESH on workflow_leases must NOT be reclaimed.
// A scan that still reads workflow_instances even partially (a leftover
// predicate, a UNION, a fallback) would reclaim this row; one that reads
// workflow_leases exclusively will not.
func TestReapStaleInstancesIgnoresAStaleInstancesHeartbeatWhenLeasesIsFresh(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "stale-scan-ignores-instances-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "stale-scan-ignores-instances-wf-1"
	seedClaimedTriple(t, db, id, defName, "worker-stale-scan-2", 1)

	// The instance copy is planted as a stale running run, which a scan that
	// read it would reclaim. seedClaimedTriple writes a narrow instance row
	// (cleat#3245 B4a), so status is planted here too, not just heartbeat_at.
	if _, err := db.Exec(`UPDATE workflow_instances SET status = 'running', heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_instances stale and running: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_leases SET heartbeat_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_leases.heartbeat_at fresh: %v", err)
	}

	n, err := store.ReapStaleInstances(ctx, 30*time.Second, 0)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 0 {
		t.Fatalf("ReapStaleInstances reclaimed %d, want 0 -- workflow_leases.heartbeat_at is "+
			"fresh, so a scan reading workflow_leases must leave this row alone regardless of "+
			"what workflow_instances.heartbeat_at says", n)
	}

	var instStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, id).Scan(&instStatus); err != nil {
		t.Fatalf("read workflow_instances.status: %v", err)
	}
	if instStatus != "running" {
		t.Errorf("workflow_instances.status = %q, want %q (untouched)", instStatus, "running")
	}
}

func TestListStaleHoldersReadsHeartbeatFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "list-stale-holders-reads-leases-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "list-stale-holders-reads-leases-wf-1"
	seedClaimedTriple(t, db, id, defName, "worker-list-stale-1", 1)

	if _, err := db.Exec(`UPDATE workflow_instances SET heartbeat_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_instances.heartbeat_at fresh: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_leases SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_leases.heartbeat_at stale: %v", err)
	}

	holders, err := store.ListStaleHolders(ctx, 30*time.Second, 0)
	if err != nil {
		t.Fatalf("ListStaleHolders: %v", err)
	}
	found := false
	for _, h := range holders {
		if h.Key.WorkflowID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListStaleHolders did not report %s, want it listed -- it must have read the "+
			"STALE workflow_leases.heartbeat_at, not the fresh workflow_instances one", id)
	}
}

// TestReapStaleInstancesExceptReadsHeartbeatFromLeasesNotInstances covers the
// second production call site this piece touches -- its own candidate SELECT
// and NOT EXISTS correlation both moved to workflow_leases, which a fix to
// ReapStaleInstances alone would not exercise.
func TestReapStaleInstancesExceptReadsHeartbeatFromLeasesNotInstances(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "reap-except-reads-leases-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "reap-except-reads-leases-wf-1"
	seedClaimedTriple(t, db, id, defName, "worker-reap-except-1", 1)

	if _, err := db.Exec(`UPDATE workflow_instances SET heartbeat_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_instances.heartbeat_at fresh: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_leases SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_leases.heartbeat_at stale: %v", err)
	}

	n, err := store.ReapStaleInstancesExcept(ctx, 30*time.Second, 0, nil)
	if err != nil {
		t.Fatalf("ReapStaleInstancesExcept: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstancesExcept reclaimed %d, want 1 -- it must have read the STALE "+
			"workflow_leases.heartbeat_at, not the fresh workflow_instances one", n)
	}
}
