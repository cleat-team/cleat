package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 5b: ResolvePromise, RejectPromise and
// CreateUpdateRequest now mirror their promise_seq bump and conditional
// next_wake_at wake onto workflow_leases, on the same tx as the
// workflow_instances write.
//
// Seeds workflow_instances/workflow_leases/workflow_payloads directly via SQL
// -- the same independent-testability pattern pieces 1-5a established --
// rather than depending on any other piece's code.
func promiseLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
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

// seedPromiseTriple creates matching workflow_instances/workflow_leases/
// workflow_payloads rows with the given status, plus a pending
// workflow_promises row -- standing in for what piece 1's dual-write (already
// merged) and CreatePromise would produce by the time a caller resolves,
// rejects or dispatches an update against it. next_wake_at is seeded an hour
// out on both tables, so a test can tell "moved to now" apart from "left
// alone".
func seedPromiseTriple(t *testing.T, store *PostgresStore, db *sql.DB, id, defName, promiseID, status string) {
	t.Helper()
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, next_wake_at)
		VALUES ($1, $2, 1, $3, '{}', 'default', $4, now() + interval '1 hour')
	`, id, defName, status, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_leases (id, tenant_id, task_queue, status, next_wake_at)
		VALUES ($1, $2, 'default', $3, now() + interval '1 hour')
	`, id, DefaultTenantUUID, status); err != nil {
		t.Fatalf("seed workflow_leases: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, input)
		VALUES ($1, $2, '{}')
	`, id, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_payloads: %v", err)
	}
	if promiseID == "" {
		return
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, status, tenant_id)
		VALUES ($1, $2, 'p1', 'pending', $3)
	`, id, promiseID, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_promises: %v", err)
	}
}

type leasePromiseFields struct {
	promiseSeq int64
	nextWakeAt time.Time
}

func readLeasePromiseFields(t *testing.T, db *sql.DB, id string) leasePromiseFields {
	t.Helper()
	var f leasePromiseFields
	if err := db.QueryRow(`
		SELECT promise_seq, next_wake_at FROM workflow_leases WHERE id = $1
	`, id).Scan(&f.promiseSeq, &f.nextWakeAt); err != nil {
		t.Fatalf("read workflow_leases promise fields for %s: %v", id, err)
	}
	return f
}

func assertLeaseMatchesInstancePromiseFields(t *testing.T, db *sql.DB, id string, lease leasePromiseFields) {
	t.Helper()
	var instSeq int64
	var instWake time.Time
	if err := db.QueryRow(`SELECT promise_seq, next_wake_at FROM workflow_instances WHERE id = $1`, id).
		Scan(&instSeq, &instWake); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.promiseSeq != instSeq {
		t.Errorf("workflow_leases.promise_seq (%d) disagrees with workflow_instances (%d)", lease.promiseSeq, instSeq)
	}
	if lease.nextWakeAt.Sub(instWake).Abs() > time.Second {
		t.Errorf("workflow_leases.next_wake_at (%v) disagrees with workflow_instances' (%v)", lease.nextWakeAt, instWake)
	}
}

func TestResolvePromiseUpdatesTheLeaseRow(t *testing.T) {
	store, db := promiseLeaseTestStore(t)
	ctx := context.Background()

	const id = "promise-lease-resolve-1"
	const promiseID = "resolve-promise-1"
	seedPromiseTriple(t, store, db, id, "resolve-lease-def", promiseID, "suspended")

	if err := store.ResolvePromise(ctx, promiseID, `{"ok":true}`); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}

	lease := readLeasePromiseFields(t, db, id)
	if lease.promiseSeq != 1 {
		t.Errorf("workflow_leases.promise_seq = %d, want 1", lease.promiseSeq)
	}
	if time.Since(lease.nextWakeAt) > 10*time.Second {
		t.Errorf("workflow_leases.next_wake_at = %v, want set to roughly now for a suspended workflow", lease.nextWakeAt)
	}
	assertLeaseMatchesInstancePromiseFields(t, db, id, lease)
}

func TestRejectPromiseUpdatesTheLeaseRow(t *testing.T) {
	store, db := promiseLeaseTestStore(t)
	ctx := context.Background()

	const id = "promise-lease-reject-1"
	const promiseID = "reject-promise-1"
	seedPromiseTriple(t, store, db, id, "reject-lease-def", promiseID, "suspended")

	if err := store.RejectPromise(ctx, promiseID, "boom"); err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}

	lease := readLeasePromiseFields(t, db, id)
	if lease.promiseSeq != 1 {
		t.Errorf("workflow_leases.promise_seq = %d, want 1", lease.promiseSeq)
	}
	if time.Since(lease.nextWakeAt) > 10*time.Second {
		t.Errorf("workflow_leases.next_wake_at = %v, want set to roughly now for a suspended workflow", lease.nextWakeAt)
	}
	assertLeaseMatchesInstancePromiseFields(t, db, id, lease)
}

func TestCreateUpdateRequestUpdatesTheLeaseRowAndWakesASuspendedWorkflow(t *testing.T) {
	store, db := promiseLeaseTestStore(t)
	ctx := context.Background()

	const id = "promise-lease-update-1"
	seedPromiseTriple(t, store, db, id, "update-lease-def", "", "suspended")

	if err := store.CreateUpdateRequest(ctx, id, "my-update", `{"x":1}`, "update-promise-1"); err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}

	lease := readLeasePromiseFields(t, db, id)
	if lease.promiseSeq != 1 {
		t.Errorf("workflow_leases.promise_seq = %d, want 1", lease.promiseSeq)
	}
	if time.Since(lease.nextWakeAt) > 10*time.Second {
		t.Errorf("workflow_leases.next_wake_at = %v, want set to roughly now for a suspended workflow", lease.nextWakeAt)
	}
	assertLeaseMatchesInstancePromiseFields(t, db, id, lease)
}

func TestCreateUpdateRequestLeavesNextWakeAtAloneForARunningWorkflow(t *testing.T) {
	store, db := promiseLeaseTestStore(t)
	ctx := context.Background()

	const id = "promise-lease-update-running-1"
	seedPromiseTriple(t, store, db, id, "update-lease-running-def", "", "running")

	if err := store.CreateUpdateRequest(ctx, id, "my-update", `{"x":1}`, "update-promise-running-1"); err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}

	lease := readLeasePromiseFields(t, db, id)
	if lease.promiseSeq != 1 {
		t.Errorf("workflow_leases.promise_seq = %d, want 1 (the bump is unconditional)", lease.promiseSeq)
	}
	if time.Until(lease.nextWakeAt) < 30*time.Minute {
		t.Errorf("workflow_leases.next_wake_at moved for a running workflow: got %v, want left at its seeded ~1h-out value",
			lease.nextWakeAt)
	}
}
