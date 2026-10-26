package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 4b: finalize_workflow_status (the
// stored procedure behind FinalizeWorkflowSegment) now mirrors its 'done'
// and 'ready' terminal writes onto workflow_leases/workflow_payloads, on
// the same transaction as the workflow_instances write -- reached via
// migration 017 rather than a Go change, since the write is delegated into
// one PL/pgSQL function call.
//
// Seeds workflow_instances/workflow_leases/workflow_payloads directly via
// SQL as an already-claimed triple, same independent-testability pattern
// as pieces 1-4a.
func finalizeLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
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

func TestFinalizeWorkflowSegmentDoneUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := finalizeLeaseTestStore(t)
	ctx := context.Background()

	const defName = "finalize-done-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "finalize-done-lease-wf-1"
	const worker = "worker-finalize-done-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)

	if err := store.FinalizeWorkflowSegment(ctx, id, worker, 1, nil, "done", `{"ok":true}`, "", "", map[string]string{"k": "v"}, time.Time{}); err != nil {
		t.Fatalf("FinalizeWorkflowSegment: %v", err)
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
	if !payload.result.Valid || payload.result.String != `{"ok": true}` {
		t.Errorf("workflow_payloads.result = %v, want %q", payload.result, `{"ok": true}`)
	}
	if !payload.queryState.Valid || payload.queryState.String != `{"k": "v"}` {
		t.Errorf("workflow_payloads.query_state = %v, want %q", payload.queryState, `{"k": "v"}`)
	}
}

func TestFinalizeWorkflowSegmentReadyUpdatesTheLeaseAndPayloadRows(t *testing.T) {
	store, db := finalizeLeaseTestStore(t)
	ctx := context.Background()

	const defName = "finalize-ready-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "finalize-ready-lease-wf-1"
	const worker = "worker-finalize-ready-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)

	// signal_seq/signal_seq_at_claim etc all default to 0 on both tables
	// (the seed sets neither), so the CASE in finalize_workflow_status's
	// 'ready' arm takes the ELSE branch -- next_wake_at should resolve to
	// exactly the nextWakeAt argument, not now().
	nextWake := time.Now().Add(5 * time.Minute).Truncate(time.Microsecond)
	if err := store.FinalizeWorkflowSegment(ctx, id, worker, 1, nil, "ready", "", "", "", nil, nextWake); err != nil {
		t.Fatalf("FinalizeWorkflowSegment: %v", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "ready" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "ready")
	}
	if lease.assignedTo.Valid {
		t.Errorf("workflow_leases.assigned_to = %v, want NULL", lease.assignedTo)
	}

	var leaseNextWake time.Time
	if err := db.QueryRow(`SELECT next_wake_at FROM workflow_leases WHERE id = $1`, id).Scan(&leaseNextWake); err != nil {
		t.Fatalf("read workflow_leases.next_wake_at: %v", err)
	}
	if !leaseNextWake.Equal(nextWake) {
		t.Errorf("workflow_leases.next_wake_at = %v, want %v", leaseNextWake, nextWake)
	}
}

func TestFinalizeWorkflowSegmentReadyWithALiveSignalWakesNow(t *testing.T) {
	store, db := finalizeLeaseTestStore(t)
	ctx := context.Background()

	const defName = "finalize-ready-live-signal-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "finalize-ready-live-signal-wf-1"
	const worker = "worker-finalize-ready-live-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)

	// A settlement that arrived mid-segment: signal_seq has moved past the
	// claim-time snapshot. The resolved next_wake_at should come back as
	// "now" rather than the far-future argument. Since cleat#3245 B3c
	// (migration 007) finalize computes that from workflow_leases' own
	// counters, the only copy DeliverSignal bumps.
	if _, err := db.ExecContext(ctx, `UPDATE workflow_leases SET signal_seq = 1 WHERE id = $1`, id); err != nil {
		t.Fatalf("bump signal_seq: %v", err)
	}

	farFuture := time.Now().Add(1 * time.Hour)
	if err := store.FinalizeWorkflowSegment(ctx, id, worker, 1, nil, "ready", "", "", "", nil, farFuture); err != nil {
		t.Fatalf("FinalizeWorkflowSegment: %v", err)
	}

	var leaseNextWake time.Time
	if err := db.QueryRow(`SELECT next_wake_at FROM workflow_leases WHERE id = $1`, id).Scan(&leaseNextWake); err != nil {
		t.Fatalf("read workflow_leases.next_wake_at: %v", err)
	}
	if leaseNextWake.Equal(farFuture) {
		t.Fatalf("workflow_leases.next_wake_at = the far-future argument, want approximately now (a live signal should wake it immediately)")
	}
}

func TestFinalizeWorkflowSegmentWithWrongGenerationLeavesTheLeaseAndPayloadRowsUntouched(t *testing.T) {
	store, db := finalizeLeaseTestStore(t)
	ctx := context.Background()

	const defName = "finalize-fence-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "finalize-fence-lease-wf-1"
	const worker = "worker-finalize-fence-1"
	seedClaimedTriple(t, db, id, defName, worker, 1)

	if err := store.FinalizeWorkflowSegment(ctx, id, worker, 2, nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != ErrFenceLost {
		t.Fatalf("FinalizeWorkflowSegment with a stale generation returned %v, want ErrFenceLost", err)
	}

	lease := readLeaseTerminalFields(t, db, id)
	if lease.status != "running" {
		t.Errorf("workflow_leases.status changed after a fenced-out finalize: got %q, want unchanged %q", lease.status, "running")
	}
	payload := readPayloadResultFields(t, db, id)
	if payload.result.Valid {
		t.Errorf("workflow_payloads.result changed after a fenced-out finalize: got %v, want still NULL", payload.result)
	}
}
