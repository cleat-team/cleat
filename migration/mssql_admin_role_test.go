package migration

// IMPROVEMENT-PLAN 2.71 / administrative access under Row-Level Security.
//
// SQL Server applies a security policy to every principal -- sysadmin, db_owner
// and dbo included -- so before migration 012 there was no connection that
// could read or delete across tenants. PostgreSQL gets that for free, because a
// superuser bypasses RLS unconditionally; migrations/postgres/005_app_role.sql
// is entirely about keeping the *application* out of that exemption. SQL Server
// has to write the privileged half into the predicate, because the predicate is
// the only place an exemption can live.
//
// These tests assert both directions, and the negative direction is the more
// important one: the exemption must be something a principal has to be given,
// not something it can assume. In particular sa must stay filtered, because
// "the application connects as sa" is a real deployment and the exemption
// silently disabling tenant isolation there would be worse than the gap it
// closes.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

const (
	adminRoleTestDB = "cleat_migration_admin_role_test"
	tenantA         = "11111111-1111-1111-1111-111111111111"
	tenantB         = "22222222-2222-2222-2222-222222222222"
	// Distinct passwords are not interesting; CHECK_POLICY = OFF is, because
	// the container's password policy would otherwise reject a literal here.
	testLoginPassword = "CleatAdminRoleProbe123!"
)

// mssqlDSNAs returns the configured SQL Server DSN pointed at a database and
// rewritten to authenticate as a different login.
func mssqlDSNAs(t *testing.T, dbName, user, password string) string {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server migration test")
	}
	swapped, err := swapMSSQLDB(dsn, dbName)
	if err != nil {
		t.Fatalf("derive SQL Server DSN: %v", err)
	}
	u, err := url.Parse(swapped)
	if err != nil {
		t.Fatalf("parse SQL Server DSN: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// countVisibleDefs opens a fresh connection as the named login, optionally sets
// a tenant session context on it, and counts the rows that connection can see.
//
// One pinned connection for the whole sequence: SESSION_CONTEXT lives on the
// connection, and database/sql recycling a pooled one sends sp_reset_connection
// and clears it. That is why the real store applies it in the connector, at
// open (engine/mssql_store.go), and why a test that used the pool directly
// would report zero rows for reasons unrelated to the policy.
func countVisibleDefs(t *testing.T, user, password, tenant string) (int, int) {
	t.Helper()
	db, err := sql.Open("sqlserver", mssqlDSNAs(t, adminRoleTestDB, user, password))
	if err != nil {
		t.Fatalf("open as %s: %v", user, err)
	}
	defer db.Close()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection as %s: %v", user, err)
	}
	defer conn.Close()

	if tenant != "" {
		if _, err := conn.ExecContext(ctx,
			`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, tenant); err != nil {
			t.Fatalf("set session context as %s: %v", user, err)
		}
	}
	var isAdmin sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT IS_ROLEMEMBER(N'cleat_admin')`).Scan(&isAdmin); err != nil {
		t.Fatalf("read role membership as %s: %v", user, err)
	}
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dbo.workflow_defs`).Scan(&n); err != nil {
		t.Fatalf("count workflow_defs as %s: %v", user, err)
	}
	return n, int(isAdmin.Int64)
}

// setupAdminRoleDB migrates a scratch database, seeds two tenants, and creates
// two logins: one placed in cleat_admin and one left out of it.
func setupAdminRoleDB(t *testing.T) *sql.DB {
	t.Helper()
	db := newMSSQLScratchDB(t, adminRoleTestDB)
	ctx := context.Background()

	if err := NewRunner(db, engine.DialectMSSQL, migrationsRoot(t)).Run(ctx); err != nil {
		t.Fatalf("apply the shipped SQL Server migrations: %v", err)
	}

	// Seeded with no session context at all, which works because 001 declares
	// FILTER predicates and a FILTER predicate constrains reads, not writes.
	// (A BLOCK predicate would refuse these inserts. There are none.)
	for i, tenant := range []string{tenantA, tenantA, tenantB} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, task_queue, tenant_id)
			VALUES (@p1, 1, 0x00, '[]', 'default', @p2)`,
			fmt.Sprintf("admin-role-def-%d", i), tenant); err != nil {
			t.Fatalf("seed workflow_defs: %v", err)
		}
	}

	masterDSN := mssqlDSNAsSA(t, "master")
	master, err := sql.Open("sqlserver", masterDSN)
	if err != nil {
		t.Fatalf("open master: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	logins := []struct {
		name    string
		isAdmin bool
	}{
		{"cleat_admin_probe", true},
		{"cleat_plain_probe", false},
	}
	for _, l := range logins {
		l := l
		// Server-level objects on a shared instance: drop first in case a
		// previous run died before its cleanup, and drop again after.
		dropLogin := fmt.Sprintf(
			`IF SUSER_ID('%s') IS NOT NULL DROP LOGIN [%s]`, l.name, l.name)
		if _, err := master.ExecContext(ctx, dropLogin); err != nil {
			t.Fatalf("drop stale login %s: %v", l.name, err)
		}
		if _, err := master.ExecContext(ctx, fmt.Sprintf(
			`CREATE LOGIN [%s] WITH PASSWORD = '%s', CHECK_POLICY = OFF`,
			l.name, testLoginPassword)); err != nil {
			t.Fatalf("create login %s: %v", l.name, err)
		}
		t.Cleanup(func() {
			cleanupDB, err := sql.Open("sqlserver", masterDSN)
			if err != nil {
				return
			}
			defer cleanupDB.Close()
			_, _ = cleanupDB.Exec(dropLogin)
		})

		for _, q := range []string{
			fmt.Sprintf(`CREATE USER [%s] FOR LOGIN [%s]`, l.name, l.name),
			fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::dbo TO [%s]`, l.name),
		} {
			if _, err := db.ExecContext(ctx, q); err != nil {
				t.Fatalf("%.40s: %v", q, err)
			}
		}
		if l.isAdmin {
			if _, err := db.ExecContext(ctx, fmt.Sprintf(
				`ALTER ROLE cleat_admin ADD MEMBER [%s]`, l.name)); err != nil {
				t.Fatalf("add %s to cleat_admin: %v", l.name, err)
			}
		}
	}
	return db
}

func mssqlDSNAsSA(t *testing.T, dbName string) string {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server migration test")
	}
	swapped, err := swapMSSQLDB(dsn, dbName)
	if err != nil {
		t.Fatalf("derive SQL Server DSN: %v", err)
	}
	return swapped
}

// TestMSSQLAdminRoleShipsEmpty is the safety property. Applying the migration
// must change nothing for any principal that existed before it, because the
// role it introduces has no members.
func TestMSSQLAdminRoleShipsEmpty(t *testing.T) {
	db := setupAdminRoleDB(t)
	ctx := context.Background()

	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sys.database_principals WHERE name = N'cleat_admin' AND type = 'R'`).
		Scan(&exists); err != nil {
		t.Fatalf("look up cleat_admin: %v", err)
	}
	if exists != 1 {
		t.Fatalf("migration 012 did not create the cleat_admin role (found %d)", exists)
	}

	// Only the two logins this test added itself, one of which it put in the
	// role deliberately. The migration must not grant it to anyone.
	rows, err := db.QueryContext(ctx, `
		SELECT m.name
		FROM sys.database_role_members rm
		JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
		JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
		WHERE r.name = N'cleat_admin'`)
	if err != nil {
		t.Fatalf("list cleat_admin members: %v", err)
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan member: %v", err)
		}
		members = append(members, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate members: %v", err)
	}
	if len(members) != 1 || members[0] != "cleat_admin_probe" {
		t.Errorf("cleat_admin members are %v, want only the one this test added; "+
			"the migration must ship the role empty so that applying it changes "+
			"nothing until a deployment grants it", members)
	}
}

// TestMSSQLAdminRoleIsNotConferredByDBOwner is the objection that would have
// sunk this design: if db_owner or sa were exempt, then every deployment that
// connects as sa -- which is the documented default in
// engine/testutil.MSSQLTestDB, and common in the wild -- would lose tenant
// isolation the moment this migration applied.
func TestMSSQLAdminRoleIsNotConferredByDBOwner(t *testing.T) {
	setupAdminRoleDB(t)

	saUser, saPassword := mssqlCredentials(t)

	n, isAdmin := countVisibleDefs(t, saUser, saPassword, "")
	if isAdmin != 0 {
		t.Errorf("%s reads IS_ROLEMEMBER('cleat_admin') = %d; the exemption must be "+
			"granted explicitly, never inherited from db_owner or sysadmin", saUser, isAdmin)
	}
	if n != 0 {
		t.Errorf("%s saw %d rows with no session context, want 0 -- the filter "+
			"predicate is not applying to it, so tenant isolation is off for "+
			"every connection this deployment makes", saUser, n)
	}

	// And it still resolves tenants the ordinary way.
	if n, _ := countVisibleDefs(t, saUser, saPassword, tenantA); n != 2 {
		t.Errorf("%s with tenant A session context saw %d rows, want 2", saUser, n)
	}
	if n, _ := countVisibleDefs(t, saUser, saPassword, tenantB); n != 1 {
		t.Errorf("%s with tenant B session context saw %d rows, want 1", saUser, n)
	}
}

// TestMSSQLPlainUserStaysFiltered guards the ordinary application principal:
// a user that is not in the role sees exactly its own tenant, which is what
// the policies existed to do before this migration and must still do after.
func TestMSSQLPlainUserStaysFiltered(t *testing.T) {
	setupAdminRoleDB(t)

	if n, isAdmin := countVisibleDefs(t, "cleat_plain_probe", testLoginPassword, ""); n != 0 || isAdmin != 0 {
		t.Errorf("a non-member with no session context saw %d rows (member=%d), want 0", n, isAdmin)
	}
	if n, _ := countVisibleDefs(t, "cleat_plain_probe", testLoginPassword, tenantA); n != 2 {
		t.Errorf("a non-member scoped to tenant A saw %d rows, want 2", n)
	}
	if n, _ := countVisibleDefs(t, "cleat_plain_probe", testLoginPassword, tenantB); n != 1 {
		t.Errorf("a non-member scoped to tenant B saw %d rows, want 1", n)
	}
}

// TestMSSQLAdminRoleSeesAndDeletesAcrossTenants is the capability the migration
// exists to provide, and the one 2.71's teardown needs: with no session context
// at all, a member of the role reads every tenant's rows and DELETE removes
// them rather than silently matching nothing.
func TestMSSQLAdminRoleSeesAndDeletesAcrossTenants(t *testing.T) {
	setupAdminRoleDB(t)

	n, isAdmin := countVisibleDefs(t, "cleat_admin_probe", testLoginPassword, "")
	if isAdmin != 1 {
		t.Fatalf("the admin login reads IS_ROLEMEMBER('cleat_admin') = %d, want 1", isAdmin)
	}
	if n != 3 {
		t.Fatalf("the admin login saw %d rows across two tenants, want 3", n)
	}

	db, err := sql.Open("sqlserver",
		mssqlDSNAs(t, adminRoleTestDB, "cleat_admin_probe", testLoginPassword))
	if err != nil {
		t.Fatalf("open as the admin login: %v", err)
	}
	defer db.Close()

	res, err := db.Exec(`DELETE FROM dbo.workflow_defs`)
	if err != nil {
		t.Fatalf("DELETE as the admin login: %v", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	// The whole of 2.71's blocker in one assertion: before 012 this reported 0
	// rows affected and reported no error, so teardown believed it had run.
	if deleted != 3 {
		t.Errorf("DELETE removed %d rows, want 3 -- teardown that deletes nothing "+
			"and reports no error is what leaves rows behind to collide with the "+
			"next test's fixtures (IMPROVEMENT-PLAN 2.71)", deleted)
	}
	if n, _ := countVisibleDefs(t, "cleat_admin_probe", testLoginPassword, ""); n != 0 {
		t.Errorf("%d rows survived the cross-tenant delete", n)
	}
}

// mssqlCredentials pulls the configured login out of CLEAT_TEST_MSSQL, rather
// than assuming sa: whoever runs this suite may have pointed it at something
// else, and the assertion is about the *configured* principal not being exempt.
func mssqlCredentials(t *testing.T) (string, string) {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server migration test")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse CLEAT_TEST_MSSQL: %v", err)
	}
	if u.User == nil || u.User.Username() == "" {
		t.Skipf("CLEAT_TEST_MSSQL has no username (integrated auth?), skipping: %s",
			strings.SplitN(dsn, "@", 2)[len(strings.SplitN(dsn, "@", 2))-1])
	}
	password, _ := u.User.Password()
	return u.User.Username(), password
}
