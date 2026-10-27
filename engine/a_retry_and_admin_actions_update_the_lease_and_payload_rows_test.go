package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 4c: RetryWorkflow, AdminReReplay, the
// one-phase arm of adminForceResolve (AdminForceComplete/AdminForceFail
// when no defer phase is owed), and the one-phase arm of preemptivelySettle
// (TerminateWorkflow/CancelWorkflow when no defer phase is owed) now mirror
// their terminal transition onto workflow_leases/workflow_payloads, on the
// same tx as the workflow_instances write.
//
// Deliberately NOT covered here, per the scope-revision comment on
// cleat#3245: adminForceMark and preemptivelySettle's two-phase (defer-owed)
// arm, both of which write pending_terminal_status with a real value --
// piece 6's job.
func retryAndAdminLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
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

// seedDeadLetteredTriple creates matching workflow_instances/workflow_leases/
// workflow_payloads rows, all dead_lettered -- standing in for what
// MoveToDeadLetterQueue's dual-write (piece 4a, already merged) produces.
func seedDeadLetteredTriple(t *testing.T, db *sql.DB, id, defName string) {
	t.Helper()
	ctx := context.Background()
	testutil.SeedRun(t, db, testutil.RunSeed{
		ID:         id,
		TenantID:   DefaultTenantUUID,
		DefName:    defName,
		DefVersion: 1,
		Input:      "{}",
		TaskQueue:  "default",
		Status:     "dead_lettered",
	})
	if _, err := db.ExecContext(ctx, `UPDATE workflow_leases SET next_wake_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_leases.next_wake_at: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE workflow_payloads SET error_msg = 'boom', error_code = 'E_BOOM', error_op = 'op1' WHERE id = $1
	`, id); err != nil {
		t.Fatalf("set workflow_payloads error fields: %v", err)
	}
}

// seedClaimableTriple creates matching workflow_instances/workflow_leases/
// workflow_payloads rows, all in an unclaimed, admin-reachable status
// (ready by default, generation 0).
func seedClaimableTriple(t *testing.T, db *sql.DB, id, defName, status string) {
	t.Helper()
	ctx := context.Background()
	testutil.SeedRun(t, db, testutil.RunSeed{
		ID:         id,
		TenantID:   DefaultTenantUUID,
		DefName:    defName,
		DefVersion: 1,
		Input:      "{}",
		TaskQueue:  "default",
		Status:     status,
	})
	if _, err := db.ExecContext(ctx, `UPDATE workflow_leases SET next_wake_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("set workflow_leases.next_wake_at: %v", err)
	}
}

func TestRetryWorkflowUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "retry-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "retry-lease-wf-1"
	seedDeadLetteredTriple(t, db, id, defName)

	if err := store.RetryWorkflow(ctx, id); err != nil {
		t.Fatalf("RetryWorkflow: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "ready" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "ready")
	}

	payload := readPayloadResultFields(t, db, id)
	if payload.errorMsg.Valid {
		t.Errorf("workflow_payloads.error_msg = %v, want NULL", payload.errorMsg)
	}
	if payload.errorCode.Valid {
		t.Errorf("workflow_payloads.error_code = %v, want NULL", payload.errorCode)
	}
	if payload.errorOp.Valid {
		t.Errorf("workflow_payloads.error_op = %v, want NULL", payload.errorOp)
	}
}

func TestRetryWorkflowOnANonDeadLetteredRowLeavesTheLeaseAndPayloadRowsUntouched(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "retry-noop-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "retry-noop-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "ready")

	if err := store.RetryWorkflow(ctx, id); err != nil {
		t.Fatalf("RetryWorkflow: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "ready" {
		t.Errorf("workflow_leases.status changed from a no-op retry: got %q, want unchanged %q", lease.status, "ready")
	}
}

func TestAdminReReplayUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "rereplay-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "rereplay-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "failed")
	// cleat#3245 Phase 3 step 3 piece F: a failed run carries a completed_at
	// from whatever terminal write failed it. Set one directly so the reset
	// assertion below is checking something, not confirming a column that
	// was already NULL stayed NULL.
	if _, err := db.Exec(`UPDATE workflow_payloads SET completed_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("seed workflow_payloads.completed_at: %v", err)
	}

	if err := store.AdminReReplay(ctx, id, 0, "operator-1"); err != nil {
		t.Fatalf("AdminReReplay: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "ready" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "ready")
	}
	var leaseGen int64
	if err := db.QueryRow(`SELECT generation FROM workflow_leases WHERE id = $1`, id).Scan(&leaseGen); err != nil {
		t.Fatalf("read workflow_leases.generation: %v", err)
	}
	if leaseGen != 1 {
		t.Errorf("workflow_leases.generation = %d, want 1 (incremented from 0)", leaseGen)
	}

	var payloadCompletedAt sql.NullTime
	if err := db.QueryRow(`SELECT completed_at FROM workflow_payloads WHERE id = $1`, id).Scan(&payloadCompletedAt); err != nil {
		t.Fatalf("read workflow_payloads.completed_at: %v", err)
	}
	if payloadCompletedAt.Valid {
		t.Errorf("workflow_payloads.completed_at = %v, want NULL -- a re-replayed run is running again, not completed", payloadCompletedAt)
	}
}

func TestAdminReReplayWithWrongGenerationLeavesTheLeaseAndPayloadRowsUntouched(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "rereplay-fence-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "rereplay-fence-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "failed")
	if _, err := db.Exec(`UPDATE workflow_payloads SET completed_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("seed workflow_payloads.completed_at: %v", err)
	}

	if err := store.AdminReReplay(ctx, id, 99, "operator-1"); err == nil {
		t.Fatal("AdminReReplay with a stale generation succeeded, want an error")
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "failed" {
		t.Errorf("workflow_leases.status changed after a fenced-out re-replay: got %q, want unchanged %q", lease.status, "failed")
	}

	var payloadCompletedAt sql.NullTime
	if err := db.QueryRow(`SELECT completed_at FROM workflow_payloads WHERE id = $1`, id).Scan(&payloadCompletedAt); err != nil {
		t.Fatalf("read workflow_payloads.completed_at: %v", err)
	}
	if !payloadCompletedAt.Valid {
		t.Errorf("workflow_payloads.completed_at changed after a fenced-out re-replay: got NULL, want unchanged (set)")
	}
}

func TestAdminForceCompleteUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "force-complete-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "force-complete-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "ready")

	if err := store.AdminForceComplete(ctx, id, 0, `{"ok":true}`, "operator-1"); err != nil {
		t.Fatalf("AdminForceComplete: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "done" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "done")
	}

	payload := readPayloadResultFields(t, db, id)
	if !payload.result.Valid || payload.result.String != `{"ok": true}` {
		t.Errorf("workflow_payloads.result = %v, want %q", payload.result, `{"ok": true}`)
	}
	// cleat#3245 Phase 3 step 3 piece F.
	if !payload.completedAt.Valid {
		t.Errorf("workflow_payloads.completed_at is NULL, want set")
	}
}

func TestAdminForceFailUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "force-fail-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "force-fail-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "ready")

	if err := store.AdminForceFail(ctx, id, 0, "boom", "E_BOOM", "operator-1"); err != nil {
		t.Fatalf("AdminForceFail: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "failed" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "failed")
	}

	payload := readPayloadResultFields(t, db, id)
	if !payload.errorMsg.Valid || payload.errorMsg.String != "boom" {
		t.Errorf("workflow_payloads.error_msg = %v, want %q", payload.errorMsg, "boom")
	}
	if !payload.errorOp.Valid || payload.errorOp.String != "admin_force_fail" {
		t.Errorf("workflow_payloads.error_op = %v, want %q", payload.errorOp, "admin_force_fail")
	}
	// cleat#3245 Phase 3 step 3 piece F.
	if !payload.completedAt.Valid {
		t.Errorf("workflow_payloads.completed_at is NULL, want set")
	}
}

func TestTerminateWorkflowUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "terminate-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "terminate-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "ready")

	if err := store.TerminateWorkflow(ctx, id, "operator said stop"); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "terminated" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "terminated")
	}

	payload := readPayloadResultFields(t, db, id)
	if !payload.errorMsg.Valid || payload.errorMsg.String != "operator said stop" {
		t.Errorf("workflow_payloads.error_msg = %v, want %q", payload.errorMsg, "operator said stop")
	}
	// cleat#3245 Phase 3 step 3 piece F: TerminateWorkflow goes through
	// preemptivelySettle's one-phase arm, which now mirrors completed_at too.
	if !payload.completedAt.Valid {
		t.Errorf("workflow_payloads.completed_at is NULL, want set")
	}
}

func TestCancelWorkflowUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	const defName = "cancel-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "cancel-lease-wf-1"
	seedClaimableTriple(t, db, id, defName, "ready")

	if err := store.CancelWorkflow(ctx, id, "operator cancelled"); err != nil {
		t.Fatalf("CancelWorkflow: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "cancelled" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "cancelled")
	}

	// cleat#3245 Phase 3 step 3 piece F: CancelWorkflow goes through
	// preemptivelySettle's one-phase arm too.
	payload := readPayloadResultFields(t, db, id)
	if !payload.completedAt.Valid {
		t.Errorf("workflow_payloads.completed_at is NULL, want set")
	}
}

func TestTerminateWorkflowOnAMissingRowReturnsNotFoundAndTouchesNoTable(t *testing.T) {
	store, _ := retryAndAdminLeaseTestStore(t)
	ctx := context.Background()

	if err := store.TerminateWorkflow(ctx, "no-such-workflow", "reason"); err != ErrWorkflowNotFound {
		t.Fatalf("TerminateWorkflow on a missing id returned %v, want ErrWorkflowNotFound", err)
	}
}
