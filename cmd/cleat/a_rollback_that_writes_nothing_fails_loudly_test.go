package main

import (
	"database/sql"
	"os/exec"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1887. `cleat rollback <name> <version>` connected, pinged, checked
// that the version existed, and printed "Rolled back ... New instances will
// use version N" -- with no write anywhere. workflow_defs has no
// active/stable pointer column for it to have written to (only disabled_at
// and gc_eligible), and a new run always resolves to the latest version
// regardless (cmd/cleat-worker/server.go's targetVersion default), so the
// command was a no-op that told the operator it had succeeded.
//
// THE MINIMUM ACCEPTABLE OUTCOME, per the issue, and the only thing fixed
// here: fail loudly instead of lying. Not implemented: a real rollback,
// which needs a routing-table write (workflow_routing already exists and is
// already consulted -- PickVersionByRouting, server.go) and a decision this
// issue leaves open -- runRollback opens a raw, tenant-less connection,
// unlike every other RLS-scoped read path in this file, and writing to the
// wrong tenant on a rollback command is worse than the no-op it would
// replace. That decision is not made here.
//
// This is the first test runRollback has ever had (`grep -rn runRollback
// cmd/cleat/*_test.go` returned nothing before this file).
func TestRollbackFailsLoudlyInsteadOfLying(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if cleatBinary == "" {
		t.Skip("cleatBinary not built (short mode)")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	seedRollbackWorkflowDef(t, db, "rollback-test-wf")

	dsn := testutil.PostgresTestDSN()
	cmd := exec.Command(cleatBinary, "--db", dsn, "rollback", "rollback-test-wf", "1")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("cleat rollback exited 0. It performs no write, so this can only be the "+
			"old lie -- a fake success message -- reappearing. Output:\n%s", out)
	}
	if strings.Contains(string(out), "Rolled back") {
		t.Errorf("output claims a rollback happened despite no write existing to perform "+
			"it: %s", out)
	}
}

// seedRollbackWorkflowDef inserts the one row runRollback's EXISTS check
// reads -- the same minimal shape used elsewhere in this repo's own tests
// for the same table (e.g. engine's seedWorkflowDef in
// concurrent_starts_under_one_key_all_get_an_answer_test.go).
func seedRollbackWorkflowDef(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	const tenantUUID = "00000000-0000-0000-0000-000000000000"
	if _, err := db.Exec(
		`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		 VALUES ($1, 1, $2, 1, 0, $3)
		 ON CONFLICT (tenant_id, name, version) DO NOTHING`,
		name, []byte{0x00, 0x61, 0x73, 0x6d}, tenantUUID); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
}
