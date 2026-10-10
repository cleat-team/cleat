package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 4a: CompleteWorkflow, FailWorkflow and
// MoveToDeadLetterQueue now mirror their terminal status transition onto
// workflow_leases (status/assigned_to/completed_by) and workflow_payloads
// (result/query_state/error_msg/error_code/error_op), on the same tx as the
// workflow_instances write.
//
// Seeds workflow_instances/workflow_leases/workflow_payloads directly via
// SQL as an already-claimed triple -- the same independent-testability
// pattern pieces 1-3 established -- rather than depending on pieces 1/2's
// code (both already merged, but this keeps the test from depending on a
// specific call path to reach the precondition).
func completionLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	applyPostgresProcedures(t, db)
	testutil.CleanupPostgresTestData(t, db)
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	})
	return NewPostgresStore(db), db
}

// seedClaimedTriple creates matching workflow_instances/workflow_leases/
// workflow_payloads rows, all claimed by workerID at the given generation --
// standing in for what pieces 1 and 2's dual-writes (both already merged)
// would produce by the time a real worker calls Complete/Fail/MoveToDeadLetterQueue.
func seedClaimedTriple(t *testing.T, db *sql.DB, id, defName, workerID string, generation int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, assigned_to, generation, next_wake_at, heartbeat_at, started_at)
		VALUES ($1, $2, 1, 'running', '{}', 'default', $3, $4, $5, now(), now(), now())
	`, id, defName, DefaultTenantUUID, workerID, generation); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_leases (id, tenant_id, task_queue, status, assigned_to, generation, heartbeat_at, started_at)
		VALUES ($1, $2, 'default', 'running', $3, $4, now(), now())
	`, id, DefaultTenantUUID, workerID, generation); err != nil {
		t.Fatalf("seed workflow_leases: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, input)
		VALUES ($1, $2, '{}')
	`, id, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_payloads: %v", err)
	}
}

type leaseTerminalFields struct {
	status      string
	assignedTo  sql.NullString
	completedBy sql.NullString
}

func readLeaseTerminalFields(t *testing.T, db *sql.DB, id string) leaseTerminalFields {
	t.Helper()
	var f leaseTerminalFields
	if err := db.QueryRow(`
		SELECT status, assigned_to, completed_by FROM workflow_leases WHERE id = $1
	`, id).Scan(&f.status, &f.assignedTo, &f.completedBy); err != nil {
		t.Fatalf("read workflow_leases terminal fields for %s: %v", id, err)
	}
	return f
}

type payloadResultFields struct {
	result     sql.NullString
	queryState sql.NullString
	errorMsg   sql.NullString
	errorCode  sql.NullString
	errorOp    sql.NullString
}

func readPayloadResultFields(t *testing.T, db *sql.DB, id string) payloadResultFields {
	t.Helper()
	var f payloadResultFields
	if err := db.QueryRow(`
		SELECT result::text, query_state::text, error_msg, error_code, error_op
		FROM workflow_payloads WHERE id = $1
	`, id).Scan(&f.result, &f.queryState, &f.errorMsg, &f.errorCode, &f.errorOp); err != nil {
		t.Fatalf("read workflow_payloads result fields for %s: %v", id, err)
	}
	return f
}

func TestCompleteWorkflowUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := completionLeaseTestStore(t)
	ctx := context.Background()

	const defName = "complete-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "complete-lease-wf-1"
	const worker = "worker-complete-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)

	if err := store.CompleteWorkflow(ctx, id, worker, 1, `{"ok":true}`, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "done" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "done")
	}
	if lease.assignedTo.Valid {
		t.Errorf("workflow_leases.assigned_to = %v, want NULL", lease.assignedTo)
	}
	if !lease.completedBy.Valid || lease.completedBy.String != worker {
		t.Errorf("workflow_leases.completed_by = %v, want %q", lease.completedBy, worker)
	}

	payload := readPayloadResultFields(t, db, id)
	// jsonb's text cast re-serializes with a space after each colon, so the
	// stored shape (not the literal input) is what's asserted on.
	if !payload.result.Valid || payload.result.String != `{"ok": true}` {
		t.Errorf("workflow_payloads.result = %v, want %q", payload.result, `{"ok": true}`)
	}
	if !payload.queryState.Valid || payload.queryState.String != `{"k": "v"}` {
		t.Errorf("workflow_payloads.query_state = %v, want %q", payload.queryState, `{"k": "v"}`)
	}

	// Matches workflow_instances, which is what the running engine still
	// reads today (step 3 moves reads over).
	var instStatus string
	var instAssignedTo sql.NullString
	if err := db.QueryRow(`SELECT status, assigned_to FROM workflow_instances WHERE id = $1`, id).
		Scan(&instStatus, &instAssignedTo); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.status != instStatus || lease.assignedTo.Valid != instAssignedTo.Valid {
		t.Errorf("workflow_leases (%q/%v) disagrees with workflow_instances (%q/%v)",
			lease.status, lease.assignedTo, instStatus, instAssignedTo)
	}
}

func TestCompleteWorkflowWithWrongGenerationLeavesTheLeaseAndPayloadRowsUntouched(t *testing.T) {
	store, db := completionLeaseTestStore(t)
	ctx := context.Background()

	const defName = "complete-lease-fence-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "complete-lease-fence-wf-1"
	const worker = "worker-complete-fence-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)

	if err := store.CompleteWorkflow(ctx, id, worker, 2, `{"ok":true}`, nil); err != ErrFenceLost {
		t.Fatalf("CompleteWorkflow with a stale generation returned %v, want ErrFenceLost", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "running" {
		t.Errorf("workflow_leases.status changed after a fenced-out complete: got %q, want unchanged %q", lease.status, "running")
	}
	payload := readPayloadResultFields(t, db, id)
	if payload.result.Valid {
		t.Errorf("workflow_payloads.result changed after a fenced-out complete: got %v, want still NULL", payload.result)
	}
}

func TestFailWorkflowUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := completionLeaseTestStore(t)
	ctx := context.Background()

	const defName = "fail-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "fail-lease-wf-1"
	const worker = "worker-fail-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)

	if err := store.FailWorkflow(ctx, id, worker, 1, "boom", "E_BOOM", "op1", nil); err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "failed" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "failed")
	}
	if !lease.completedBy.Valid || lease.completedBy.String != worker {
		t.Errorf("workflow_leases.completed_by = %v, want %q", lease.completedBy, worker)
	}

	payload := readPayloadResultFields(t, db, id)
	if !payload.errorMsg.Valid || payload.errorMsg.String != "boom" {
		t.Errorf("workflow_payloads.error_msg = %v, want %q", payload.errorMsg, "boom")
	}
	if !payload.errorCode.Valid || payload.errorCode.String != "E_BOOM" {
		t.Errorf("workflow_payloads.error_code = %v, want %q", payload.errorCode, "E_BOOM")
	}
	if !payload.errorOp.Valid || payload.errorOp.String != "op1" {
		t.Errorf("workflow_payloads.error_op = %v, want %q", payload.errorOp, "op1")
	}
}

func TestMoveToDeadLetterQueueUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := completionLeaseTestStore(t)
	ctx := context.Background()

	const defName = "dlq-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "dlq-lease-wf-1"
	const worker = "worker-dlq-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)

	if err := store.MoveToDeadLetterQueue(ctx, id, worker, 1, "dead", "E_DEAD", "op2", nil); err != nil {
		t.Fatalf("MoveToDeadLetterQueue: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "dead_lettered" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "dead_lettered")
	}

	payload := readPayloadResultFields(t, db, id)
	if !payload.errorMsg.Valid || payload.errorMsg.String != "dead" {
		t.Errorf("workflow_payloads.error_msg = %v, want %q", payload.errorMsg, "dead")
	}
}
