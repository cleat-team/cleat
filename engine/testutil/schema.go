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

// postgresCleanupTables is the set of tables CleanupPostgresTestData clears.
//
// Child tables first, because of the foreign keys. Kept in the same order as
// mysqlCleanupTables and mssqlCleanupTables so the three can be diffed by eye --
// they had drifted, and TestCleanupTableListsAgree now fails if they do again.
var postgresCleanupTables = []string{
	"tenant_api_keys",
	"workflow_tags",
	"workflow_routing",
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
	"plugin_defs",
}

// CleanupPostgresTestData deletes all rows from the cleat test tables.
// Call before and after tests to ensure isolation from parallel tests.
//
// PostgreSQL only, despite having been called with MySQL and SQL Server
// handles for a long time -- see CleanupAllTestData.
func CleanupPostgresTestData(t *testing.T, db *sql.DB) {
	t.Helper()

	// Only the tables this database actually has. SetupMinimalSchema creates a
	// subset, so the full list is not present everywhere -- which is what the
	// existence check is for, and is exactly what SQL Server's cleanup has
	// always done. Without it, widening this list to match the other dialects
	// fails every minimal-schema test on `relation "tenant_api_keys" does not
	// exist`.
	present := existingTables(t, db, DialectPostgres, postgresCleanupTables)

	for _, table := range present {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("cleanup: delete from %s: %v\n\n"+
				"This used to be a t.Logf, so a cleanup that did nothing was "+
				"indistinguishable from one that worked, and the fixtures it "+
				"failed to remove surfaced later as an unrelated test failing "+
				"on a duplicate key. See IMPROVEMENT-PLAN 2.60d.", table, err)
		}
	}
	assertTablesEmpty(t, db, present, func(s string) string { return s })
}

// assertTablesEmpty proves the deletes above actually removed the rows.
//
// An error is not the only way cleanup fails, and it is not the way that has
// cost the most here. A DELETE issued on a connection whose rows are hidden
// from it removes nothing and reports no error: PostgreSQL row-level security
// filters the delete to the caller's tenant, and SQL Server applies its
// security policy to every principal including sysadmin (§3.37, where
// CleanupMSSQLTestData deleted nothing, reported success, and rows accumulated
// until a later fixture collided on a primary key -- the 141-failure signature
// in §2.71's residual).
//
// So the check is not "did the statement error" but "is the table empty now".
// One round trip for all of them, because this runs on the order of a hundred
// times per suite.
//
// quote adapts the identifier to the dialect; the caller supplies it because
// SQL Server needs bracketed, schema-qualified names and the other two do not.
func assertTablesEmpty(t *testing.T, db *sql.DB, tables []string, quote func(string) string) {
	t.Helper()
	leftover, err := nonEmptyTables(db, tables, quote)
	if err != nil {
		t.Fatalf("cleanup: verifying tables are empty: %v", err)
	}
	if len(leftover) > 0 {
		t.Fatalf("cleanup deleted nothing from %s, and reported no error.\n\n"+
			"A DELETE that removes no rows without failing means the rows are "+
			"not visible to this connection -- a row-level security policy or "+
			"security predicate is filtering them. Cleanup then believes it ran, "+
			"and the rows surface later as a duplicate key in an unrelated test. "+
			"Use a connection that can see every tenant's rows. See "+
			"IMPROVEMENT-PLAN 3.37 and 2.60d.", strings.Join(leftover, ", "))
	}
}

// CleanupAllTestData deletes every row from the tables the given dialect's
// cleanup knows about, dispatching to the right one.
//
// It exists because CleanupPostgresTestData was being called with MySQL and
// SQL Server handles -- the name says PostgreSQL, the SQL it issued was
// dialect-neutral, and so the mistake was invisible until the PostgreSQL
// cleanup grew a PostgreSQL-specific query (`current_schema()`) and the MySQL
// and SQL Server runs failed with "FUNCTION cleat.current_schema does not
// exist". Prefer this in anything that loops over dialects.
func CleanupAllTestData(t *testing.T, db *sql.DB, dialect Dialect) {
	t.Helper()
	switch dialect {
	case DialectPostgres:
		CleanupPostgresTestData(t, db)
	case DialectMySQL:
		CleanupMySQLTestData(t, db)
	case DialectMSSQL:
		CleanupMSSQLTestData(t, db)
	default:
		t.Fatalf("CleanupAllTestData: unknown dialect: %s", dialect)
	}
}

// existingTables filters candidates down to the tables this database has,
// preserving the caller's order (which is foreign-key order, so it matters).
//
// SQL Server's cleanup has always done this, one table at a time against
// sys.tables. PostgreSQL and MySQL did not, which is why their lists could
// only ever contain tables present in *every* schema variant -- and why the
// PostgreSQL list had silently drifted four tables behind the other two:
// tenant_api_keys, workflow_tags, workflow_routing and plugin_defs exist in
// the migrated schema but not in SetupMinimalSchema's subset, so adding them
// without this check fails every minimal-schema test.
//
// One query, not one per table: cleanup runs on the order of a hundred times
// per suite.
func existingTables(t *testing.T, db *sql.DB, dialect Dialect, candidates []string) []string {
	t.Helper()

	have := make(map[string]bool)

	switch dialect {
	case DialectPostgres:
		// to_regclass, not information_schema.tables filtered by
		// current_schema(). The question is "would an unqualified DELETE FROM
		// <t> resolve", and that is decided by the whole search_path, not by
		// the first entry on it. Filtering on current_schema() answered a
		// different question and answered it wrongly wherever the tables live
		// somewhere else on the path: existingTables returned nothing, cleanup
		// deleted nothing, and -- because assertTablesEmpty verifies only the
		// tables it was given -- nothing noticed. That is the silent no-op
		// this whole item exists to remove, reintroduced by the check meant to
		// support it. Caught by the Cluster job, where leftover rows surfaced
		// as `CreateSchedule: duplicate key value violates unique constraint`.
		// One round trip, N columns -- not one query per table. Cleanup runs on
		// the order of a hundred times per suite, so a per-table query turned
		// ~15 statements into ~1500 and made the engine suite visibly slower.
		// The names are compile-time constants in this package, not input.
		exprs := make([]string, 0, len(candidates))
		dest := make([]any, 0, len(candidates))
		found := make([]bool, len(candidates))
		for i, c := range candidates {
			exprs = append(exprs, fmt.Sprintf("to_regclass('%s') IS NOT NULL", c))
			dest = append(dest, &found[i])
		}
		if err := db.QueryRow("SELECT " + strings.Join(exprs, ", ")).Scan(dest...); err != nil {
			t.Fatalf("cleanup: resolving table names: %v", err)
		}
		for i, c := range candidates {
			if found[i] {
				have[c] = true
			}
		}
	case DialectMySQL:
		// MySQL has no search_path: a connection has exactly one default
		// database, so this is the same question.
		rows, err := db.Query(
			`SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()`)
		if err != nil {
			t.Fatalf("cleanup: listing tables: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("cleanup: scanning table names: %v", err)
			}
			have[name] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("cleanup: reading table names: %v", err)
		}
	case DialectMSSQL:
		// Schema-aware, not name-alone. sys.tables is keyed on name, so an
		// unqualified lookup happily finds a table in another schema that an
		// unqualified DELETE would then fail to resolve -- the trap
		// CleanupMSSQLTestData records for admin.tenant_api_keys. Candidates
		// here are unqualified, so they mean dbo.
		rows, err := db.Query(`SELECT t.name FROM sys.tables t
			JOIN sys.schemas s ON t.schema_id = s.schema_id
			WHERE s.name = 'dbo'`)
		if err != nil {
			t.Fatalf("cleanup: listing tables: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("cleanup: scanning table names: %v", err)
			}
			have[name] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("cleanup: reading table names: %v", err)
		}
	default:
		t.Fatalf("existingTables: unsupported dialect %s", dialect)
	}

	present := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if have[c] {
			present = append(present, c)
		}
	}

	// None of them present means the schema is not where this connection can
	// see it, not that there is nothing to clean. Returning an empty list
	// would make cleanup a silent no-op and the emptiness check vacuous --
	// both would report success having done nothing, which is precisely the
	// failure 2.60d is about.
	if len(present) == 0 {
		t.Fatalf("cleanup: none of the %d expected tables are visible to this "+
			"connection.\n\nThe schema is not on this connection's search path. "+
			"Cleaning nothing and reporting success is how fixtures leak into "+
			"the next test; see IMPROVEMENT-PLAN 2.60d.", len(candidates))
	}
	return present
}

// nonEmptyTables returns "<table>=<count>" for every table that still has rows.
//
// Split out from assertTablesEmpty so the check itself can be tested: a helper
// whose only failure path is t.Fatalf cannot be shown to fire without failing
// the test that proves it. See TestNonEmptyTablesSeesRowsCleanupMissed.
func nonEmptyTables(db *sql.DB, tables []string, quote func(string) string) ([]string, error) {
	if len(tables) == 0 {
		return nil, nil
	}

	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		// The table names are compile-time constants in this package, not
		// input, so there is nothing here to inject.
		parts = append(parts, fmt.Sprintf(
			"SELECT '%s' AS t, COUNT(*) AS n FROM %s", table, quote(table)))
	}

	rows, err := db.Query(strings.Join(parts, " UNION ALL "))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var leftover []string
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		if n > 0 {
			leftover = append(leftover, fmt.Sprintf("%s=%d", name, n))
		}
	}
	return leftover, rows.Err()
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
	// These seven are exactly the tables a workflow ID can select rows in: six
	// carry a workflow_id column and workflow_instances carries it as `id`.
	// The other eight tables the blanket cleanups clear are keyed by name or
	// tenant (workflow_defs, plugin_defs, workflow_schedules, workflow_tags,
	// tenant_api_keys, workflow_memory_stats) or by a surrogate id unrelated
	// to any workflow (workflow_routing, workflow_memory_samples), so they
	// cannot be scoped this way at all. Measured 2026-08-31 against the
	// PostgreSQL schema; this is a complete list, not a partial one.
	deletes := []struct{ table, where string }{
		{"event_history", "workflow_id LIKE " + p},
		{"workflow_signals", "workflow_id LIKE " + p},
		{"workflow_promises", "workflow_id LIKE " + p},
		{"concurrency_keys", "workflow_id LIKE " + p},
		{"idempotency_keys", "workflow_id LIKE " + p},
		{"workflow_update_requests", "workflow_id LIKE " + p},
		{"workflow_instances", "id LIKE " + p},
	}

	names := make([]string, 0, len(deletes))
	for _, d := range deletes {
		names = append(names, d.table)
	}
	present := make(map[string]bool)
	for _, n := range existingTables(t, db, dialect, names) {
		present[n] = true
	}

	for _, d := range deletes {
		if !present[d.table] {
			continue
		}
		// Errors were discarded here with `_, _ =`, so a cleanup that failed
		// outright was indistinguishable from one that worked -- the same
		// defect IMPROVEMENT-PLAN 2.60d records for the blanket cleanups,
		// which this helper did not get when they were fixed.
		if _, err := db.Exec("DELETE FROM "+d.table+" WHERE "+d.where, runID); err != nil {
			t.Fatalf("cleanup: delete from %s where %s: %v", d.table, d.where, err)
		}
	}
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
