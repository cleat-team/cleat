package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 6a: UpdateStickyWorker and
// ClearStickyWorker now mirror sticky_worker_id onto workflow_leases, on the
// same tx as the workflow_instances write.
//
// Seeds workflow_instances/workflow_leases/workflow_payloads directly via SQL
// -- the same independent-testability pattern pieces 1-5b established --
// rather than depending on any other piece's code.
func stickyWorkerLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
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

func seedStickyWorkerTriple(t *testing.T, store *PostgresStore, db *sql.DB, id, defName string) {
	t.Helper()
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id)
		VALUES ($1, $2, 1, 'ready', '{}', 'default', $3)
	`, id, defName, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_leases (id, tenant_id, task_queue, status)
		VALUES ($1, $2, 'default', 'ready')
	`, id, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_leases: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, input)
		VALUES ($1, $2, '{}')
	`, id, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_payloads: %v", err)
	}
}

func readLeaseStickyWorker(t *testing.T, db *sql.DB, id string) sql.NullString {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(`SELECT sticky_worker_id FROM workflow_leases WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read workflow_leases.sticky_worker_id for %s: %v", id, err)
	}
	return v
}

func readInstanceStickyWorker(t *testing.T, db *sql.DB, id string) sql.NullString {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(`SELECT sticky_worker_id FROM workflow_instances WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read workflow_instances.sticky_worker_id for %s: %v", id, err)
	}
	return v
}

func TestUpdateStickyWorkerUpdatesTheLeaseRow(t *testing.T) {
	store, db := stickyWorkerLeaseTestStore(t)
	ctx := context.Background()

	const id = "sticky-lease-update-1"
	seedStickyWorkerTriple(t, store, db, id, "sticky-lease-update-def")

	if err := store.UpdateStickyWorker(ctx, id, "worker-sticky-1"); err != nil {
		t.Fatalf("UpdateStickyWorker: %v", err)
	}

	lease := readLeaseStickyWorker(t, db, id)
	if !lease.Valid || lease.String != "worker-sticky-1" {
		t.Errorf("workflow_leases.sticky_worker_id = %v, want %q", lease, "worker-sticky-1")
	}
	inst := readInstanceStickyWorker(t, db, id)
	if lease.Valid != inst.Valid || lease.String != inst.String {
		t.Errorf("workflow_leases.sticky_worker_id (%v) disagrees with workflow_instances (%v)", lease, inst)
	}
}

func TestClearStickyWorkerUpdatesTheLeaseRow(t *testing.T) {
	store, db := stickyWorkerLeaseTestStore(t)
	ctx := context.Background()

	const id = "sticky-lease-clear-1"
	seedStickyWorkerTriple(t, store, db, id, "sticky-lease-clear-def")

	// Seeded directly via SQL on BOTH tables, not through UpdateStickyWorker
	// -- otherwise this test cannot tell ClearStickyWorker's own mirror apart
	// from UpdateStickyWorker's: a reverted ClearStickyWorker mirror would
	// leave workflow_leases.sticky_worker_id at whatever UpdateStickyWorker's
	// (also-reverted-in-that-scenario, but not here) mirror left it, and if
	// that setup call's mirror never ran either, the row would already be
	// NULL before ClearStickyWorker touches it, passing for the wrong
	// reason. Falsified: confirmed this version fails when only
	// ClearStickyWorker's own mirror is removed.
	if _, err := db.ExecContext(ctx, `UPDATE workflow_instances SET sticky_worker_id = $2 WHERE id = $1`, id, "worker-sticky-2"); err != nil {
		t.Fatalf("seed workflow_instances.sticky_worker_id: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workflow_leases SET sticky_worker_id = $2 WHERE id = $1`, id, "worker-sticky-2"); err != nil {
		t.Fatalf("seed workflow_leases.sticky_worker_id: %v", err)
	}

	if err := store.ClearStickyWorker(ctx, id); err != nil {
		t.Fatalf("ClearStickyWorker: %v", err)
	}

	lease := readLeaseStickyWorker(t, db, id)
	if lease.Valid {
		t.Errorf("workflow_leases.sticky_worker_id = %v, want NULL", lease)
	}
	inst := readInstanceStickyWorker(t, db, id)
	if lease.Valid != inst.Valid {
		t.Errorf("workflow_leases.sticky_worker_id (%v) disagrees with workflow_instances (%v)", lease, inst)
	}
}
