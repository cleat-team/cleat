package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// cleat#3245 Phase 3 step 2, piece 6d: FinalizeDeferPhase and ExpireDeferPhases
// (engine/store_defer_phase.go) now mirror the apply-defer-phase transition
// onto workflow_leases, on the same tx as the workflow_instances write. This
// is the apply side of the mark/apply pair pieces 4c and 6c mirrored on the
// mark side.
//
// Neither call writes workflow_payloads. result/error_msg/error_code/
// error_op are written at MARK time (piece 4c's one-phase arm, piece 6c's
// defer-owed arm) -- FinalizeDeferPhase and ExpireDeferPhases only ever flip
// status from pending_terminal_status to status, so there is nothing new to
// write on the payload side.
//
// Seeds workflow_instances/workflow_leases/workflow_payloads directly via SQL
// in the already-marked, defer-owed state (as if piece 6c's mark side had
// already run) rather than calling AdminForceComplete/TerminateWorkflow to
// reach it -- the same independent-testability pattern pieces 1-6c
// established, and the same reason piece 6c's own ReleaseWorkflow regression
// test seeded its precondition directly rather than depending on piece 6c's
// own mark-side code.

// seedDeferOwedTriple creates matching workflow_instances/workflow_leases/
// workflow_payloads rows in the "marked for a deferred terminal outcome"
// state: status='terminating', pending_terminal_status=outcome, claimed by
// workerID at generation, with its defer_phase_deadline deadlineOffset from
// now (negative = already past, for ExpireDeferPhases; positive = not yet
// due, for FinalizeDeferPhase and as ExpireDeferPhases' negative control).
func seedDeferOwedTriple(t *testing.T, db *sql.DB, id, defName, workerID, outcome string, generation int64, deadlineOffset time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (
			id, def_name, def_version, status, pending_terminal_status, input, task_queue,
			tenant_id, assigned_to, generation, defer_phase_deadline, next_wake_at, heartbeat_at, started_at
		)
		VALUES ($1, $2, 1, $3, $4, '{}', 'default', $5, $6, $7, now() + ($8 * interval '1 second'), now(), now(), now())
	`, id, defName, statusTerminating, outcome, DefaultTenantUUID, workerID, generation, deadlineOffset.Seconds()); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_leases (
			id, tenant_id, task_queue, status, pending_terminal_status, assigned_to, generation,
			defer_phase_deadline, heartbeat_at, started_at
		)
		VALUES ($1, $2, 'default', $3, $4, $5, $6, now() + ($7 * interval '1 second'), now(), now())
	`, id, DefaultTenantUUID, statusTerminating, outcome, workerID, generation, deadlineOffset.Seconds()); err != nil {
		t.Fatalf("seed workflow_leases: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, input)
		VALUES ($1, $2, '{}')
	`, id, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_payloads: %v", err)
	}
}

func TestFinalizeDeferPhaseUpdatesTheLeaseRow(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "finalize-defer-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "finalize-defer-lease-wf-1"
	const worker = "worker-finalize-defer-1"
	seedDeferOwedTriple(t, db, id, defName, worker, "done", 1, time.Hour)

	if err := store.FinalizeDeferPhase(ctx, id, worker, 1, nil); err != nil {
		t.Fatalf("FinalizeDeferPhase: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "done" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "done")
	}
	if lease.assignedTo.Valid {
		t.Errorf("workflow_leases.assigned_to = %v, want NULL", lease.assignedTo)
	}

	var leasePending sql.NullString
	var leaseDeadline sql.NullTime
	if err := db.QueryRow(`
		SELECT pending_terminal_status, defer_phase_deadline FROM workflow_leases WHERE id = $1
	`, id).Scan(&leasePending, &leaseDeadline); err != nil {
		t.Fatalf("read workflow_leases pending/deadline: %v", err)
	}
	if leasePending.Valid {
		t.Errorf("workflow_leases.pending_terminal_status = %v, want NULL", leasePending)
	}
	if leaseDeadline.Valid {
		t.Errorf("workflow_leases.defer_phase_deadline = %v, want NULL", leaseDeadline)
	}

	var instStatus string
	var instPending sql.NullString
	if err := db.QueryRow(`
		SELECT status, pending_terminal_status FROM workflow_instances WHERE id = $1
	`, id).Scan(&instStatus, &instPending); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.status != instStatus || leasePending.Valid != instPending.Valid {
		t.Errorf("workflow_leases (%q/%v) disagrees with workflow_instances (%q/%v)",
			lease.status, leasePending, instStatus, instPending)
	}
}

func TestExpireDeferPhasesUpdatesTheLeaseRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "expire-defer-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const idA = "expire-defer-lease-wf-a"
	const idB = "expire-defer-lease-wf-b"
	seedDeferOwedTriple(t, db, idA, defName, "worker-expire-a", "done", 1, -time.Hour)
	seedDeferOwedTriple(t, db, idB, defName, "worker-expire-b", "failed", 1, -time.Minute)

	n, err := store.ExpireDeferPhases(ctx)
	if err != nil {
		t.Fatalf("ExpireDeferPhases: %v", err)
	}
	if n != 2 {
		t.Fatalf("ExpireDeferPhases returned %d, want 2", n)
	}

	for id, wantStatus := range map[string]string{idA: "done", idB: "failed"} {
		lease := readLeaseTerminalFields(t, db, id)
		if lease.status != wantStatus {
			t.Errorf("%s: workflow_leases.status = %q, want %q", id, lease.status, wantStatus)
		}
		if lease.assignedTo.Valid {
			t.Errorf("%s: workflow_leases.assigned_to = %v, want NULL", id, lease.assignedTo)
		}

		var leaseGen int64
		var leasePending sql.NullString
		if err := db.QueryRow(`
			SELECT generation, pending_terminal_status FROM workflow_leases WHERE id = $1
		`, id).Scan(&leaseGen, &leasePending); err != nil {
			t.Fatalf("%s: read workflow_leases generation/pending: %v", id, err)
		}
		if leaseGen != 2 {
			t.Errorf("%s: workflow_leases.generation = %d, want 2 (bumped from the seeded 1)", id, leaseGen)
		}
		if leasePending.Valid {
			t.Errorf("%s: workflow_leases.pending_terminal_status = %v, want NULL", id, leasePending)
		}

		var instStatus string
		if err := db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, id).Scan(&instStatus); err != nil {
			t.Fatalf("%s: read workflow_instances: %v", id, err)
		}
		if lease.status != instStatus {
			t.Errorf("%s: workflow_leases.status (%q) disagrees with workflow_instances (%q)", id, lease.status, instStatus)
		}
	}
}

// TestExpireDeferPhasesLeavesAFutureDeadlineUntouched is the negative control
// for the test above: a defer phase that has not yet run out must not be
// swept, on either table.
func TestExpireDeferPhasesLeavesAFutureDeadlineUntouched(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "expire-defer-lease-future-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "expire-defer-lease-future-wf-1"
	seedDeferOwedTriple(t, db, id, defName, "worker-expire-future", "done", 1, time.Hour)

	n, err := store.ExpireDeferPhases(ctx)
	if err != nil {
		t.Fatalf("ExpireDeferPhases: %v", err)
	}
	if n != 0 {
		t.Fatalf("ExpireDeferPhases returned %d, want 0 (nothing is past its deadline)", n)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != statusTerminating {
		t.Errorf("workflow_leases.status = %q, want %q (untouched)", lease.status, statusTerminating)
	}
	if !lease.assignedTo.Valid || lease.assignedTo.String != "worker-expire-future" {
		t.Errorf("workflow_leases.assigned_to = %v, want %q (untouched)", lease.assignedTo, "worker-expire-future")
	}

	var leaseGen int64
	if err := db.QueryRow(`SELECT generation FROM workflow_leases WHERE id = $1`, id).Scan(&leaseGen); err != nil {
		t.Fatalf("read workflow_leases.generation: %v", err)
	}
	if leaseGen != 1 {
		t.Errorf("workflow_leases.generation = %d, want 1 (untouched)", leaseGen)
	}
}
