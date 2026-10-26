package main

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 B3a: cleatctl's PostgreSQL readers of the lease columns read
// workflow_leases, not workflow_instances. Each test seeds one run whose two
// copies disagree, so a reader still on workflow_instances gives the other
// copy's answer.
//
// The statuses are deliberately not real ones. check-db counts every run in
// the database, and this package shares that database with other tests; a
// status no other test writes makes the count about this run alone.

func seedDivergedLeaseRun(t *testing.T, db *sql.DB, leaseStatus, instanceStatus string) string {
	t.Helper()
	id := fmt.Sprintf("cleatctl-b3a-%d", time.Now().UnixNano())
	if _, err := db.Exec(`
		INSERT INTO workflow_defs (name, version, wasm_bytes)
		VALUES ('cleatctl-b3a-def', 1, '\x00'::bytea)
		ON CONFLICT (tenant_id, name, version) DO NOTHING`); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status, assigned_to, generation)
		VALUES ($1, 'cleatctl-b3a-def', 1, $2, 'w-instance', 3)`, id, instanceStatus); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })
	testutil.SyncWorkflowPayload(t, db, id)
	testutil.SyncWorkflowLease(t, db, id)
	if _, err := db.Exec(`
		UPDATE workflow_leases SET status = $2, assigned_to = 'w-lease', generation = 7 WHERE id = $1`,
		id, leaseStatus); err != nil {
		t.Fatalf("diverge workflow_leases: %v", err)
	}
	return id
}

func TestLoadWorkflowInstanceReadsTheLeaseColumnsFromWorkflowLeases(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	id := seedDivergedLeaseRun(t, db, "running", "ready")

	inst, err := loadWorkflowInstance(context.Background(), db, dialectPostgres, id)
	if err != nil {
		t.Fatalf("loadWorkflowInstance: %v", err)
	}
	if inst.Status != "running" || inst.AssignedTo != "w-lease" || inst.Generation != 7 {
		t.Fatalf("loadWorkflowInstance = (status %q, assigned_to %q, generation %d), want (running, w-lease, 7) from workflow_leases -- (ready, w-instance, 3) is workflow_instances",
			inst.Status, inst.AssignedTo, inst.Generation)
	}
}

func TestCheckDBCountsStatusFromWorkflowLeases(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	seedDivergedLeaseRun(t, db, "b3a-lease-status", "b3a-instance-status")

	rows, err := db.Query(instanceStatusCountsSQL(dialectPostgres, ""))
	if err != nil {
		t.Fatalf("status counts: %v", err)
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[status] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if counts["b3a-lease-status"] != 1 || counts["b3a-instance-status"] != 0 {
		t.Fatalf("check-db status counts = %v, want b3a-lease-status=1 and no b3a-instance-status -- it read workflow_instances.status", counts)
	}
}
