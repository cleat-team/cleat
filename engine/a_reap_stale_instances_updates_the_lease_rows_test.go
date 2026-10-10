package engine

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 6b: ReapStaleInstances and
// ReapStaleInstancesExcept now mirror their bulk reclaim onto
// workflow_leases, on the same tx -- the exact set of ids the
// workflow_instances UPDATE just reclaimed, captured via RETURNING rather
// than re-derived against workflow_leases' own columns.
//
// Seeds workflow_instances/workflow_leases/workflow_payloads directly via
// SQL -- the same independent-testability pattern pieces 1-6a established --
// rather than depending on any other piece's code.
func reapLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
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

// seedStaleRunningTriple creates matching workflow_instances/workflow_leases/
// workflow_payloads rows, all 'running' with a heartbeat staleAge in the
// past -- standing in for what piece 1's dual-write (already merged) and
// piece 2's claim path would produce by the time a run goes stale. If
// pendingTerminal is non-empty, pending_terminal_status is seeded on both
// tables too, to exercise the CASE branch.
func seedStaleRunningTriple(t *testing.T, store *PostgresStore, db *sql.DB, id, defName, workerID string, staleAge time.Duration, pendingTerminal string) {
	t.Helper()
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	var pendingArg any
	if pendingTerminal != "" {
		pendingArg = pendingTerminal
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, assigned_to, generation, heartbeat_at, started_at, pending_terminal_status)
		VALUES ($1, $2, 1, 'running', '{}', 'default', $3, $4, 1, now() - $5::interval, now(), $6)
	`, id, defName, DefaultTenantUUID, workerID, fmt.Sprintf("%d milliseconds", staleAge.Milliseconds()), pendingArg); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_leases (id, tenant_id, task_queue, status, assigned_to, generation, heartbeat_at, started_at, pending_terminal_status)
		VALUES ($1, $2, 'default', 'running', $3, 1, now() - $4::interval, now(), $5)
	`, id, DefaultTenantUUID, workerID, fmt.Sprintf("%d milliseconds", staleAge.Milliseconds()), pendingArg); err != nil {
		t.Fatalf("seed workflow_leases: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, input)
		VALUES ($1, $2, '{}')
	`, id, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_payloads: %v", err)
	}
}

type leaseReclaimFields struct {
	status       string
	assignedTo   sql.NullString
	heartbeatAt  sql.NullTime
	generation   int64
	reclaimCount int64
}

func readLeaseReclaimFields(t *testing.T, db *sql.DB, id string) leaseReclaimFields {
	t.Helper()
	var f leaseReclaimFields
	if err := db.QueryRow(`
		SELECT status, assigned_to, heartbeat_at, generation, reclaim_count FROM workflow_leases WHERE id = $1
	`, id).Scan(&f.status, &f.assignedTo, &f.heartbeatAt, &f.generation, &f.reclaimCount); err != nil {
		t.Fatalf("read workflow_leases reclaim fields for %s: %v", id, err)
	}
	return f
}

func TestReapStaleInstancesUpdatesTheLeaseRows(t *testing.T) {
	store, db := reapLeaseTestStore(t)
	ctx := context.Background()

	const id = "reap-lease-1"
	seedStaleRunningTriple(t, store, db, id, "reap-lease-def", "worker-reap-1", time.Hour, "")

	n, err := store.ReapStaleInstances(ctx, time.Minute, 0)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d, want 1", n)
	}

	lease := readLeaseReclaimFields(t, db, id)
	if lease.status != "ready" {
		t.Errorf("workflow_leases.status = %q, want %q", lease.status, "ready")
	}
	if lease.assignedTo.Valid {
		t.Errorf("workflow_leases.assigned_to = %v, want NULL", lease.assignedTo)
	}
	if lease.heartbeatAt.Valid {
		t.Errorf("workflow_leases.heartbeat_at = %v, want NULL", lease.heartbeatAt)
	}
	if lease.generation != 2 {
		t.Errorf("workflow_leases.generation = %d, want 2", lease.generation)
	}
	if lease.reclaimCount != 1 {
		t.Errorf("workflow_leases.reclaim_count = %d, want 1", lease.reclaimCount)
	}

	var instStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, id).Scan(&instStatus); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.status != instStatus {
		t.Errorf("workflow_leases.status (%q) disagrees with workflow_instances (%q)", lease.status, instStatus)
	}
}

func TestReapStaleInstancesWithAPendingTerminalStatusGoesToTerminatingNotReady(t *testing.T) {
	store, db := reapLeaseTestStore(t)
	ctx := context.Background()

	const id = "reap-lease-terminating-1"
	seedStaleRunningTriple(t, store, db, id, "reap-lease-terminating-def", "worker-reap-2", time.Hour, "done")

	n, err := store.ReapStaleInstances(ctx, time.Minute, 0)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d, want 1", n)
	}

	lease := readLeaseReclaimFields(t, db, id)
	if lease.status != "terminating" {
		t.Errorf("workflow_leases.status = %q, want %q (pending_terminal_status was set)", lease.status, "terminating")
	}
}

func TestReapStaleInstancesLeavesAFreshHeartbeatUntouched(t *testing.T) {
	store, db := reapLeaseTestStore(t)
	ctx := context.Background()

	const id = "reap-lease-fresh-1"
	seedStaleRunningTriple(t, store, db, id, "reap-lease-fresh-def", "worker-reap-3", 0, "")

	n, err := store.ReapStaleInstances(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if n != 0 {
		t.Fatalf("ReapStaleInstances reclaimed %d, want 0 (heartbeat is fresh)", n)
	}

	lease := readLeaseReclaimFields(t, db, id)
	if lease.status != "running" {
		t.Errorf("workflow_leases.status = %q, want unchanged %q", lease.status, "running")
	}
	if lease.generation != 1 {
		t.Errorf("workflow_leases.generation = %d, want unchanged 1", lease.generation)
	}
}

func TestReapStaleInstancesExceptUpdatesTheLeaseRows(t *testing.T) {
	store, db := reapLeaseTestStore(t)
	ctx := context.Background()

	const excludedID = "reap-except-excluded-1"
	const reapedID = "reap-except-reaped-1"
	seedStaleRunningTriple(t, store, db, excludedID, "reap-except-def", "worker-reap-4", time.Hour, "")
	seedStaleRunningTriple(t, store, db, reapedID, "reap-except-def", "worker-reap-5", time.Hour, "")

	n, err := store.ReapStaleInstancesExcept(ctx, time.Minute, 0, []GenerationKey{{WorkflowID: excludedID, Generation: 1}})
	if err != nil {
		t.Fatalf("ReapStaleInstancesExcept: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReapStaleInstancesExcept reclaimed %d, want 1", n)
	}

	excludedLease := readLeaseReclaimFields(t, db, excludedID)
	if excludedLease.status != "running" {
		t.Errorf("excluded workflow_leases.status = %q, want unchanged %q", excludedLease.status, "running")
	}
	reapedLease := readLeaseReclaimFields(t, db, reapedID)
	if reapedLease.status != "ready" {
		t.Errorf("reaped workflow_leases.status = %q, want %q", reapedLease.status, "ready")
	}
}
