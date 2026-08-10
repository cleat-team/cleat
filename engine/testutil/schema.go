package testutil

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

// applyPostgresSchemaFile applies the real, shipped PostgreSQL migrations
// (migrations/postgres/*.sql) to db via applyMigrations/migration.Runner --
// the exact code path cmd/cleat-worker/main.go runs at boot.
//
// Named for what it used to be: a hand-maintained list of files read and
// exec'd directly, with its own advisory lock and content-fingerprint cache
// to make repeated calls within a test binary cheap. Both of those are now
// migration.Runner's job -- it takes its own advisory lock per call
// (migration/runner.go's Runner.session) and its own schema_migrations table
// makes a second Run against an already-migrated database cheap without a
// fingerprint of file contents. Kept as a thin wrapper, rather than inlining
// applyMigrations at each of this function's two call sites (SetupMinimalSchema
// and the concurrency test below), so neither has to change.
//
// Must be called with a connection that owns (or can create) the schema --
// migrations/postgres/001_schema.sql creates the `admin` and `cleat`
// schemas, several admin.* functions, and enables/forces Row-Level Security
// on every tenant-scoped table. For tests that need RLS to actually be
// enforced against their queries (rather than silently bypassed, which
// PostgreSQL does unconditionally for superuser connections and, absent
// FORCE ROW LEVEL SECURITY, for the owning role too) see
// SetupPostgresRLSRole and OpenPostgresRLSTestDB below.
func applyPostgresSchemaFile(t *testing.T, db *sql.DB) {
	t.Helper()
	applyMigrations(t, db, DialectPostgres)
}

// SetupMinimalSchema builds the test schema for one dialect.
//
// There is exactly one schema definition per dialect and this is how you reach
// it: the real migration file for PostgreSQL, SetupMySQLFullSchema for MySQL,
// SetupMSSQLFullSchema for SQL Server. "Minimal" is now a historical name --
// every dialect gets its full schema, because the minimal/full split is what
// made the duplication possible.
//
// It used to hold its own hand-written MySQL and SQL Server DDL, so each of
// those dialects had *two* independent definitions in this package plus a third
// in migrations/. All of them use CREATE TABLE IF NOT EXISTS against one shared
// test database, so whichever test ran first decided the schema for the whole
// package and Go's ordering decided which test that was. Four consecutive full
// runs against live MySQL and SQL Server produced four different failure sets,
// and two of the failing tests passed in isolation against the same database
// moments before and after failing in the suite. IMPROVEMENT-PLAN 2.60b.
//
// The three columns where the copies had actually drifted were real defects and
// are fixed; collapsing to one definition is what stops the next three.
func SetupMinimalSchema(t *testing.T, db *sql.DB, dialect Dialect) {
	t.Helper()

	switch dialect {
	case DialectPostgres:
		applyPostgresSchemaFile(t, db)
	case DialectMySQL:
		SetupMySQLFullSchema(t, db)
	case DialectMSSQL:
		SetupMSSQLFullSchema(t, db)
	default:
		t.Fatalf("setup minimal schema: unknown dialect: %s", dialect)
	}
}

// SetupFullSchema is SetupMinimalSchema. It is kept because roughly forty call
// sites use it and the distinction it used to draw -- a subset of tables, then
// the rest -- is exactly the seam the two dialects duplicated themselves
// across. Every dialect now gets one complete schema either way.
func SetupFullSchema(t *testing.T, db *sql.DB, dialect Dialect) {
	t.Helper()
	SetupMinimalSchema(t, db, dialect)
}

// CleanupPostgresTestData deletes all rows from the cleat test tables.
// Call before and after tests to ensure isolation from parallel tests.
func CleanupPostgresTestData(t *testing.T, db *sql.DB) {
	t.Helper()
	tables := []string{
		"workflow_update_requests",
		"workflow_promises",
		"workflow_signals",
		"concurrency_keys",
		"idempotency_keys",
		"event_history",
		"workflow_memory_samples",
		"workflow_memory_stats",
		"workflow_schedules",
		"workflow_instances",
		"workflow_defs",
	}
	for _, table := range tables {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			t.Logf("cleanup: delete from %s: %v", table, err)
		}
	}
}

// CleanupTestData deletes test data matching the given runID pattern
// (e.g., "test-%") from event_history, workflow_signals, workflow_promises,
// concurrency_keys, idempotency_keys, workflow_update_requests, and workflow_instances.
func CleanupTestData(t *testing.T, db *sql.DB, dialect Dialect, runID string) {
	t.Helper()
	var p string
	switch dialect {
	case DialectPostgres:
		p = "$1"
	case DialectMySQL:
		p = "?"
	case DialectMSSQL:
		p = "@p1"
	default:
		t.Fatalf("cleanup test data: unknown dialect: %s", dialect)
	}
	_, _ = db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM workflow_signals WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM workflow_promises WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM concurrency_keys WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM idempotency_keys WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM workflow_update_requests WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id LIKE `+p, runID)
}

// TestDB opens a database connection for the given dialect using environment
// variables:
//
//	DialectPostgres — CLEAT_TEST_POSTGRES (fallback CLEAT_TEST_DB, then
//	                  postgres://localhost:5432/cleat?sslmode=disable)
//	DialectMySQL    — CLEAT_TEST_MYSQL (skipped if not set)
//	DialectMSSQL    — CLEAT_TEST_MSSQL (skipped if not set)
//
// It creates the minimal schema and returns the connection. The test is
// skipped in short mode or if no database is available.
func TestDB(t *testing.T, dialect Dialect) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping database test in short mode")
	}

	var dsn string
	var driverName string
	// configured records whether a DSN for *this* dialect was supplied, as
	// opposed to falling back to a built-in default. It must be per-dialect:
	// the Multi-DB CI MySQL job sets CLEAT_TEST_MYSQL and has no PostgreSQL at
	// all, so treating any DSN variable as "a database was requested" would
	// fail every PostgreSQL subtest there for the right reason in the wrong
	// job.
	var configured bool
	switch dialect {
	case DialectPostgres:
		dsn = PostgresTestDSN()
		driverName = "postgres"
		configured = os.Getenv("CLEAT_TEST_POSTGRES") != "" || os.Getenv("CLEAT_TEST_DB") != ""
	case DialectMySQL:
		dsn = os.Getenv("CLEAT_TEST_MYSQL")
		if dsn == "" {
			t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
		}
		driverName = "mysql"
		configured = true
	case DialectMSSQL:
		dsn = os.Getenv("CLEAT_TEST_MSSQL")
		if dsn == "" {
			t.Skip("CLEAT_TEST_MSSQL not set, skipping MSSQL tests")
		}
		driverName = "sqlserver"
		configured = true
	default:
		t.Fatalf("TestDB: unknown dialect: %s", dialect)
	}

	// An unreachable database is only a reason to skip when nobody asked for
	// one. If a DSN was configured explicitly, being unable to connect to it
	// is a failure of the configuration, and skipping hides it: the Multi-DB
	// CI workflow set CLEAT_TEST_POSTGRES for a service container it had not
	// published a port for, so every PostgreSQL subtest skipped itself and the
	// job reported green for its whole existence without connecting once. The
	// same treatment is applied in cmd/cleat-worker/auth_test.go.
	unavailable := t.Skipf
	reason := "no %s database at %s (default DSN, none configured): %v"
	if configured {
		unavailable = t.Fatalf
		reason = "configured %s database at %s is unreachable: %v"
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		unavailable(reason, dialect, redactDSN(dsn), err)
		return nil
	}
	if err := db.Ping(); err != nil {
		unavailable(reason, dialect, redactDSN(dsn), err)
		return nil
	}
	SetupMinimalSchema(t, db, dialect)
	return db
}

// redactDSN strips the password from a DSN so it can appear in test output.
func redactDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		u.User = url.User(u.User.Username())
		return u.String()
	}
	// Not a URL (the MySQL driver uses its own format); drop anything that
	// looks like credentials before an @.
	if i := strings.LastIndex(dsn, "@"); i >= 0 {
		return "***@" + dsn[i+1:]
	}
	return dsn
}

// PostgresTestDSN resolves the PostgreSQL test DSN the same way
// TestDB(t, DialectPostgres) does: CLEAT_TEST_POSTGRES, falling back to
// CLEAT_TEST_DB, falling back to a hardcoded localhost DSN. Exported so
// callers that need a second, differently-privileged connection to the same
// test database (see OpenPostgresRLSTestDB) can derive it without
// duplicating the env var precedence.
func PostgresTestDSN() string {
	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}
	if dsn == "" {
		dsn = "postgres://localhost:5432/cleat?sslmode=disable"
	}
	return dsn
}

// PostgresRLSTestRole is a fixed, low-privilege PostgreSQL role used by
// tests that must exercise real Row-Level Security enforcement rather than
// merely configure it.
//
// PostgreSQL unconditionally bypasses RLS for superuser connections, and
// bypasses it for the owning role of a table unless that table has FORCE
// ROW LEVEL SECURITY set (migrations/postgres/001_schema.sql sets FORCE on
// all seven tenant-scoped tables, but that only closes the owner gap, not
// the superuser one). CLEAT_TEST_DB / CLEAT_TEST_POSTGRES conventionally
// point at a superuser role -- e.g. the default "postgres" role, or the
// POSTGRES_USER bootstrap role in the official postgres Docker image, which
// is also a superuser -- that then creates and therefore owns every table
// in SetupMinimalSchema/SetupFullSchema. A connection using that role would
// see RLS as a no-op regardless of how the policies are written, proving
// nothing about tenant isolation. Any role that is neither a superuser nor
// the table owner is always subject to RLS in Postgres (FORCE or not), so
// SetupPostgresRLSRole provisions exactly such a role, and
// OpenPostgresRLSTestDB opens a connection as it.
const PostgresRLSTestRole = "cleat_rls_test_role"

// postgresRLSTestPassword is the fixed login password for
// PostgresRLSTestRole. This role only ever exists inside ephemeral test
// databases (CLEAT_TEST_DB/CLEAT_TEST_POSTGRES), never a real deployment,
// so a hardcoded password is fine.
const postgresRLSTestPassword = "cleat-rls-test-role-password"

// SetupPostgresRLSRole ensures PostgresRLSTestRole exists and can perform
// ordinary DML (SELECT/INSERT/UPDATE/DELETE) against every table in the
// public schema, plus EXECUTE on cleat.assert_tenant_set(), without owning
// any of it. Must be called with a superuser/owner connection (e.g. the one
// TestDB(t, DialectPostgres) returns) after the schema has been applied, so
// that GRANT ... ON ALL TABLES IN SCHEMA public sees every table.
//
// It deliberately does not grant anything beyond DML: the whole point of
// this role is to be an ordinary, non-owning application role whose queries
// are actually subject to RLS.
func SetupPostgresRLSRole(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + PostgresRLSTestRole + `') THEN
				CREATE ROLE ` + PostgresRLSTestRole + ` LOGIN PASSWORD '` + postgresRLSTestPassword + `' NOSUPERUSER NOCREATEDB NOCREATEROLE;
			END IF;
		END $$;`,
		`GRANT USAGE ON SCHEMA public TO ` + PostgresRLSTestRole,
		`GRANT USAGE ON SCHEMA cleat TO ` + PostgresRLSTestRole,
		`GRANT EXECUTE ON FUNCTION cleat.assert_tenant_set() TO ` + PostgresRLSTestRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + PostgresRLSTestRole,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + PostgresRLSTestRole,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup postgres RLS test role: %v\nstatement: %s", err, stmt)
		}
	}
}

// PostgresRLSDSN derives a DSN for PostgresRLSTestRole from a superuser/
// owner DSN (as returned by PostgresTestDSN), preserving host, port,
// database, and query parameters and replacing only the user info.
func PostgresRLSDSN(superuserDSN string) (string, error) {
	u, err := url.Parse(superuserDSN)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	u.User = url.UserPassword(PostgresRLSTestRole, postgresRLSTestPassword)
	return u.String(), nil
}

// OpenPostgresRLSTestDB provisions PostgresRLSTestRole via superuserDB (see
// SetupPostgresRLSRole), then opens and returns a *separate* connection
// authenticated as that role. Tests that need genuine RLS enforcement --
// rather than the superuser/owner bypass that superuserDB itself is subject
// to -- must build their WorkflowStore (or issue their raw SQL) against the
// returned *sql.DB, not against superuserDB. superuserDB should still be
// used for schema setup and any privileged cleanup.
func OpenPostgresRLSTestDB(t *testing.T, superuserDB *sql.DB) *sql.DB {
	t.Helper()
	SetupPostgresRLSRole(t, superuserDB)
	dsn, err := PostgresRLSDSN(PostgresTestDSN())
	if err != nil {
		t.Fatalf("derive RLS test role DSN: %v", err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open RLS test role connection: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping RLS test role connection: %v", err)
	}
	return db
}
