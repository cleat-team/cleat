package main

import (
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3314. `versions`, `rollback` and `rollback --clear` never called
// set_config('cleat.tenant_id', ...) the way `deploy`'s own cleat#2065 fix
// does, even though workflow_defs/workflow_routing FORCE row-level security
// and the worker's own startup check refuses any connection RLS does not
// apply to -- the one the worker's error message tells an operator to use,
// and the one `deploy` is documented ("cleat#3027... exits 0 ... both work")
// to work under.
//
// Every other test in this package runs `cleat ...` against
// testutil.PostgresTestDSN(), which is the superuser/owner role: RLS does
// not apply to it, so a missing set_config was invisible to every test that
// came before this one -- the exact §1.10 shape CLAUDE.md names: "the
// policies were present, correct, tested, and bypassed in practice by every
// connection that had ever run against them."
//
// appRoleDSN below connects as cleat_app instead, which is the only
// connection that can observe either half of the defect this fixes:
//   - before the fix, `versions`/`rollback`/`rollback --clear` all failed
//     outright with "cleat.tenant_id is not set" on this role.
//   - before the fix, `versions` ALSO had no tenant_id predicate on its
//     query at all, so even once it stopped failing it would have returned
//     every tenant's rows merged into one list -- which an app-role
//     connection alone cannot show, because RLS hides the other tenant's
//     rows from it by construction. The cross-tenant half is instead shown
//     on the owner connection in TestVersionsDoesNotLeakAcrossTenants below.

// appRoleDSN grants cleat_app a login (the schema baseline creates it
// NOLOGIN, deliberately -- see engine/flush_rls_test.go's identical
// appRolePassword constant and comment) and returns a DSN that connects as
// it: unprivileged, NOBYPASSRLS, and therefore subject to every row-level
// security policy. It opens one connection to prove the premise -- a
// connection that turned out to be RLS-exempt would pass every assertion
// below for the wrong reason -- then closes it; the actual test traffic
// goes through subprocesses of the `cleat` binary, which open their own.
func appRoleDSN(t *testing.T, owner *sql.DB) string {
	t.Helper()
	const appRolePassword = "cleat-app-test-pw"
	if _, err := owner.Exec(fmt.Sprintf(
		`ALTER ROLE cleat_app LOGIN PASSWORD '%s'`, appRolePassword)); err != nil {
		t.Fatalf("granting cleat_app a login (is the schema baseline applied?): %v", err)
	}

	dsn := testutil.PostgresTestDSN()
	at := strings.Index(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at < 0 || scheme < 0 {
		t.Fatalf("cannot derive a cleat_app DSN from the configured test DSN %q", dsn)
	}
	appDSN := dsn[:scheme+3] + "cleat_app:" + appRolePassword + dsn[at:]

	db, err := sql.Open("postgres", appDSN)
	if err != nil {
		t.Fatalf("opening the cleat_app connection: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("cleat_app cannot connect: %v", err)
	}
	var bypass bool
	if err := db.QueryRow(
		`SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("checking rolbypassrls: %v", err)
	}
	if bypass {
		t.Fatalf("cleat_app has BYPASSRLS, so this test cannot observe an RLS failure; " +
			"the schema baseline asserts NOBYPASSRLS every time it runs")
	}
	return appDSN
}

func seedWorkflowDefForTenant(t *testing.T, db *sql.DB, name, tenantID string, version int) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		 VALUES ($1, $2, $3, 1, 0, $4)
		 ON CONFLICT (tenant_id, name, version) DO NOTHING`,
		name, version, []byte{0x00, 0x61, 0x73, 0x6d}, tenantID); err != nil {
		t.Fatalf("seed workflow_defs for tenant %s: %v", tenantID, err)
	}
}

// TestVersionsRollbackAndRollbackClearWorkUnderCleatApp is the regression test
// for the hard-failure half of cleat#3314: before the fix, all three calls
// below exited non-zero with "cleat.tenant_id is not set" on this
// connection, never reaching their own logic at all.
func TestVersionsRollbackAndRollbackClearWorkUnderCleatApp(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, owner)

	const tenantID = "33333333-3333-3333-3333-333333333333"
	const wf = "app-role-wf"
	seedWorkflowDefForTenant(t, owner, wf, tenantID, 1)
	seedWorkflowDefForTenant(t, owner, wf, tenantID, 2)

	appDSN := appRoleDSN(t, owner)

	runAsApp := func(args ...string) (string, error) {
		full := append([]string{"--tenant", tenantID, "--db", appDSN}, args...)
		out, err := exec.Command(cleatBinary, full...).CombinedOutput()
		return string(out), err
	}

	out, err := runAsApp("versions", wf)
	if err != nil {
		t.Fatalf("cleat versions failed under cleat_app: %v\n%s", err, out)
	}
	if !strings.Contains(out, "2") || !strings.Contains(out, "1") {
		t.Errorf("cleat versions under cleat_app did not list both seeded versions:\n%s", out)
	}

	out, err = runAsApp("rollback", wf, "1")
	if err != nil {
		t.Fatalf("cleat rollback failed under cleat_app: %v\n%s", err, out)
	}

	out, err = runAsApp("rollback", "--clear", wf)
	if err != nil {
		t.Fatalf("cleat rollback --clear failed under cleat_app: %v\n%s", err, out)
	}
}

// TestVersionsDoesNotLeakAcrossTenants is the regression test for the
// cross-tenant half of cleat#3314: on a connection RLS cannot filter (the
// owner/superuser role every migration and most of this package's other
// tests already use), `versions` used to have no tenant_id predicate at all
// and returned every tenant's rows for a shared workflow name merged into one
// list, indistinguishable from each other.
func TestVersionsDoesNotLeakAcrossTenants(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)

	const tenantA = "44444444-4444-4444-4444-444444444444"
	const tenantB = "55555555-5555-5555-5555-555555555555"
	const wf = "cross-tenant-versions-wf"
	seedWorkflowDefForTenant(t, db, wf, tenantA, 1)
	seedWorkflowDefForTenant(t, db, wf, tenantB, 1)
	seedWorkflowDefForTenant(t, db, wf, tenantB, 2)

	runVersionsFor := func(tenantID string) (string, error) {
		out, err := exec.Command(cleatBinary, "--tenant", tenantID, "--db", testutil.PostgresTestDSN(),
			"versions", wf).CombinedOutput()
		return string(out), err
	}

	outA, err := runVersionsFor(tenantA)
	if err != nil {
		t.Fatalf("cleat versions for tenant A: %v\n%s", err, outA)
	}
	linesA := strings.Fields(outA)
	if len(linesA) != 1 || linesA[0] != "1" {
		t.Errorf("tenant A should see exactly its own version [1], got %v -- "+
			"a longer list means tenant B's version(s) leaked in", linesA)
	}

	outB, err := runVersionsFor(tenantB)
	if err != nil {
		t.Fatalf("cleat versions for tenant B: %v\n%s", err, outB)
	}
	linesB := strings.Fields(outB)
	if len(linesB) != 2 {
		t.Errorf("tenant B should see exactly its own two versions, got %v -- "+
			"missing a version, or seeing tenant A's as well", linesB)
	}
}
