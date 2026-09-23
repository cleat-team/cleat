package testutil

// Administrative access for SQL Server test teardown.
//
// IMPROVEMENT-PLAN 3.37 / 2.71. SQL Server applies a security policy to every
// principal -- sysadmin, db_owner and dbo included -- so on a database built
// from the shipped migrations a plain pool cannot see across tenants. Teardown
// that issues DELETE on such a pool matches nothing, reports no error, and
// leaves every row behind. Migration 012 introduces the cleat_admin role as the
// exemption; this file provisions a member of it for the test harness.
//
// The gate is "does this database have security policies", not "does the role
// exist". On the hand-written schema there are no policies, so a plain
// connection already is an administrative one and nothing needs provisioning;
// on the shipped schema there are, and then the absence of the role is a hard
// error rather than something to work around silently. Getting that condition
// backwards would reintroduce the exact defect -- teardown that quietly does
// nothing.

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

const (
	// A login the harness creates on the test server. Not a credential in any
	// meaningful sense -- CLEAT_TEST_MSSQL already carries sa's password in
	// the clear, and this only ever exists on a throwaway test instance. The
	// shipped migration deliberately creates no login at all, leaving that to
	// the deployment (migrations/mssql/012_admin_role.sql).
	mssqlTestAdminLogin    = "cleat_test_admin"
	mssqlTestAdminPassword = "CleatTestAdmin123!"
)

var (
	mssqlAdminMu    sync.Mutex
	mssqlAdminPools = map[string]*sql.DB{}
	// mssqlAdminRefs counts live callers of MSSQLAdminDB per DSN. cleat#2125:
	// applyMSSQLCrossTenantOptIn flips the WHOLE DATABASE's predicate form
	// from 'plain' to 'admin', and nothing used to flip it back -- so the
	// first test in a package (or a whole `go test` process) to call
	// MSSQLAdminDB left every later test, in every later package sharing the
	// same CLEAT_TEST_MSSQL, running against a predicate no default deployment
	// installs. Refcounted rather than restored on the first Cleanup: several
	// tests (including parallel ones) can hold the admin pool at once, and
	// restoring while any of them is still mid-test would pull the form out
	// from under it. Only the caller whose Cleanup drops the count to zero
	// restores 'plain' and evicts the cached pool, so the next caller
	// re-establishes 'admin' from scratch rather than reusing a pool that
	// authenticates fine but now grants nothing (cleat#1541's failure mode).
	mssqlAdminRefs = map[string]int{}
)

// mssqlHasSecurityPolicies reports whether this database enforces RLS at all.
func mssqlHasSecurityPolicies(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sys.security_policies`).Scan(&n); err != nil {
		t.Fatalf("count security policies: %v", err)
	}
	return n > 0
}

// MSSQLAdminDB returns a handle that can read and delete across tenants.
//
// On a database with no security policies it returns db unchanged: there is
// nothing to be exempt from, and handing back a second pool would only add a
// connection. On a database that does enforce RLS it provisions a member of
// cleat_admin and returns a pool authenticated as it.
//
// Pools are cached per DSN. Teardown runs once per test and opening a fresh
// pool each time would leave hundreds of them behind over a suite.
func MSSQLAdminDB(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	if !mssqlHasSecurityPolicies(t, db) {
		return db
	}

	baseDSN := os.Getenv("CLEAT_TEST_MSSQL")
	if baseDSN == "" {
		baseDSN = "sqlserver://sa:CleatTest123!@localhost:1433?database=cleat"
	}

	mssqlAdminMu.Lock()
	defer mssqlAdminMu.Unlock()
	if pool, ok := mssqlAdminPools[baseDSN]; ok {
		mssqlAdminRefs[baseDSN]++
		t.Cleanup(func() { mssqlReleaseAdminDB(t, baseDSN) })
		return pool
	}

	requireMSSQLAdminRole(t, db)
	// Since cleat#1541 the SHIPPED predicate does not mention IS_ROLEMEMBER, so
	// membership on its own grants nothing and this whole path would hand back a
	// pool that deletes silently -- the exact failure the comment below is
	// about, arriving by a route that comment could not anticipate.
	//
	// Test teardown is a legitimate cross-tenant reader: CleanupMSSQLTestData
	// deletes every tenant's rows by design. So the suite opts in, the way a
	// deployment that wants --claim-across-tenants does.
	applyMSSQLCrossTenantOptIn(t, db)
	provisionMSSQLAdminLogin(t, db)

	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse CLEAT_TEST_MSSQL: %v", err)
	}
	u.User = url.UserPassword(mssqlTestAdminLogin, mssqlTestAdminPassword)
	pool, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open the administrative SQL Server connection: %v", err)
	}
	if err := pool.Ping(); err != nil {
		t.Fatalf("ping as %s: %v", mssqlTestAdminLogin, err)
	}

	// Prove it before handing it out. A pool that authenticates but is not
	// actually in the role would delete nothing and report success, which is
	// the failure this whole path exists to remove -- so it is checked here
	// rather than trusted from the GRANT above having not errored.
	var isMember sql.NullInt64
	if err := pool.QueryRow(`SELECT IS_ROLEMEMBER(N'cleat_admin')`).Scan(&isMember); err != nil {
		t.Fatalf("check cleat_admin membership: %v", err)
	}
	if isMember.Int64 != 1 {
		t.Fatalf("%s authenticated but reads IS_ROLEMEMBER('cleat_admin') = %v; "+
			"teardown through this connection would silently delete nothing",
			mssqlTestAdminLogin, isMember)
	}

	// AND that membership buys something. Since cleat#1541 the two questions
	// came apart: a member under the plain predicate reads IS_ROLEMEMBER = 1
	// and still sees zero rows, so the check above passes while teardown
	// deletes nothing -- which is precisely what it was written to prevent,
	// reached by a route that did not exist when it was written.
	var form string
	if err := pool.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
		t.Fatalf("read admin.rls_predicate_form as %s: %v. Migration 075 creates it; "+
			"without it there is no way to tell whether cleat_admin membership grants "+
			"anything", mssqlTestAdminLogin, err)
	}
	if form != "admin" {
		t.Fatalf("%s is a member of cleat_admin but the installed predicate is %q, so "+
			"membership admits nothing and teardown would silently delete nothing "+
			"(cleat#1541). applyMSSQLCrossTenantOptIn should have opted this database in",
			mssqlTestAdminLogin, form)
	}

	mssqlAdminPools[baseDSN] = pool
	mssqlAdminRefs[baseDSN]++
	t.Cleanup(func() { mssqlReleaseAdminDB(t, baseDSN) })
	return pool
}

// mssqlReleaseAdminDB is the Cleanup counterpart of every MSSQLAdminDB call
// that flipped or reused the admin predicate form. When the count for baseDSN
// reaches zero, no live caller still needs 'admin', so it restores 'plain'
// (migration 102 is idempotent -- see restoreMSSQLPlainPredicate) and evicts
// the cached pool, so the next MSSQLAdminDB call re-provisions and
// re-verifies rather than handing back a pool that now authenticates into a
// predicate it never checked.
//
// Takes baseDSN only, not the caller's plain db -- see
// restoreMSSQLPlainPredicate's own comment for why db cannot be used here:
// every StoreBackend.Setup in this package hands back a teardown the test
// body defers, and that defer closes db before this Cleanup ever runs.
func mssqlReleaseAdminDB(t *testing.T, baseDSN string) {
	t.Helper()
	mssqlAdminMu.Lock()
	defer mssqlAdminMu.Unlock()

	mssqlAdminRefs[baseDSN]--
	if mssqlAdminRefs[baseDSN] > 0 {
		return
	}
	delete(mssqlAdminRefs, baseDSN)

	pool, ok := mssqlAdminPools[baseDSN]
	if !ok {
		// Already restored and evicted by a concurrent last-releaser, or this
		// DSN's database has no security policies (MSSQLAdminDB returned db
		// unchanged and never incremented the refcount in the first place, in
		// which case this function is never registered as a Cleanup at all).
		return
	}
	delete(mssqlAdminPools, baseDSN)

	restoreMSSQLPlainPredicate(t, baseDSN)
	if err := pool.Close(); err != nil {
		t.Logf("closing the administrative SQL Server pool: %v", err)
	}
}

// restoreMSSQLPlainPredicate re-applies
// migrations/mssql/102_a_plugin_sweep_can_ask_which_workflows_are_in_flight.sql,
// not 075 -- both are idempotent and both restore the plain predicate, but
// only 102 also carries the cleat_dispatcher disjunct (cleat#2125). 075's own
// body does not: migration.Runner never re-runs an already-applied file on a
// real deployment, so 075 is reachable only through this helper, and editing
// its SQL in place to add the disjunct would make it byte-identical to 102's
// definition -- which left engine/routine_definition_drift_test.go's
// dbo.fn_tenant_filter discriminator with no earlier/latest difference to
// find (measured: "the latest introduces neither a new identifier nor a new
// body line ... nothing here can tell the versions apart"). Re-pointing this
// helper at 102 instead keeps 075's shipped bytes untouched and restores
// exactly what a real, fully-migrated, not-opted-in deployment has.
//
// Takes baseDSN, not the caller's plain db, and opens its OWN connection --
// store_backends_test.go's mssqlRowDisappearanceReporter already documents
// exactly why, for the identical shape of bug, one comment above where this
// function is called from: "the test's `defer teardown()` closes db first"
// runs BEFORE any t.Cleanup callback, defers in a test body being ordinary
// Go defers that fire as the test function returns, ahead of the testing
// package's own Cleanup machinery. Every StoreBackend.Setup in this package
// returns exactly that shape -- teardown calls db.Close() directly, and the
// test body says `defer teardown()` -- so by the time THIS function's own
// t.Cleanup fires, db is already closed and db.Begin() fails with "sql:
// database is closed" (measured: every registeredBackends-driven mssql test
// in the package failed this way the first time this used db instead).
func restoreMSSQLPlainPredicate(t *testing.T, baseDSN string) {
	t.Helper()
	restoreDB, err := sql.Open("sqlserver", baseDSN)
	if err != nil {
		t.Fatalf("open a connection to restore the plain predicate: %v", err)
	}
	defer restoreDB.Close()

	path := filepath.Join(repoRootForMSSQLTestutil(t), "migrations", "mssql",
		"102_a_plugin_sweep_can_ask_which_workflows_are_in_flight.sql")
	execMSSQLBatchFile(t, restoreDB, path)
}

// AdminDB returns the handle a test should use when it needs to see or change
// rows without regard to which tenant owns them -- seeding a fixture for
// another tenant, reading a row back to check what the code under test wrote,
// or deleting one at teardown.
//
// It exists because the three dialects give a test wildly different privileges
// by default, and only one of them says so out loud:
//
//	PostgreSQL  the test role is a superuser, and a superuser bypasses RLS
//	            unconditionally. Every raw read-back in the suite already runs
//	            exempt from the policies, silently.
//	MySQL       has no row-level security. Tenancy is a separate database.
//	SQL Server  applies its security policies to every principal, sa and dbo
//	            included, so the same read-back returns nothing at all.
//
// So a dialect-generic test that wrote through a store and then read the row
// back on its own connection was doing two different things: on PostgreSQL it
// bypassed the fence, and on SQL Server it hit it. Under the hand-written test
// schema that never showed, because that schema had no policies. Built from the
// shipped migrations it shows immediately -- and in both directions. In
// TestCascadeDelete the unscoped DELETE matched no rows, so nothing cascaded;
// three of the five child-table assertions then failed, and the other two
// *passed*, because their rows were still there and the same policy hid them
// from the count.
//
// Returning db unchanged for the two dialects that need nothing keeps the call
// sites free of dialect switches, and keeps the asymmetry documented in one
// place rather than restated at each of them.
func AdminDB(t *testing.T, db *sql.DB, dialect Dialect) *sql.DB {
	t.Helper()
	if dialect == DialectMSSQL {
		return MSSQLAdminDB(t, db)
	}
	return db
}

// requireMSSQLAdminRole fails loudly when the database enforces RLS but has not
// had migration 012 applied. Falling back to the plain pool here is what would
// make teardown silently no-op again.
func requireMSSQLAdminRole(t *testing.T, db *sql.DB) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sys.database_principals WHERE name = N'cleat_admin' AND type = 'R'`).
		Scan(&n); err != nil {
		t.Fatalf("look up the cleat_admin role: %v", err)
	}
	if n == 0 {
		t.Fatalf("this database enforces row-level security but has no cleat_admin role, " +
			"so nothing can read or delete across tenants.\n" +
			"Apply migrations/mssql/012_admin_role.sql (IMPROVEMENT-PLAN 3.37), or drop and " +
			"recreate the test database so the shipped migrations run from scratch.")
	}
}

// provisionMSSQLAdminLogin creates the login, maps it into this database and
// puts it in cleat_admin. Every step tolerates "already exists", because the
// login is server-level and shared by every package that runs concurrently
// against the same instance.
func provisionMSSQLAdminLogin(t *testing.T, db *sql.DB) {
	t.Helper()
	steps := []struct {
		what string
		sql  string
	}{
		{"create login", fmt.Sprintf(
			`IF SUSER_ID('%s') IS NULL
			 CREATE LOGIN [%s] WITH PASSWORD = '%s', CHECK_POLICY = OFF`,
			mssqlTestAdminLogin, mssqlTestAdminLogin, mssqlTestAdminPassword)},
		{"create user", fmt.Sprintf(
			`IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'%s')
			 CREATE USER [%s] FOR LOGIN [%s]`,
			mssqlTestAdminLogin, mssqlTestAdminLogin, mssqlTestAdminLogin)},
		// Teardown needs to read sys.tables as well as delete, and metadata
		// visibility follows object permissions.
		{"grant DML", fmt.Sprintf(
			`GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::dbo TO [%s]`, mssqlTestAdminLogin)},
		{"grant DML on admin schema", fmt.Sprintf(
			`GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::admin TO [%s]`, mssqlTestAdminLogin)},
		{"grant VIEW DEFINITION", fmt.Sprintf(
			`GRANT VIEW DEFINITION ON SCHEMA::dbo TO [%s]`, mssqlTestAdminLogin)},
		{"add to cleat_admin", fmt.Sprintf(
			`ALTER ROLE cleat_admin ADD MEMBER [%s]`, mssqlTestAdminLogin)},
	}
	for _, s := range steps {
		if _, err := db.Exec(s.sql); err != nil {
			// A concurrent package may have won the race between the guard and
			// the statement.
			if isMSSQLAlreadyExists(err) {
				continue
			}
			t.Fatalf("provision the administrative principal (%s): %v", s.what, err)
		}
	}
}

func isMSSQLAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "is already a member") ||
		strings.Contains(msg, "already a member of the role")
}

// applyMSSQLCrossTenantOptIn applies migrations/mssql/optional/cross_tenant_claim.sql.
//
// That file is deliberately outside the auto-applied set (cleat#1541): the
// disjunction it installs costs the index seek on any query that does not carry
// its own tenant predicate, so a deployment opts in only if it wants
// --claim-across-tenants. The TEST SUITE wants it for a different reason --
// CleanupMSSQLTestData deletes across tenants by design -- and that is exactly
// the shape the opt-in exists to serve.
//
// Idempotent, and applied once per database because MSSQLAdminDB caches its
// pool per DSN and calls this before provisioning.
func applyMSSQLCrossTenantOptIn(t *testing.T, db *sql.DB) {
	t.Helper()
	path := filepath.Join(repoRootForMSSQLTestutil(t), "migrations", "mssql", "optional", "cross_tenant_claim.sql")
	execMSSQLBatchFile(t, db, path)
}

// execMSSQLBatchFile runs a .sql file's GO-separated batches over db, in
// order. Shared by applyMSSQLCrossTenantOptIn and restoreMSSQLPlainPredicate,
// which switch dbo.fn_tenant_filter between its two forms.
//
// Uses migration.SplitMSSQL -- the same splitter migration.Runner applies to
// every shipped migration -- rather than a bare strings.Split(raw, "\nGO\n").
// That simpler form is what this function used until cleat#2125: it requires
// an exact "\nGO\n" and 075_the_admin_bypass_is_opt_in.sql's GO lines carry no
// guarantee of that exact spacing, so a batch boundary was missed and the
// #cleat_bound_policies temp table 075 creates in one batch and reads in a
// later one came apart into two separate batches sent as one -- "Invalid
// object name '#cleat_bound_policies'", the temp table having gone out of
// scope with the batch that never actually ended where this function thought
// it did. migration.SplitMSSQL matches GO case-insensitively and tolerates
// trailing whitespace, which is what the real migration Runner has always
// required this exact file to survive.
func execMSSQLBatchFile(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// A single *sql.Tx, not db.Exec in a loop -- #cleat_bound_policies (this
	// file's own #temp table, and cross_tenant_claim.sql's) is SESSION-scoped,
	// and database/sql's pool does not guarantee two Exec calls on the plain
	// *sql.DB land on the same underlying connection. migration.Runner pins
	// one connection per file for exactly this reason (see its own comment on
	// applyMigration, and 075's header: "The captured policy set survives the
	// batch boundary because it is a #temp table: those live for the session,
	// and migration.Runner.applyMigration runs every batch of a file on one
	// connection inside one transaction"). This function used to loop
	// db.Exec() directly and got away with it only because an otherwise-idle
	// pool tends to hand back its one most-recently-released connection --
	// not a guarantee, and it broke the first time this helper ran 075 from a
	// pool that already had more than one connection open (cleat#2125): batch
	// 1 created and populated the temp table on one connection, a later batch
	// reading it landed on another, and SQL Server reported "Invalid object
	// name '#cleat_bound_policies'" for a table that very much existed --
	// just not on the connection asking.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin transaction for %s: %v", path, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	for _, batch := range migration.SplitMSSQL(string(raw)) {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := tx.Exec(batch); err != nil {
			t.Fatalf("applying %s: %v", path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit %s: %v", path, err)
	}
}

// repoRootForMSSQLTestutil finds the repository root from this package, so the
// migration path does not depend on which package's test binary is running.
func repoRootForMSSQLTestutil(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
