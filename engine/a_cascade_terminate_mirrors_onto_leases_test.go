package engine

import (
	"context"
	"testing"
)

// cleat#3245 Phase 3 step 3 piece B: found by TestBothCloseArmsReachTheSameDepth
// failing after the claim-path cutover, not by name -- enforceParentClosePolicyAt's
// three bulk cascade UPDATEs (TERMINATE, TERMINATE's defer-phase arm,
// REQUEST_CANCEL) never got a piece-4c/6c-style workflow_leases mirror when
// those pieces landed. That was invisible as long as nothing read
// workflow_leases for a cascade-closed row. Once ClaimWorkflows' candidate
// scan did, a cascade-terminated child's stale workflow_leases.status='ready'
// row read as claimable again, and the claim's own UPDATE set
// workflow_instances back to 'running' over an already-terminated workflow --
// silent revival of a terminated workflow, the data-loss direction rather
// than a merely-stale read.
//
// This test is the direct, minimal reproduction: one plain-arm TERMINATE
// cascade, then one broad ClaimWorkflows call that must NOT touch the
// cascade-closed child.
func TestCascadeTerminatedChildIsNotRevivedByAClaim(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	deployConcurrencyTestWorkflows(t, store, "wf-cascade-revive-root")

	child, err := store.StartChildWorkflow(ctx, "wf-cascade-revive-root",
		"concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}

	if err := store.TerminateWorkflow(ctx, "wf-cascade-revive-root", "terminated by test"); err != nil {
		t.Fatalf("TerminateWorkflow(root): %v", err)
	}

	instStatus := readInstanceRow(t, db, child).status
	if instStatus != "terminated" {
		t.Fatalf("child workflow_instances.status = %q, want %q (sanity on the existing cascade write, before any claim)", instStatus, "terminated")
	}
	leaseStatus := readLeaseRow(t, db, child).status
	if leaseStatus != "terminated" {
		t.Errorf("child workflow_leases.status = %q, want %q -- the cascade's lease mirror is missing or scoped wrong", leaseStatus, "terminated")
	}

	// The actual failure mode: a claim broad enough to consider this child
	// must not touch it. Before the fix, workflow_leases.status stayed
	// 'ready' (the child's creation-time value), so this call claimed it
	// and set workflow_instances back to 'running'.
	if _, err := store.ClaimWorkflows(ctx, "worker-cascade-revive", 50); err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}

	instAfter := readInstanceRow(t, db, child).status
	if instAfter != "terminated" {
		t.Errorf("child workflow_instances.status AFTER a broad ClaimWorkflows = %q, want still %q -- "+
			"a cascade-terminated workflow was revived by a claim that should never have seen it",
			instAfter, "terminated")
	}
}
