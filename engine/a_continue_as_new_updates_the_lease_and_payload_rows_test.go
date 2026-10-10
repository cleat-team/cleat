package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 4d: ContinueAsNew now mirrors BOTH halves
// of its work onto workflow_leases/workflow_payloads, on the same tx as the
// workflow_instances writes -- the new run's row creation (piece 1's
// insertLeaseAndPayloadRows) and the old run's completion (piece 4a's
// completeLeaseRow/writeResultPayload). This is the last piece in group 4
// per the original sequencing plan, landing last because it depends on
// both helpers already existing.
func continueAsNewLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
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

func TestContinueAsNewUpdatesBothRunsLeaseAndPayloadRows(t *testing.T) {
	store, db := continueAsNewLeaseTestStore(t)
	ctx := context.Background()

	const defName = "continue-as-new-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const oldID = "canew-lease-old-wf-1"
	const worker = "worker-canew-1"
	seedClaimedTriple(t, db, oldID, defName, worker, 1)

	newRunID, err := store.ContinueAsNew(ctx, oldID, worker, 1, defName, 1,
		[]byte(`{"next":true}`), nil, `{"ok":true}`, map[string]string{"k": "v"}, 7)
	if err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}
	if newRunID == "" {
		t.Fatal("ContinueAsNew returned an empty new run id")
	}

	// The OLD run: completed, same shape as CompleteWorkflow (piece 4a).
	oldLease := readLeaseTerminalFields(t, db, oldID)
	if oldLease.status != "done" {
		t.Errorf("old run workflow_leases.status = %q, want %q", oldLease.status, "done")
	}
	if oldLease.assignedTo.Valid {
		t.Errorf("old run workflow_leases.assigned_to = %v, want NULL", oldLease.assignedTo)
	}
	if !oldLease.completedBy.Valid || oldLease.completedBy.String != worker {
		t.Errorf("old run workflow_leases.completed_by = %v, want %q", oldLease.completedBy, worker)
	}
	oldPayload := readPayloadResultFields(t, db, oldID)
	if !oldPayload.result.Valid || oldPayload.result.String != `{"ok": true}` {
		t.Errorf("old run workflow_payloads.result = %v, want %q", oldPayload.result, `{"ok": true}`)
	}
	if !oldPayload.queryState.Valid || oldPayload.queryState.String != `{"k": "v"}` {
		t.Errorf("old run workflow_payloads.query_state = %v, want %q", oldPayload.queryState, `{"k": "v"}`)
	}

	// The NEW run: a lease/payload pair must exist (piece 1's shape),
	// 'ready', unclaimed, carrying the new input.
	var newLeaseExists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_leases WHERE id = $1)`, newRunID).Scan(&newLeaseExists); err != nil {
		t.Fatalf("check new run workflow_leases exists: %v", err)
	}
	if !newLeaseExists {
		t.Fatal("new run has no workflow_leases row")
	}
	var newLeaseStatus, newLeaseTaskQueue string
	var newLeasePriority int
	if err := db.QueryRow(`SELECT status, task_queue, priority FROM workflow_leases WHERE id = $1`, newRunID).
		Scan(&newLeaseStatus, &newLeaseTaskQueue, &newLeasePriority); err != nil {
		t.Fatalf("read new run workflow_leases: %v", err)
	}
	if newLeaseStatus != "ready" {
		t.Errorf("new run workflow_leases.status = %q, want %q", newLeaseStatus, "ready")
	}
	if newLeaseTaskQueue != "default" {
		t.Errorf("new run workflow_leases.task_queue = %q, want %q", newLeaseTaskQueue, "default")
	}
	if newLeasePriority != 7 {
		t.Errorf("new run workflow_leases.priority = %d, want 7", newLeasePriority)
	}

	var newPayloadInput string
	if err := db.QueryRow(`SELECT input::text FROM workflow_payloads WHERE id = $1`, newRunID).Scan(&newPayloadInput); err != nil {
		t.Fatalf("read new run workflow_payloads.input: %v", err)
	}
	if newPayloadInput != `{"next": true}` {
		t.Errorf("new run workflow_payloads.input = %q, want %q", newPayloadInput, `{"next": true}`)
	}

	// Matches workflow_instances for both runs, which is what the running
	// engine still reads today (step 3 moves reads over).
	var oldInstStatus, newInstStatus, newInstTaskQueue string
	var newInstPriority int
	if err := db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, oldID).Scan(&oldInstStatus); err != nil {
		t.Fatalf("read old run workflow_instances: %v", err)
	}
	if err := db.QueryRow(`SELECT status, task_queue, priority FROM workflow_instances WHERE id = $1`, newRunID).
		Scan(&newInstStatus, &newInstTaskQueue, &newInstPriority); err != nil {
		t.Fatalf("read new run workflow_instances: %v", err)
	}
	if oldLease.status != oldInstStatus {
		t.Errorf("old run: workflow_leases.status (%q) disagrees with workflow_instances.status (%q)", oldLease.status, oldInstStatus)
	}
	if newLeaseStatus != newInstStatus || newLeaseTaskQueue != newInstTaskQueue || newLeasePriority != newInstPriority {
		t.Errorf("new run: workflow_leases (%q/%q/%d) disagrees with workflow_instances (%q/%q/%d)",
			newLeaseStatus, newLeaseTaskQueue, newLeasePriority, newInstStatus, newInstTaskQueue, newInstPriority)
	}
}

func TestContinueAsNewWithWrongGenerationCreatesNoNewRunAndLeavesTheOldRowsUntouched(t *testing.T) {
	store, db := continueAsNewLeaseTestStore(t)
	ctx := context.Background()

	const defName = "continue-as-new-fence-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const oldID = "canew-fence-lease-old-wf-1"
	const worker = "worker-canew-fence-1"
	seedClaimedTriple(t, db, oldID, defName, worker, 1)

	var leaseCountBefore int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_leases`).Scan(&leaseCountBefore); err != nil {
		t.Fatalf("count workflow_leases before: %v", err)
	}

	if _, err := store.ContinueAsNew(ctx, oldID, worker, 2, defName, 1,
		[]byte(`{"next":true}`), nil, `{"ok":true}`, nil, 0); err != ErrFenceLost {
		t.Fatalf("ContinueAsNew with a stale generation returned %v, want ErrFenceLost", err)
	}

	// The whole transaction must have rolled back: no orphaned new-run
	// lease/payload row, and the old run's row untouched.
	var leaseCountAfter int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_leases`).Scan(&leaseCountAfter); err != nil {
		t.Fatalf("count workflow_leases after: %v", err)
	}
	if leaseCountAfter != leaseCountBefore {
		t.Errorf("workflow_leases row count changed from a fenced-out ContinueAsNew: before=%d after=%d (an orphaned new-run row was left behind)", leaseCountBefore, leaseCountAfter)
	}

	oldLease := readLeaseTerminalFields(t, db, oldID)
	if oldLease.status != "running" {
		t.Errorf("old run workflow_leases.status changed after a fenced-out ContinueAsNew: got %q, want unchanged %q", oldLease.status, "running")
	}
}
