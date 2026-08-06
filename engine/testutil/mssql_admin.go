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
	"strings"
	"sync"
	"testing"
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
		return pool
	}

	requireMSSQLAdminRole(t, db)
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

	mssqlAdminPools[baseDSN] = pool
	return pool
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
