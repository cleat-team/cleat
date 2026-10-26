package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// cleat#3245 Phase 3 step 2 gap fix (found scoping step 3's piece A, before
// any read-cutover code was written): (*AdaptiveFlusher).partitionFencedBatch
// renews workflow_instances.heartbeat_at for a batch in one statement, and is
// its own doc's "batch-mode counterpart to Engine.flushEvent's single
// Heartbeat call". Heartbeat (db.go) got piece 3's workflow_leases mirror;
// this one never did, so workflow_leases.heartbeat_at could go stale for a
// workflow kept alive only through this path -- invisible until something
// reads heartbeat_at from workflow_leases instead of workflow_instances,
// which step 3's piece A is about to do.
//
// Uses the real StartNewRun/ClaimWorkflow (pieces 1/2, already merged) to
// reach a genuinely claimed row rather than seeding one via raw SQL: unlike
// every other dual-write test in this tree, the thing under test here is a
// RENEWAL of an existing heartbeat_at, so the precondition has to be a real
// claim with a real initial timestamp to renew, not just the right columns
// present.
func TestPartitionFencedBatchUpdatesTheLeaseRow(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "partition-fenced-batch-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("partition-fenced-batch-lease-wf-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	const worker = "worker-partition-fenced-batch-1"
	claimed, err := store.ClaimWorkflow(ctx, worker)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimWorkflow: %v (nil=%v)", err, claimed == nil)
	}

	// cleat#3245 B2: heartbeat_at is renewed on workflow_leases only. Plant a
	// sentinel on the instance row so a renewal still landing there would
	// show; the claim no longer writes that column at all.
	sentinel := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`UPDATE workflow_instances SET heartbeat_at = $2 WHERE id = $1`, wfID, sentinel); err != nil {
		t.Fatalf("plant the instance sentinel: %v", err)
	}
	var leaseBefore time.Time
	if err := db.QueryRow(`SELECT heartbeat_at FROM workflow_leases WHERE id = $1`, wfID).Scan(&leaseBefore); err != nil {
		t.Fatalf("read workflow_leases.heartbeat_at (before): %v", err)
	}

	// A measurable gap before the renewal, so "renewed" can be distinguished
	// from "happened to read back the same instant" -- not needed for
	// correctness (both statements use the same transaction-local now()), but
	// needed for THIS TEST to prove a renewal occurred at all.
	time.Sleep(20 * time.Millisecond)

	af := &AdaptiveFlusher{db: db, tenantID: DefaultTenantUUID}
	batch := []batchEntry{
		{workflowID: wfID, workerID: worker, generation: claimed.Generation},
	}
	held, lost, err := af.partitionFencedBatch(ctx, batch)
	if err != nil {
		t.Fatalf("partitionFencedBatch: %v", err)
	}
	if len(held) != 1 || held[0].workflowID != wfID {
		t.Fatalf("held = %+v, want exactly the claimed entry (%s)", held, wfID)
	}
	if len(lost) != 0 {
		t.Fatalf("lost = %+v, want none: the claim is still held by %s", lost, worker)
	}

	var instAfter, leaseAfter time.Time
	if err := db.QueryRow(`SELECT heartbeat_at FROM workflow_instances WHERE id = $1`, wfID).Scan(&instAfter); err != nil {
		t.Fatalf("read workflow_instances.heartbeat_at (after): %v", err)
	}
	if err := db.QueryRow(`SELECT heartbeat_at FROM workflow_leases WHERE id = $1`, wfID).Scan(&leaseAfter); err != nil {
		t.Fatalf("read workflow_leases.heartbeat_at (after): %v", err)
	}

	if !leaseAfter.After(leaseBefore) {
		t.Errorf("workflow_leases.heartbeat_at did not advance (before=%v after=%v): the batch "+
			"fence renewed nothing", leaseBefore, leaseAfter)
	}
	if !instAfter.Equal(sentinel) {
		t.Errorf("workflow_instances.heartbeat_at = %v, want the planted %v untouched -- the batch "+
			"fence must lock the instance row, not rewrite it", instAfter, sentinel)
	}
}

// TestPartitionFencedBatchWithALostFenceLeavesTheLeaseRowUntouched is the
// negative control: an entry whose generation has moved on must not renew
// either table's heartbeat_at, on workflow_leases just as on
// workflow_instances.
func TestPartitionFencedBatchWithALostFenceLeavesTheLeaseRowUntouched(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "partition-fenced-batch-lost-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("partition-fenced-batch-lost-wf-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	const worker = "worker-partition-fenced-batch-lost-1"
	claimed, err := store.ClaimWorkflow(ctx, worker)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimWorkflow: %v (nil=%v)", err, claimed == nil)
	}

	var leaseBefore time.Time
	if err := db.QueryRow(`SELECT heartbeat_at FROM workflow_leases WHERE id = $1`, wfID).Scan(&leaseBefore); err != nil {
		t.Fatalf("read workflow_leases.heartbeat_at (before): %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	af := &AdaptiveFlusher{db: db, tenantID: DefaultTenantUUID}
	stale := []batchEntry{
		{workflowID: wfID, workerID: worker, generation: claimed.Generation + 1},
	}
	held, lost, err := af.partitionFencedBatch(ctx, stale)
	if err != nil {
		t.Fatalf("partitionFencedBatch: %v", err)
	}
	if len(held) != 0 || len(lost) != 1 {
		t.Fatalf("held=%+v lost=%+v, want the stale generation reported LOST", held, lost)
	}

	var leaseAfter time.Time
	if err := db.QueryRow(`SELECT heartbeat_at FROM workflow_leases WHERE id = $1`, wfID).Scan(&leaseAfter); err != nil {
		t.Fatalf("read workflow_leases.heartbeat_at (after): %v", err)
	}
	if !leaseAfter.Equal(leaseBefore) {
		t.Errorf("workflow_leases.heartbeat_at advanced (%v -> %v) for a fence that was reported lost",
			leaseBefore, leaseAfter)
	}
}
