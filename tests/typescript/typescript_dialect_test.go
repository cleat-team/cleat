package typescript

// The database a TypeScript worker test runs against, selected by CLEAT_TS_DIALECT (cleat#3988).
//
// These tests were PostgreSQL by construction: a Postgres scratch database, a cleat_app role with a password, and
// `--driver=postgres` hardcoded in every worker and cleatctl call. So setting CLEAT_TEST_MYSQL or CLEAT_TEST_MSSQL on the job
// would have run nothing, and a green job would have looked like multi-dialect coverage. This file is the other half: what the
// tests need from a database, per dialect.
//
// ONE DIALECT PER RUN, chosen by CLEAT_TS_DIALECT (postgres, mysql or mssql; unset means postgres). It is not a subtest loop,
// on purpose: the test names and counts under the default are exactly what they were before this file existed, so the
// PostgreSQL result is comparable run to run, and a CI matrix leg is one dialect with one DSN. An unknown value is a failure,
// never a fallback to postgres: a typo that quietly ran the wrong database is the green-that-measured-nothing this exists to
// prevent.
//
// The recipes are the ones cmd/cleat-worker's TestAWorkerBootsAndServesAsTheAppLoginOnEveryDialect already proves for all three
// dialects; they are package main and cannot be imported, so they are adapted here, and the worker is served as the app login
// on every dialect, exactly as it is on PostgreSQL.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"
)

const dialectEnv = "CLEAT_TS_DIALECT"

// dialect is one database server the worker tests can run against.
type dialect struct {
	name   string // the worker's and cleatctl's --driver
	env    string // the variable holding the admin DSN
	driver string // the database/sql driver name
}

// activeDialect is the dialect this run was asked for. Unknown values fail.
func activeDialect(t *testing.T) dialect {
	t.Helper()
	switch v := os.Getenv(dialectEnv); v {
	case "", "postgres":
		return dialect{"postgres", postgresEnv, "postgres"}
	case "mysql":
		return dialect{"mysql", "CLEAT_TEST_MYSQL", "mysql"}
	case "mssql":
		return dialect{"mssql", "CLEAT_TEST_MSSQL", "sqlserver"}
	default:
		t.Fatalf("%s=%q is not a dialect: want postgres, mysql or mssql (or unset, which means postgres)", dialectEnv, v)
		return dialect{}
	}
}

// admin returns the admin DSN, or "". PostgreSQL also reads CLEAT_TEST_DB, as every other database-backed test does.
func (d dialect) admin() string {
	if v := os.Getenv(d.env); v != "" {
		return v
	}
	if d.name == "postgres" {
		return os.Getenv(postgresEnvFall)
	}
	return ""
}

// needDatabase returns the admin DSN of the active dialect. Unset: a SKIP, unless CLEAT_REQUIRE_TOOLCHAINS names the dialect,
// in which case it is a failure. The name there means the DSN, and the job that provides the service declares it, so a service
// that stopped starting cannot turn these tests into skips.
func needDatabase(t *testing.T) string {
	t.Helper()
	d := activeDialect(t)
	dsn := d.admin()
	if dsn == "" {
		if toolchainRequired(d.name) {
			t.Fatalf("%s names %s, but %s is not set: the job's database service is broken, not the test",
				requireToolchainEnv, d.name, d.env)
		}
		t.Skip(d.env + " is not set; only the TypeScript tier-2 job provides a database for these tests")
	}
	return dsn
}

// randomTestPassword is safe to splice into a MySQL single-quoted string and a T-SQL one with no escaping (hex, plus a suffix
// that satisfies SQL Server's default password complexity policy).
func randomTestPassword(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generating a random test password: %v", err)
	}
	return hex.EncodeToString(buf) + "Aa1!"
}

// scratchName is the name every dialect's scratch database gets.
func scratchName() string {
	return fmt.Sprintf("cleat_ts_worker_%d", time.Now().UnixNano()%1_000_000_000)
}

// scratchMySQL creates an empty database on the admin server and returns its owner DSN, dropping it when the test ends.
func scratchMySQL(t *testing.T, d dialect, admin string) string {
	t.Helper()
	adb, err := sql.Open(d.driver, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adb.Close() })
	if err := adb.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", d.env, err)
	}
	cfg, err := mysql.ParseDSN(admin)
	if err != nil {
		t.Fatal(err)
	}
	name := scratchName()
	if _, err := adb.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open(d.driver, admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec("DROP DATABASE IF EXISTS `" + name + "`")
	})
	cfg.DBName = name
	return cfg.FormatDSN()
}

// scratchMSSQL is scratchMySQL for SQL Server, which needs the database taken out of use before it can be dropped.
func scratchMSSQL(t *testing.T, d dialect, admin string) string {
	t.Helper()
	adb, err := sql.Open(d.driver, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adb.Close() })
	if err := adb.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", d.env, err)
	}
	name := scratchName()
	if _, err := adb.Exec("CREATE DATABASE [" + name + "]"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open(d.driver, admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(fmt.Sprintf("IF DB_ID('%[1]s') IS NOT NULL BEGIN ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%[1]s] END", name))
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	return u.String()
}

// mysqlAppRoleDSN creates cleat_app with a password, grants it what the schema needs (queried from information_schema, not a
// fixed list, because plugin migrations add tables), and returns its DSN. It also grants the per-tenant-database pattern,
// without which the worker cannot create its own tenant database at boot: measured by cmd/cleat-worker's boot test, which is why
// that test passes the same grant here. cleat_app is a SERVER-GLOBAL principal, so it is dropped when the test ends: left in
// place its pattern grant would reach every other test's scratch database.
func mysqlAppRoleDSN(t *testing.T, d dialect, ownerDSN string) string {
	t.Helper()
	owner, err := sql.Open(d.driver, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	cfg, err := mysql.ParseDSN(ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	pw := randomTestPassword(t)
	if _, err := owner.Exec(fmt.Sprintf(`CREATE USER IF NOT EXISTS 'cleat_app'@'%%' IDENTIFIED BY '%s'`, pw)); err != nil {
		t.Fatalf("creating cleat_app: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DROP USER IF EXISTS 'cleat_app'@'%'`) })
	if _, err := owner.Exec(fmt.Sprintf(`ALTER USER 'cleat_app'@'%%' IDENTIFIED BY '%s'`, pw)); err != nil {
		t.Fatalf("setting cleat_app's password: %v", err)
	}
	tables := queryStrings(t, owner, `SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE'`, cfg.DBName)
	if len(tables) == 0 {
		t.Fatalf("%s has no tables -- has the migrate step run?", cfg.DBName)
	}
	for _, tbl := range tables {
		if _, err := owner.Exec(fmt.Sprintf("GRANT SELECT ON `%s` TO 'cleat_app'@'%%'", tbl)); err != nil {
			t.Fatalf("granting SELECT on %s: %v", tbl, err)
		}
		if tbl == "deployment_secrets" {
			continue // the app login may read these, never write them
		}
		if _, err := owner.Exec(fmt.Sprintf("GRANT INSERT, UPDATE, DELETE ON `%s` TO 'cleat_app'@'%%'", tbl)); err != nil {
			t.Fatalf("granting write on %s: %v", tbl, err)
		}
	}
	for _, r := range queryStrings(t, owner, `SELECT routine_name FROM information_schema.routines WHERE routine_schema = ?`, cfg.DBName) {
		if _, err := owner.Exec(fmt.Sprintf("GRANT EXECUTE ON PROCEDURE `%s` TO 'cleat_app'@'%%'", r)); err != nil {
			t.Fatalf("granting EXECUTE on %s: %v", r, err)
		}
	}
	if _, err := owner.Exec("GRANT ALL PRIVILEGES ON `cleat\\_%`.* TO 'cleat_app'@'%'"); err != nil {
		t.Fatalf("granting cleat_app rights on the tenant-database pattern: %v", err)
	}
	cfg.User, cfg.Passwd = "cleat_app", pw
	return cfg.FormatDSN()
}

// mssqlAppRoleDSN creates the cleat_app LOGIN with a password, maps it into this database and adds it to cleat_app_role, the
// database role migrations/mssql/001_schema.sql already created to carry the GRANT/DENY. The login is server-level, so it is
// dropped when the test ends.
func mssqlAppRoleDSN(t *testing.T, d dialect, ownerDSN string) string {
	t.Helper()
	owner, err := sql.Open(d.driver, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	pw := randomTestPassword(t)
	if _, err := owner.Exec(`IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'cleat_app')
		EXEC('CREATE LOGIN cleat_app WITH PASSWORD = ''` + pw + `''')`); err != nil {
		t.Fatalf("creating cleat_app login: %v", err)
	}
	if _, err := owner.Exec(`ALTER LOGIN cleat_app WITH PASSWORD = '` + pw + `'`); err != nil {
		t.Fatalf("setting cleat_app's password: %v", err)
	}
	if _, err := owner.Exec(`ALTER LOGIN cleat_app ENABLE`); err != nil {
		t.Fatalf("enabling cleat_app: %v", err)
	}
	if _, err := owner.Exec(`IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_app')
		CREATE USER cleat_app FOR LOGIN cleat_app`); err != nil {
		t.Fatalf("mapping cleat_app into this database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(`DROP USER IF EXISTS cleat_app`)
		_, _ = owner.Exec(`DROP LOGIN cleat_app`)
	})
	var roleExists int
	if err := owner.QueryRow(`SELECT COUNT(*) FROM sys.database_principals WHERE name = N'cleat_app_role' AND type = 'R'`).Scan(&roleExists); err != nil {
		t.Fatal(err)
	}
	if roleExists == 0 {
		t.Fatal("cleat_app_role does not exist -- has migrations/mssql/001_schema.sql applied?")
	}
	if _, err := owner.Exec(`ALTER ROLE cleat_app_role ADD MEMBER cleat_app`); err != nil {
		t.Fatalf("adding cleat_app to cleat_app_role: %v", err)
	}
	u, err := url.Parse(ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("cleat_app", pw)
	return u.String()
}

// queryStrings runs a one-column query and returns every row.
func queryStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// historyRows counts a run's persisted history rows, as the owner. Two things differ by dialect: the placeholder, and WHERE
// the table is. On MySQL a tenant's per-tenant tables live in a database of their own (engine.MySQLTenantDatabaseName), not in
// the scratch database the worker was pointed at, so reading the scratch database's event_history answers a different question
// and reports an empty table, which the caller's own "no persisted history" guard correctly refuses to treat as a pass.
//
// That per-tenant database is SERVER-GLOBAL and is not dropped when the test ends (dropping the default tenant's database on a
// shared server would destroy other suites' data). CI gives the job a fresh service container, so nothing carries over there; a
// reused local server keeps the previous run's instances, which the next worker may reclaim. Use a throwaway server.
func historyRows(t *testing.T, ownerDSN, workflowID string) int {
	t.Helper()
	d := activeDialect(t)
	db, err := sql.Open(d.driver, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	table, ph := "event_history", "$1"
	switch d.name {
	case "mysql":
		table, ph = "`"+engine.MySQLTenantDatabaseName(defaultTenant)+"`.event_history", "?"
	case "mssql":
		ph = "@p1"
	}
	// SQL Server isolates tenants with row-level security keyed on a SESSION value, and an owner connection that has not set it
	// sees NO rows: the same empty answer a run with no history gives. So the value is set on the very connection that counts
	// (a pool would hand the count a different one), the way the engine's own store does it (engine/mssql_store.go).
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if d.name == "mssql" {
		if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, defaultTenant); err != nil {
			t.Fatalf("setting the tenant context to read the run's history: %v", err)
		}
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE workflow_id = `+ph, workflowID).Scan(&n); err != nil {
		t.Fatalf("reading the run's history: %v", err)
	}
	return n
}
