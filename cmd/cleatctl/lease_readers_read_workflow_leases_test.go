package main

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245: cleatctl's PostgreSQL readers of a run's state read
// workflow_leases and workflow_payloads. workflow_instances holds none of it
// since B5, so these are the only tests that run those statements against a real
// narrowed schema: a reader pointed back at workflow_instances is a runtime
// `column "status" does not exist`, which nothing else here would see. They used
// to prove it by making the instance copy disagree; there is no copy to disagree
// with, and they now assert the values read back.
//
// The statuses are deliberately not real ones. check-db counts every run in
// the database, and this package shares that database with other tests; a
// status no other test writes makes the count about this run alone.

const leaseReaderDef = "cleatctl-b3a-def"

func seedLeaseRun(t *testing.T, db *sql.DB, status string) string {
	t.Helper()
	id := fmt.Sprintf("cleatctl-b3a-%d", time.Now().UnixNano())
	if _, err := db.Exec(`
		INSERT INTO workflow_defs (name, version, wasm_bytes)
		VALUES ($1, 1, '\x00'::bytea)
		ON CONFLICT (tenant_id, name, version) DO NOTHING`, leaseReaderDef); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	testutil.SeedRun(t, db, testutil.RunSeed{
		ID: id, DefName: leaseReaderDef, Input: `{"k":"v"}`,
		Status: status, AssignedTo: "w-lease", Generation: 7,
	})
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })
	return id
}

func TestLoadWorkflowInstanceReadsTheLeaseColumnsFromWorkflowLeases(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	id := seedLeaseRun(t, db, "running")

	inst, err := loadWorkflowInstance(context.Background(), db, dialectPostgres, id)
	if err != nil {
		t.Fatalf("loadWorkflowInstance: %v", err)
	}
	if inst.Status != "running" || inst.AssignedTo != "w-lease" || inst.Generation != 7 {
		t.Fatalf("loadWorkflowInstance = (status %q, assigned_to %q, generation %d), want (running, w-lease, 7) from workflow_leases",
			inst.Status, inst.AssignedTo, inst.Generation)
	}
	if inst.DefName != leaseReaderDef || inst.DefVersion != 1 || string(inst.Input) != `{"k": "v"}` {
		t.Fatalf("loadWorkflowInstance = (def %q v%d, input %s), want (%s v1, {\"k\": \"v\"}) from workflow_payloads",
			inst.DefName, inst.DefVersion, inst.Input, leaseReaderDef)
	}
}

func TestCheckDBCountsStatusFromWorkflowLeases(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	seedLeaseRun(t, db, "b3a-lease-status")

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
	if counts["b3a-lease-status"] != 1 {
		t.Fatalf("check-db status counts = %v, want b3a-lease-status=1", counts)
	}
}
