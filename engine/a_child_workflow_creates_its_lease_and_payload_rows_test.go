package engine

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// cleat#3245 Phase 3 step 2 gap fix (found scoping step 3's piece A, before
// any read-cutover code was written): StartChildWorkflow and
// StartChildWorkflowAtomic now create matching workflow_leases and
// workflow_payloads rows, same tx, same insertLeaseAndPayloadRows helper
// piece 1's startNewRun already uses -- neither ever did before this. Every
// child workflow started through either method has had no lease/payload row
// at all since migration 016 (folded into migrations/postgres/001_schema.sql
// by #3358) landed; claimLeaseRows (piece 2) is a plain
// UPDATE, not an upsert, so claiming one silently touched zero rows on the
// lease side with no error to notice by.
//
// Reuses newRunTestStore/readLeaseRow/readInstanceRow/readPayloadInput from
// piece 1's own test file (already merged, same package).
func TestStartChildWorkflowCreatesMatchingLeaseAndPayloadRows(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "child-workflow-lease-payload-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_defs SET task_queue = 'gpu-queue' WHERE name = $1 AND version = 1 AND tenant_id = $2`,
		defName, DefaultTenantUUID); err != nil {
		t.Fatalf("set def task_queue: %v", err)
	}

	parentID, _, err := store.StartNewRun(ctx, "", defName, 1, []byte(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (parent): %v", err)
	}

	childID, err := store.StartChildWorkflow(ctx, parentID, defName, `{"child":true}`, 1, "ABANDON", 5)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}

	instRow := readInstanceRow(t, db, childID)
	leaseRow := readLeaseRow(t, db, childID)

	if leaseRow.tenantID != instRow.tenantID {
		t.Errorf("workflow_leases.tenant_id = %q, workflow_instances.tenant_id = %q, want equal", leaseRow.tenantID, instRow.tenantID)
	}
	if leaseRow.taskQueue != "gpu-queue" {
		t.Errorf("workflow_leases.task_queue = %q, want %q (the resolved value, not the literal default)", leaseRow.taskQueue, "gpu-queue")
	}
	if leaseRow.taskQueue != instRow.taskQueue {
		t.Errorf("workflow_leases.task_queue = %q, workflow_instances.task_queue = %q, want equal", leaseRow.taskQueue, instRow.taskQueue)
	}
	if leaseRow.priority != 5 || leaseRow.priority != instRow.priority {
		t.Errorf("workflow_leases.priority = %d, workflow_instances.priority = %d, want both 5", leaseRow.priority, instRow.priority)
	}
	if leaseRow.status != "ready" || leaseRow.status != instRow.status {
		t.Errorf("workflow_leases.status = %q, workflow_instances.status = %q, want both %q", leaseRow.status, instRow.status, "ready")
	}

	gotInput := readPayloadInput(t, db, childID)
	var wantInput string
	if err := db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, childID).Scan(&wantInput); err != nil {
		t.Fatalf("read workflow_instances.input: %v", err)
	}
	if gotInput != wantInput {
		t.Errorf("workflow_payloads.input = %q, workflow_instances.input = %q, want byte-identical (same sealed value reused, not re-sealed)", gotInput, wantInput)
	}
}

// TestStartChildWorkflowAtomicCreatesMatchingLeaseAndPayloadRows covers the
// SECOND child-creating entry point, which has its own copy of the INSERT
// (and now its own call to insertLeaseAndPayloadRows) -- a fix applied to
// only one of the two would pass the test above while leaving this path
// uncovered, the same reasoning piece 1's own idempotency-key test gives for
// testing startNewRun's two branches separately.
func TestStartChildWorkflowAtomicCreatesMatchingLeaseAndPayloadRows(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	const defName = "child-workflow-atomic-lease-payload-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	parentID, _, err := store.StartNewRun(ctx, "", defName, 1, []byte(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (parent): %v", err)
	}

	childID := uuid.New().String()
	event := EventRecord{Step: 0, EventType: "child_workflow", ChildName: defName, ChildInput: `{"child":true}`}
	got, err := store.StartChildWorkflowAtomic(ctx, childID, parentID, defName, `{"child":true}`, 1, "ABANDON", event, 2)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if got != childID {
		t.Fatalf("StartChildWorkflowAtomic returned %q, want the given childID %q", got, childID)
	}

	instRow := readInstanceRow(t, db, childID)
	leaseRow := readLeaseRow(t, db, childID)

	if leaseRow.tenantID != instRow.tenantID {
		t.Errorf("workflow_leases.tenant_id = %q, workflow_instances.tenant_id = %q, want equal", leaseRow.tenantID, instRow.tenantID)
	}
	if leaseRow.taskQueue != instRow.taskQueue {
		t.Errorf("workflow_leases.task_queue = %q, workflow_instances.task_queue = %q, want equal", leaseRow.taskQueue, instRow.taskQueue)
	}
	if leaseRow.priority != 2 || leaseRow.priority != instRow.priority {
		t.Errorf("workflow_leases.priority = %d, workflow_instances.priority = %d, want both 2", leaseRow.priority, instRow.priority)
	}
	if leaseRow.status != "ready" || leaseRow.status != instRow.status {
		t.Errorf("workflow_leases.status = %q, workflow_instances.status = %q, want both %q", leaseRow.status, instRow.status, "ready")
	}

	gotInput := readPayloadInput(t, db, childID)
	var wantInput string
	if err := db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, childID).Scan(&wantInput); err != nil {
		t.Fatalf("read workflow_instances.input: %v", err)
	}
	if gotInput != wantInput {
		t.Errorf("workflow_payloads.input = %q, workflow_instances.input = %q, want byte-identical", gotInput, wantInput)
	}
}
