package testutil

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// schemaApplyLockKey is the advisory-lock key that serialises concurrent
// applications of 001_schema.sql against one database. Any fixed int64 works
// as long as every caller uses the same one; this is "cleatddl" in ASCII,
// chosen to be recognisable in pg_locks when diagnosing a stuck test.
const schemaApplyLockKey int64 = 0x636c65617464646c

// execIgnoreDupKey executes a SQL statement, ignoring MySQL error 1061
// (Duplicate key name) and 1060 (Duplicate column name). Other errors are
// passed through. This allows idempotent CREATE INDEX / ALTER TABLE ADD
// COLUMN in MySQL which does not support IF NOT EXISTS for those operations.
func execIgnoreDupKey(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "Error 1061") && !strings.Contains(msg, "Error 1060") {
			t.Fatalf("setup schema: %v", err)
		}
	}
}

// execMSSQLBestEffort executes a SQL statement, ignoring errors that are
// expected in test schemas (e.g., index creation on NVARCHAR(MAX) columns).
func execMSSQLBestEffort(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		msg := err.Error()
		// NVARCHAR(MAX) columns cannot be index keys; this is expected in
		// test schemas that use NVARCHAR(MAX) for flexibility.
		if strings.Contains(msg, "invalid for use as a key column") {
			return
		}
		t.Fatalf("setup schema: %v", err)
	}
}

// postgresSchemaFiles locates the real, shipped PostgreSQL schema migrations,
// applied directly by SetupMinimalSchema/SetupFullSchema for DialectPostgres
// instead of a hand-maintained duplicate.
//
// This file previously hand-duplicated the schema (a third copy, alongside
// migrations/postgres/001_schema.sql and the root schema.sql) and had
// already drifted from it twice in one session before this fix (the
// `generation` column's nullability, and MySQL collation). Reading the real
// migration from disk -- the same approach store_backends_procedures_test.go
// already uses for 003_procedures.sql/004_*.sql -- makes drift structurally
// impossible for Postgres.
//
// The path is computed from this source file's own location via
// runtime.Caller rather than a hardcoded relative path, because this
// package is exercised from two different `go test` working directories:
// engine/ (which imports testutil) and engine/testutil/ itself
// (testutil_test.go) -- a single ".."-relative path cannot be correct for
// both.
// It returns every migration that shapes a table, in version order, because
// the schema a test runs against is all of them and not just the first.
// 001_schema.sql is the bulk of it; 010 widens idempotency_keys' primary key
// to (key_hash, tenant_id) (IMPROVEMENT-PLAN 3.10); 020 adds event_history's
// intent_at, which LoadEventHistory selects on every dialect (1.4 phase D).
//
// The list is explicit rather than a directory glob. The other files in
// migrations/postgres/ are not table shape: 002 seeds defaults, 003 and 004
// define finalize_workflow_status (applied by applyPostgresProcedures in the
// tests that exercise it), and 005 creates the cleat_app role, which
// SetupPostgresRLSRole handles on its own terms. Globbing would drag all of
// those into every SetupFullSchema call.
//
// A migration that adds or alters a column therefore has to be added here.
// That is a maintenance cost, and the alternative -- a second, hand-written
// copy of the DDL in Go -- is the one this function exists to avoid.
func postgresSchemaFiles() []string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("postgresSchemaFiles: runtime.Caller failed")
	}
	// thisFile is .../engine/testutil/schema.go; the repo root is two
	// levels up, and the migrations live under migrations/postgres/.
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", "postgres")
	return []string{
		filepath.Join(dir, "001_schema.sql"),
		filepath.Join(dir, "010_idempotency_keys_tenant_id.sql"),
		filepath.Join(dir, "020_event_intent.sql"),
		filepath.Join(dir, "021_schedule_timezone.sql"),
		filepath.Join(dir, "022_schedule_policies.sql"),
	}
}

// applyPostgresSchemaFile reads and executes each postgresSchemaFiles() entry
// against db, in order. lib/pq's simple query protocol accepts a whole
// multi-statement file as a single Exec (as applyPostgresProcedures in
// store_backends_procedures_test.go already relies on for 003/004).
//
// The statements are idempotent (CREATE ... IF NOT EXISTS, CREATE OR REPLACE,
// DROP POLICY IF EXISTS ... CREATE POLICY), so this is safe to call more than
// once against the same database **sequentially**. It is NOT safe to call
// concurrently, and an earlier version of this comment claimed otherwise --
// which is why the resulting flake read as mysterious rather than obvious
// (IMPROVEMENT-PLAN §2.21). PostgreSQL's IF NOT EXISTS forms are not atomic:
// two sessions both observe the object missing, both insert the catalog row,
// and one loses on a unique index. Observed as
//
//	pq: duplicate key value violates unique constraint
//	"pg_extension_name_index" (23505)
//
// from CREATE EXTENSION IF NOT EXISTS pgcrypto, though CREATE TABLE IF NOT
// EXISTS carries the same hazard.
//
// Concurrency here is the norm, not the exception: `go test ./plugins/...`
// runs distinct packages in parallel (-p defaults to NumCPU) and they all
// point at the same CLEAT_TEST_POSTGRES database. So the apply is serialised
// with a session-level advisory lock.
//
// The lock is taken on a single pinned *sql.Conn rather than on db. Advisory
// locks belong to a session, and database/sql hands out arbitrary pooled
// connections per call -- so locking via db could take the lock on one
// connection and try to release it on another, which silently fails to
// unlock and leaks the lock for the life of that connection.
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
	paths := postgresSchemaFiles()
	files := make([][]byte, 0, len(paths))
	var combined []byte
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		files = append(files, data)
		combined = append(combined, data...)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("apply postgres schema: acquire connection: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaApplyLockKey); err != nil {
		t.Fatalf("apply postgres schema: acquire advisory lock: %v", err)
	}
	defer func() {
		// Release explicitly. conn.Close() only returns the connection to the
		// pool; the session lives on and would keep holding the lock.
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, schemaApplyLockKey); err != nil {
			t.Errorf("apply postgres schema: release advisory lock: %v", err)
		}
	}()

	// Skip the DDL entirely when this exact schema file has already been
	// applied to this database.
	//
	// The advisory lock above serialises schema application against schema
	// application, which is not the collision that actually bites. This
	// function used to run on *every* TestDB call -- 24 times for
	// tests/integrity alone -- and every run takes ACCESS EXCLUSIVE on tables
	// another package's tests are reading and writing at that moment. Go runs
	// distinct packages in parallel against the same CLEAT_TEST_DB, so
	// `go test ./tests/integrity/... ./tests/upgrade/... ./tests/scale/...`
	// deadlocked DDL against DML:
	//
	//	apply migrations/postgres/001_schema.sql: pq: deadlock detected (40P01)
	//	append events in tx: increment event_count: pq: deadlock detected (40P01)
	//
	// 17 failures, every one of which passes when the suites are run one at a
	// time -- a screen of red that means nothing, which is its own kind of
	// false signal. Fingerprinting makes the DDL run once for a given schema
	// file instead of once per test. IMPROVEMENT-PLAN 2.39.
	//
	// Tests that add their own columns (all IF NOT EXISTS) or drop objects
	// they themselves created are unaffected: the fingerprint tracks the
	// schema *files*, and re-applying them is exactly what those tests do not
	// need. The fingerprint covers every file in the list, so adding one to
	// postgresSchemaFiles re-applies the whole set once on each database.
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(combined))
	var applied string
	err = conn.QueryRowContext(ctx, `SELECT fingerprint FROM cleat_test_schema WHERE id = 1`).Scan(&applied)
	if err == nil && applied == fingerprint {
		return
	}

	// No-args Exec keeps lib/pq on the simple query protocol, which is what
	// allows the whole multi-statement file to go in one round trip.
	for i, data := range files {
		if _, err := conn.ExecContext(ctx, string(data)); err != nil {
			t.Fatalf("apply %s: %v", paths[i], err)
		}
	}

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cleat_test_schema (
		id INTEGER PRIMARY KEY, fingerprint TEXT NOT NULL)`); err != nil {
		t.Fatalf("apply postgres schema: create fingerprint table: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO cleat_test_schema (id, fingerprint) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET fingerprint = EXCLUDED.fingerprint`, fingerprint); err != nil {
		t.Fatalf("apply postgres schema: record fingerprint: %v", err)
	}
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
