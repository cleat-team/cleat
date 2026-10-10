package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#3245 Phase 3 step 2, piece 6c: adminForceMark (the defer-owed arm of
// AdminForceComplete/AdminForceFail) and preemptivelySettle's two-phase arm
// (the defer-owed arm of TerminateWorkflow/CancelWorkflow) now mirror the
// mark-defer-phase transition onto workflow_leases/workflow_payloads, on the
// same tx as the workflow_instances write -- the piece-4c-deferred half of
// each pair. This is also what makes ReleaseWorkflow's own mirror (piece 2)
// correct for the first time: see its upgraded CASE expression.
//
// Reuses retryAndAdminLeaseTestStore/seedClaimableTriple/
// readLeaseTerminalFields/readPayloadResultFields from piece 4c's test file
// (already-merged, same package) and seedClaimedTriple from piece 4a's.
//
// AppendEventHistoryBatch is unrelated plumbing (predates this migration
// entirely, like DeployWorkflowDef) rather than a sibling dual-write piece,
// so using it to register the defer event these tests need does not weaken
// independent-testability the way depending on another piece's INSERT code
// would.

func TestAdminForceCompleteWithADeferOwedMarksTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "force-complete-defer-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "force-complete-defer-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "ready")
	if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
		{Step: 0, EventType: EventTypeDefer, Service: "cleanup", Op: "release"},
	}); err != nil {
		t.Fatalf("register defer: %v", err)
	}

	if err := store.AdminForceComplete(ctx, id, 0, `{"ok":true}`, "operator-1"); err != nil {
		t.Fatalf("AdminForceComplete: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != statusTerminating {
		t.Errorf("workflow_leases.status = %q, want %q (a defer phase is owed)", lease.status, statusTerminating)
	}

	var leasePending string
	if err := db.QueryRow(`SELECT pending_terminal_status FROM workflow_leases WHERE id = $1`, id).Scan(&leasePending); err != nil {
		t.Fatalf("read workflow_leases.pending_terminal_status: %v", err)
	}
	if leasePending != "done" {
		t.Errorf("workflow_leases.pending_terminal_status = %q, want %q", leasePending, "done")
	}

	payload := readPayloadResultFields(t, db, id)
	if !payload.result.Valid || payload.result.String != `{"ok": true}` {
		t.Errorf("workflow_payloads.result = %v, want %q", payload.result, `{"ok": true}`)
	}

	var instStatus, instPending string
	if err := db.QueryRow(`SELECT status, pending_terminal_status FROM workflow_instances WHERE id = $1`, id).
		Scan(&instStatus, &instPending); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.status != instStatus || leasePending != instPending {
		t.Errorf("workflow_leases (%q/%q) disagrees with workflow_instances (%q/%q)",
			lease.status, leasePending, instStatus, instPending)
	}
}

func TestAdminForceFailWithADeferOwedMarksTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "force-fail-defer-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "force-fail-defer-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "ready")
	if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
		{Step: 0, EventType: EventTypeDefer, Service: "cleanup", Op: "release"},
	}); err != nil {
		t.Fatalf("register defer: %v", err)
	}

	if err := store.AdminForceFail(ctx, id, 0, "boom", "E_BOOM", "operator-1"); err != nil {
		t.Fatalf("AdminForceFail: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != statusTerminating {
		t.Errorf("workflow_leases.status = %q, want %q (a defer phase is owed)", lease.status, statusTerminating)
	}

	var leasePending string
	if err := db.QueryRow(`SELECT pending_terminal_status FROM workflow_leases WHERE id = $1`, id).Scan(&leasePending); err != nil {
		t.Fatalf("read workflow_leases.pending_terminal_status: %v", err)
	}
	if leasePending != "failed" {
		t.Errorf("workflow_leases.pending_terminal_status = %q, want %q", leasePending, "failed")
	}

	// adminForceMark leaves error_msg/error_code/error_op NULL on
	// workflow_payloads until FinalizeDeferPhase applies the recorded
	// outcome -- unlike the one-phase arm (piece 4c), which writes them
	// directly. Nothing to assert on the payload row yet; the lease row's
	// pending_terminal_status is what carries the operator's intent.
}

func TestTerminateWorkflowWithADeferOwedMarksTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "terminate-defer-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "terminate-defer-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "ready")
	if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
		{Step: 0, EventType: EventTypeDefer, Service: "cleanup", Op: "release"},
	}); err != nil {
		t.Fatalf("register defer: %v", err)
	}

	if err := store.TerminateWorkflow(ctx, id, "operator requested stop"); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != statusTerminating {
		t.Errorf("workflow_leases.status = %q, want %q (a defer phase is owed)", lease.status, statusTerminating)
	}

	var leasePending string
	if err := db.QueryRow(`SELECT pending_terminal_status FROM workflow_leases WHERE id = $1`, id).Scan(&leasePending); err != nil {
		t.Fatalf("read workflow_leases.pending_terminal_status: %v", err)
	}
	if leasePending != statusTerminated {
		t.Errorf("workflow_leases.pending_terminal_status = %q, want %q", leasePending, statusTerminated)
	}

	payload := readPayloadResultFields(t, db, id)
	if !payload.errorMsg.Valid || payload.errorMsg.String != "operator requested stop" {
		t.Errorf("workflow_payloads.error_msg = %v, want %q", payload.errorMsg, "operator requested stop")
	}

	var instStatus, instPending string
	if err := db.QueryRow(`SELECT status, pending_terminal_status FROM workflow_instances WHERE id = $1`, id).
		Scan(&instStatus, &instPending); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.status != instStatus || leasePending != instPending {
		t.Errorf("workflow_leases (%q/%q) disagrees with workflow_instances (%q/%q)",
			lease.status, leasePending, instStatus, instPending)
	}
}

// TestReleaseWorkflowWithAPendingTerminalStatusGoesToTerminatingNotReady is
// the regression test for ReleaseWorkflow's upgraded mirror: before piece
// 6c, this exact scenario (a claimed workflow with pending_terminal_status
// already set -- what adminForceMark/preemptivelySettle's two-phase arm,
// now mirrored above, produce) released to workflow_leases.status='ready'
// while workflow_instances went to 'terminating'. Seeds the precondition
// directly via SQL on both tables, as if piece 6c's own mark side had
// already run, rather than depending on it.
func TestReleaseWorkflowWithAPendingTerminalStatusGoesToTerminatingNotReady(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "release-pending-terminal-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "release-pending-terminal-wf-1"
	const worker = "worker-release-pending-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)
	if _, err := db.ExecContext(ctx, `UPDATE workflow_instances SET pending_terminal_status = 'done' WHERE id = $1`, id); err != nil {
		t.Fatalf("seed workflow_instances.pending_terminal_status: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workflow_leases SET pending_terminal_status = 'done' WHERE id = $1`, id); err != nil {
		t.Fatalf("seed workflow_leases.pending_terminal_status: %v", err)
	}

	nextWake := time.Now().Add(time.Hour)
	if err := store.ReleaseWorkflow(ctx, id, worker, 1, nextWake); err != nil {
		t.Fatalf("ReleaseWorkflow: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != statusTerminating {
		t.Errorf("workflow_leases.status = %q, want %q (pending_terminal_status is set)", lease.status, statusTerminating)
	}

	var instStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, id).Scan(&instStatus); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.status != instStatus {
		t.Errorf("workflow_leases.status (%q) disagrees with workflow_instances (%q)", lease.status, instStatus)
	}
}
