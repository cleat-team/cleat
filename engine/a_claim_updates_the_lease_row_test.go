package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 2: ClaimWorkflow(s), ClaimStickyWorkflows
// and ReleaseWorkflow now mirror their claim/release SET clause onto
// workflow_leases, on the same tx as the workflow_instances write.
//
// This worktree does not carry piece 1 (startNewRun's row creation, a
// sibling PR not yet merged), so these tests seed the workflow_leases row
// directly via SQL rather than through StartNewRun -- exactly the "a pair
// already exists from creation" precondition step 2 relies on, just
// established by the test fixture instead of by piece 1's code. The two
// pieces are independently testable because the dual-write pieces TAKE the
// lease row's existence as a precondition; they do not themselves create
// one, so nothing here depends on piece 1 having landed.
func claimTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
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

// seedInstanceAndLeaseRow creates a workflow_instances row and its matching
// workflow_leases row directly, standing in for what piece 1's
// insertLeaseAndPayloadRows does inside StartNewRun.
func seedInstanceAndLeaseRow(t *testing.T, db *sql.DB, id, defName string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, next_wake_at)
		VALUES ($1, $2, 1, 'ready', '{}', 'default', $3, now() - INTERVAL '1 second')
	`, id, defName, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_leases (id, tenant_id, task_queue)
		VALUES ($1, $2, 'default')
	`, id, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_leases: %v", err)
	}
}

type leaseClaimFields struct {
	status                string
	assignedTo            sql.NullString
	heartbeatAt           sql.NullTime
	startedAt             sql.NullTime
	generation            int64
	signalSeqAtClaim      int64
	signalConsumedAtClaim int64
	promiseSeqAtClaim     int64
	nextWakeAt            time.Time
}

func readLeaseClaimFields(t *testing.T, db *sql.DB, id string) leaseClaimFields {
	t.Helper()
	var f leaseClaimFields
	if err := db.QueryRow(`
		SELECT status, assigned_to, heartbeat_at, started_at, generation,
		       signal_seq_at_claim, signal_consumed_at_claim, promise_seq_at_claim, next_wake_at
		FROM workflow_leases WHERE id = $1
	`, id).Scan(&f.status, &f.assignedTo, &f.heartbeatAt, &f.startedAt, &f.generation,
		&f.signalSeqAtClaim, &f.signalConsumedAtClaim, &f.promiseSeqAtClaim, &f.nextWakeAt); err != nil {
		t.Fatalf("read workflow_leases claim fields for %s: %v", id, err)
	}
	return f
}

func TestClaimWorkflowUpdatesTheLeaseRow(t *testing.T) {
	store, db := claimTestStore(t)
	ctx := context.Background()

	const defName = "claim-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "claim-lease-wf-1"
	seedInstanceAndLeaseRow(t, db, id, defName)

	// Precondition the test itself depends on: the lease row starts
	// 'ready', unassigned, generation 0 -- otherwise a bug that leaves it
	// untouched could coincidentally read back as "already correct".
	pre := readLeaseClaimFields(t, db, id)
	if pre.status != "ready" || pre.assignedTo.Valid || pre.generation != 0 {
		t.Fatalf("UNMEASURED: precondition not met -- status=%q assignedTo=%v generation=%d, want ready/NULL/0",
			pre.status, pre.assignedTo, pre.generation)
	}

	wf, err := store.ClaimWorkflow(ctx, "worker-claim-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil || wf.ID != id {
		t.Fatalf("ClaimWorkflow returned %v, want the seeded workflow %q", wf, id)
	}

	post := readLeaseClaimFields(t, db, id)
	if post.status != "running" {
		t.Errorf("workflow_leases.status = %q, want %q", post.status, "running")
	}
	if !post.assignedTo.Valid || post.assignedTo.String != "worker-claim-1" {
		t.Errorf("workflow_leases.assigned_to = %v, want %q", post.assignedTo, "worker-claim-1")
	}
	if !post.heartbeatAt.Valid {
		t.Error("workflow_leases.heartbeat_at is NULL after claim, want set")
	}
	if !post.startedAt.Valid {
		t.Error("workflow_leases.started_at is NULL after claim, want set")
	}
	if post.generation != 1 {
		t.Errorf("workflow_leases.generation = %d, want 1 (incremented from 0)", post.generation)
	}
	// All three claim-snapshot columns start at 0 on a fresh row, so this
	// does not yet distinguish "copied the live counter" from "left at its
	// own default" -- piece 5 (signal/promise delivery) is what moves the
	// live counters away from 0, and that is where this gets a sharper
	// assertion.
	if post.signalSeqAtClaim != 0 || post.signalConsumedAtClaim != 0 || post.promiseSeqAtClaim != 0 {
		t.Errorf("claim-snapshot columns = %d/%d/%d, want 0/0/0 on a freshly seeded row",
			post.signalSeqAtClaim, post.signalConsumedAtClaim, post.promiseSeqAtClaim)
	}

	// Matches workflow_instances, which is what the running engine still
	// reads today (step 3 moves reads over).
	var instStatus, instAssignedTo string
	var instGeneration int64
	if err := db.QueryRow(`SELECT status, assigned_to, generation FROM workflow_instances WHERE id = $1`, id).
		Scan(&instStatus, &instAssignedTo, &instGeneration); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if post.status != instStatus || post.assignedTo.String != instAssignedTo || post.generation != instGeneration {
		t.Errorf("workflow_leases (%q/%q/%d) disagrees with workflow_instances (%q/%q/%d)",
			post.status, post.assignedTo.String, post.generation, instStatus, instAssignedTo, instGeneration)
	}
}

func TestReleaseWorkflowUpdatesTheLeaseRow(t *testing.T) {
	store, db := claimTestStore(t)
	ctx := context.Background()

	const defName = "release-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "release-lease-wf-1"
	seedInstanceAndLeaseRow(t, db, id, defName)

	wf, err := store.ClaimWorkflow(ctx, "worker-release-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil || wf.ID != id {
		t.Fatalf("ClaimWorkflow returned %v, want the seeded workflow %q", wf, id)
	}

	nextWake := time.Now().Add(5 * time.Minute).Truncate(time.Millisecond)
	if err := store.ReleaseWorkflow(ctx, id, "worker-release-1", wf.Generation, nextWake); err != nil {
		t.Fatalf("ReleaseWorkflow: %v", err)
	}

	post := readLeaseClaimFields(t, db, id)
	if post.status != "ready" {
		t.Errorf("workflow_leases.status after release = %q, want %q (pending_terminal_status is NULL)", post.status, "ready")
	}
	if post.assignedTo.Valid {
		t.Errorf("workflow_leases.assigned_to after release = %v, want NULL", post.assignedTo)
	}
	if !post.nextWakeAt.Equal(nextWake) {
		t.Errorf("workflow_leases.next_wake_at after release = %v, want %v", post.nextWakeAt, nextWake)
	}

	// A release against a generation that no longer matches (the fence)
	// must be refused identically on both tables -- i.e. must not touch
	// workflow_leases either. Calling again with the NOW-STALE generation
	// the first claim held should return ErrFenceLost and leave the lease
	// row exactly as the first release left it.
	if err := store.ReleaseWorkflow(ctx, id, "worker-release-1", wf.Generation, time.Now()); err == nil {
		t.Fatal("second ReleaseWorkflow with a stale generation succeeded, want ErrFenceLost")
	} else if err != ErrFenceLost {
		t.Fatalf("second ReleaseWorkflow error = %v, want ErrFenceLost", err)
	}
	postStale := readLeaseClaimFields(t, db, id)
	if !postStale.nextWakeAt.Equal(nextWake) {
		t.Errorf("workflow_leases.next_wake_at changed after a fence-lost release: got %v, want unchanged %v", postStale.nextWakeAt, nextWake)
	}
}

func TestClaimStickyWorkflowsUpdatesTheLeaseRow(t *testing.T) {
	store, db := claimTestStore(t)
	ctx := context.Background()

	const defName = "claim-sticky-lease-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	const id = "claim-sticky-lease-wf-1"
	seedInstanceAndLeaseRow(t, db, id, defName)
	if _, err := db.ExecContext(ctx, `UPDATE workflow_instances SET sticky_worker_id = $1 WHERE id = $2`, "worker-sticky-1", id); err != nil {
		t.Fatalf("set sticky_worker_id: %v", err)
	}

	wfs, err := store.ClaimStickyWorkflows(ctx, "worker-sticky-1", 10)
	if err != nil {
		t.Fatalf("ClaimStickyWorkflows: %v", err)
	}
	if len(wfs) != 1 || wfs[0].ID != id {
		t.Fatalf("ClaimStickyWorkflows returned %v, want exactly the seeded workflow %q", wfs, id)
	}

	post := readLeaseClaimFields(t, db, id)
	if post.status != "running" {
		t.Errorf("workflow_leases.status = %q, want %q", post.status, "running")
	}
	if !post.assignedTo.Valid || post.assignedTo.String != "worker-sticky-1" {
		t.Errorf("workflow_leases.assigned_to = %v, want %q", post.assignedTo, "worker-sticky-1")
	}
	if post.generation != 1 {
		t.Errorf("workflow_leases.generation = %d, want 1", post.generation)
	}
}
