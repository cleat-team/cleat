package main

import (
	"database/sql"
	"os/exec"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1887. `cleat rollback <name> <version>` connected, pinged, checked the
// version existed, and printed "Rolled back ... New instances will use version
// N" -- with no write anywhere. cleat#1893 made it fail loudly rather than lie,
// leaving the real write open on one question: which tenant scope it takes.
//
// That question is now answered the only way that cannot disagree with itself:
// resolveDeployTenant, the same function `deploy` uses. A rollback landing on a
// different tenant than the deploy it reverses would be worse than the no-op,
// and calling the same function is the only way to be sure they agree.
//
// WHAT THESE TESTS ASSERT, and why the first one is the one that matters: that
// the DATABASE CHANGED. The defect was not a wrong message, it was a message
// with nothing behind it, and only a test that reads the row back can tell
// those apart. Asserting on stdout would pass against the original defect.

func TestRollbackWritesAPinANewRunWillResolveTo(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	const wf = "rollback-pin-wf"
	seedRollbackWorkflowDef(t, db, wf)
	seedRollbackWorkflowDefVersion(t, db, wf, 2)

	out, err := runCleatRollback(t, wf, "1")
	if err != nil {
		t.Fatalf("cleat rollback failed: %v\n%s", err, out)
	}

	// THE ASSERTION THE OLD DEFECT WOULD HAVE FAILED.
	version, weight := readRoutingPin(t, db, wf)
	if version != 1 {
		t.Errorf("routing pin targets version %d, want 1 -- the rollback printed success; "+
			"this checks it wrote something", version)
	}
	if weight != 1.0 {
		t.Errorf("pin weight is %v, want 1.0: a weight below 1 is a weighted experiment, "+
			"not a rollback, and would route only a fraction of new runs", weight)
	}
	if !strings.Contains(out, "version 1") {
		t.Errorf("output does not name the version it pinned: %s", out)
	}
}

// A pin is worthless if the resolution path does not read it. This asserts the
// row is consumed by the function that actually decides a new run's version --
// server.go consults PickVersionByRouting before falling back to latest -- so
// the test fails if the pin is written somewhere nothing looks.
func TestTheRollbackPinIsWhatVersionResolutionReads(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	const wf = "rollback-resolve-wf"
	seedRollbackWorkflowDef(t, db, wf)
	seedRollbackWorkflowDefVersion(t, db, wf, 2)

	if out, err := runCleatRollback(t, wf, "1"); err != nil {
		t.Fatalf("cleat rollback failed: %v\n%s", err, out)
	}

	got := pickVersionByRoutingForTest(t, db, wf)
	if got != 1 {
		t.Errorf("version resolution returned %d after a rollback to 1. The pin exists but "+
			"the path that chooses a new run's version does not read it, which is the same "+
			"outcome as not writing it at all", got)
	}
}

// --clear returns the workflow to latest-wins. Without it a rollback is
// permanent and the next deploy silently does nothing.
func TestRollbackClearRemovesThePin(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	const wf = "rollback-clear-wf"
	seedRollbackWorkflowDef(t, db, wf)

	if out, err := runCleatRollback(t, wf, "1"); err != nil {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	out, err := runCleatArgs(t, "rollback", "--clear", wf)
	if err != nil {
		t.Fatalf("rollback --clear failed: %v\n%s", err, out)
	}
	if n := countRoutingRules(t, db, wf); n != 0 {
		t.Errorf("%d routing rule(s) remain after --clear, want 0", n)
	}
	if got := pickVersionByRoutingForTest(t, db, wf); got != 0 {
		t.Errorf("resolution still returns %d after --clear; 0 means 'no rule, use latest'", got)
	}
}

// A weighted experiment is someone else's deliberate state. Refusing is
// recoverable; silently discarding it is not.
func TestRollbackRefusesToDiscardALiveExperiment(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	const wf = "rollback-experiment-wf"
	seedRollbackWorkflowDef(t, db, wf)
	seedRollbackWorkflowDefVersion(t, db, wf, 2)
	if _, err := db.Exec(
		`INSERT INTO workflow_routing (workflow_name, target_version, weight, tenant_id)
		 VALUES ($1, 2, 0.5, '00000000-0000-0000-0000-000000000000')`, wf); err != nil {
		t.Fatalf("seed experiment: %v", err)
	}

	out, err := runCleatRollback(t, wf, "1")
	if err == nil {
		t.Fatalf("rollback succeeded against a live weighted experiment, discarding it: %s", out)
	}
	if !strings.Contains(out, "--clear") {
		t.Errorf("refusal does not tell the operator how to proceed: %s", out)
	}
	// And it must not have half-applied.
	if n := countRoutingRules(t, db, wf); n != 1 {
		t.Errorf("%d routing rules after a refused rollback, want the 1 experiment untouched", n)
	}
}

// An unknown version must not leave a pin behind. The EXISTS check and the
// write are in one transaction precisely so a refusal writes nothing.
func TestRollbackToAnUnknownVersionWritesNothing(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	const wf = "rollback-unknown-wf"
	seedRollbackWorkflowDef(t, db, wf)

	out, err := runCleatRollback(t, wf, "99")
	if err == nil {
		t.Fatalf("rollback to a nonexistent version succeeded: %s", out)
	}
	if n := countRoutingRules(t, db, wf); n != 0 {
		t.Errorf("%d routing rule(s) written for a version that does not exist", n)
	}
}

// ---- helpers ----

func runCleatRollback(t *testing.T, name, version string) (string, error) {
	t.Helper()
	return runCleatArgs(t, "rollback", name, version)
}

func runCleatArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--db", testutil.PostgresTestDSN()}, args...)
	out, err := exec.Command(cleatBinary, full...).CombinedOutput()
	return string(out), err
}

func readRoutingPin(t *testing.T, db *sql.DB, name string) (int, float64) {
	t.Helper()
	var version int
	var weight float64
	err := db.QueryRow(
		`SELECT target_version, weight FROM workflow_routing WHERE workflow_name = $1`,
		name).Scan(&version, &weight)
	if err != nil {
		t.Fatalf("no routing pin for %q after rollback: %v", name, err)
	}
	return version, weight
}

func countRoutingRules(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM workflow_routing WHERE workflow_name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("count routing rules: %v", err)
	}
	return n
}

// pickVersionByRoutingForTest mirrors what the worker's resolution does: a
// weighted pick over the rules for this workflow. With a single rule at weight
// 1.0 it is deterministic. 0 means "no rule -- use the latest version".
func pickVersionByRoutingForTest(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var version int
	err := db.QueryRow(
		`SELECT target_version FROM workflow_routing WHERE workflow_name = $1 ORDER BY weight DESC LIMIT 1`,
		name).Scan(&version)
	if err == sql.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("resolve version: %v", err)
	}
	return version
}

func seedRollbackWorkflowDef(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	seedRollbackWorkflowDefVersion(t, db, name, 1)
}

func seedRollbackWorkflowDefVersion(t *testing.T, db *sql.DB, name string, version int) {
	t.Helper()
	const tenantUUID = "00000000-0000-0000-0000-000000000000"
	if _, err := db.Exec(
		`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		 VALUES ($1, $2, $3, 1, 0, $4)
		 ON CONFLICT (tenant_id, name, version) DO NOTHING`,
		name, version, []byte{0x00, 0x61, 0x73, 0x6d}, tenantUUID); err != nil {
		t.Fatalf("seed workflow_defs v%d: %v", version, err)
	}
}
