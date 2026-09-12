// Package migration provides a lightweight SQL migration runner for cleat.
//
// Migration files follow the naming convention NNN_name.sql and live in a
// configurable directory (default "migrations/"). Applied versions are tracked
// in a schema_migrations table. On each Run call, only pending (unapplied)
// migrations are executed in version order, each within its own transaction.
package migration

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Dialect identifies the SQL dialect a Runner applies migrations for.
//
// Deliberately not engine.Dialect: this package is meant to be a leaf that
// anything bootstrapping a database schema can depend on, including
// engine/testutil. engine/testutil is imported by many of engine's,
// migration's and plugin's own *_test.go files (internal tests, same
// package as the code under test), and Go refuses to build a package whose
// internal test files import something that imports the package itself
// ("import cycle not allowed in test") -- so if this package imported
// engine.Dialect, engine/testutil could not import this package without
// breaking every one of those. A three-value string enum is cheap enough to
// declare twice; callers that already have an engine.Dialect (e.g.
// cmd/cleat-worker/main.go) convert with a plain string conversion, since
// the two types share the same underlying values by construction --
// TestDialectValuesMatchEngine below pins that.
type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectMySQL    Dialect = "mysql"
	DialectMSSQL    Dialect = "mssql"
)

// Runner applies pending SQL migrations to a database.
type Runner struct {
	db            *sql.DB
	dialect       Dialect
	migrationsDir string
	schema        string
}

// migration represents a single versioned SQL migration file.
type migration struct {
	version int
	name    string
	sql     string
}

// migrationsLockKey is the pg_advisory_lock key used to serialise migration
// runs. Every worker runs migrations at boot, so in a cluster N workers start
// applying the same DDL at the same instant. That produced two distinct
// failures against a database that was still being built:
//
//	duplicate key value violates unique constraint "pg_type_typname_nsp_index"
//	relation "tenant_api_keys" does not exist
//
// The first is PostgreSQL's well-known CREATE TABLE IF NOT EXISTS race: the
// existence check and the catalogue insert are not atomic with respect to
// another session doing the same thing. The second is one worker reading a
// half-applied schema while another is still writing it.
//
// The value is arbitrary but must never change: it is the identity of the
// lock, and two cleat versions that disagree about it would not exclude each
// other.
const migrationsLockKey int64 = 7215842093104561

// trackingTable returns the schema_migrations reference to use for this
// dialect.
//
// On PostgreSQL it is schema-qualified, and that qualification is load-bearing
// rather than stylistic. A bare SET is session-scoped, not
// transaction-scoped, so a migration file that changed search_path would
// outlive the transaction applyMigration runs it in and change name
// resolution for every later statement on this pinned connection -- including
// the runner's own bookkeeping. Unqualified, the tracking table was created
// under one search_path before any migration ran and then looked up under
// another afterwards:
//
//	[migration] applying 001_schema.sql
//	ERROR: relation "schema_migrations" does not exist (42P01)
//	  INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)
//
// which aborted the transaction, rolled 001 back, and failed the worker's
// boot outright. Qualifying the name makes the runner independent of whatever
// a migration file does to search_path -- which matters less now that the
// files no longer set it (cleat#1287, see WithSchema) and is kept because
// "no file does this today" is not a property anything checks.
//
// What the qualifier IS depends on the configured schema. It was the literal
// public until cleat#1287; with --schema it has to follow, or the runner
// records its bookkeeping in a different schema from the one it is building
// into and every boot re-applies every migration.
//
// SET LOCAL is not an alternative to the session-level SET in session(): the
// same files are also applied by docker-entrypoint-initdb.d via psql
// (deploy/postgres/100-apply-migrations.sh), where each statement runs in its
// own implicit transaction and a LOCAL setting would be discarded
// immediately. That path sets search_path through PGOPTIONS instead.
func (r *Runner) trackingTable() string {
	if r.dialect == DialectPostgres {
		return r.schemaIdent() + ".schema_migrations"
	}
	return "schema_migrations"
}

// schemaIdent is the PostgreSQL schema this runner builds into, ready to
// interpolate. It defaults to public, which is what every caller that does not
// call WithSchema gets and what cleat has always done.
//
// Interpolated rather than parameterised because a schema name cannot be a
// bind parameter in SET or in a qualified table name. WithSchema is what makes
// that safe: it rejects anything that is not a plain identifier at
// construction time, so the value reaching here is [A-Za-z_][A-Za-z0-9_]* and
// needs no quoting. An invalid name fails the Run rather than being escaped
// into something that would work -- a schema called "my schema" is far more
// likely to be a mistake than an intention.
func (r *Runner) schemaIdent() string {
	if r.schema == "" {
		return "public"
	}
	return r.schema
}

// searchPath is what the runner sets on its connection: the configured schema
// followed by pg_temp.
//
// The trailing pg_temp is not decoration. The SECURITY DEFINER functions in
// migrations/postgres/ that carry `SET search_path FROM CURRENT` freeze
// whatever this value is onto themselves at creation time -- that is how they
// follow --schema without any substitution step.
//
// No count, deliberately. This said "four" and there were three: five files
// contain the statement and 040's admin.claim_workflows is superseded by
// 055's, so counting statements over-reports the functions by one. Both
// numbers are also a census of a growing population. Re-derive if you need
// the set, deduping by name so the last definition wins:
//
//	grep -rln 'SET search_path FROM CURRENT' migrations/postgres/ PostgreSQL searches
//
// pg_temp FIRST when it is not named explicitly, so a captured path of
// "cleat_prod" alone would let a temporary table shadow a real one inside a
// function that holds an RLS exemption. Naming it last is the standard
// hardening and is the whole reason it is here rather than in schemaIdent.
func (r *Runner) searchPath() string {
	return r.schemaIdent() + ", pg_temp"
}

// NewRunner creates a migration runner that reads .sql files from the
// dialect-specific subdirectory under dir and applies pending ones against db.
func NewRunner(db *sql.DB, dialect Dialect, dir string) *Runner {
	return &Runner{
		db:            db,
		dialect:       dialect,
		migrationsDir: dir,
	}
}

// WithSchema directs the runner at a PostgreSQL schema other than public,
// which is what cmd/cleat-worker's --schema flag names.
//
// WHY THIS IS NOT JUST A PREFIX ON THE TRACKING TABLE. Before cleat#1287 the
// schema every migration built into was a literal: nineteen of the files in
// migrations/postgres/ opened with `SET search_path = public;` and this
// package's tracking table was hardcoded public.schema_migrations. The other
// twenty-five files did not pin, so they ran under whatever the connection
// carried -- and with --schema set, that is the configured schema. The core
// schema was therefore built across two schemas at once and the run died
// partway:
//
//	migration 020_event_intent.sql: execute: pq: relation "event_history"
//	does not exist (42P01)
//
// 020 is simply the first file in version order that does not pin. So the
// runner owns search_path now and the files no longer state it, which is the
// only arrangement where the answer cannot depend on which file you are in.
//
// WHY THE LITERAL WAS THERE AND WHAT STILL DEFENDS AGAINST IT. The default
// search_path is "$user", public, so an unqualified CREATE lands in a schema
// named after the connecting role whenever one exists -- and 001_schema.sql
// creates a schema called "cleat" while docker-compose.cluster.yml connects as
// POSTGRES_USER=cleat, so the entire schema was once built inside "cleat"
// instead of public. Setting search_path explicitly, to public or to anything
// else, still defeats that. What is gone is the assumption that the answer is
// always public.
//
// An empty schema, or "public", leaves the runner exactly as it was.
func (r *Runner) WithSchema(schema string) *Runner {
	r.schema = schema
	return r
}

// plainIdentifier matches a PostgreSQL identifier that needs no quoting.
//
// Declared here rather than shared with plugin.isPlainIdentifier because this
// package is deliberately a leaf -- see the Dialect comment above for what
// importing upward costs in test-only import cycles. The duplication is four
// tokens of regexp; the cycle is not.
var plainIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Run applies all pending migrations in version order within individual
// transactions. It creates the schema_migrations tracking table if it
// does not already exist. Run returns the first error encountered;
// no further migrations are attempted after a failure.
func (r *Runner) Run(ctx context.Context) error {
	// 0. Pin one connection and serialise against other processes running
	//    migrations concurrently.
	//
	//    Everything below runs on that same connection. A session-level
	//    advisory lock belongs to the connection that took it, so the lock
	//    would have to be held on a connection of its own otherwise -- and a
	//    Runner that needs two connections deadlocks against a pool of one,
	//    which is a legitimate configuration for a process whose only job at
	//    that moment is to migrate.
	session, release, err := r.session(ctx)
	if err != nil {
		return err
	}
	defer release()

	// 1. Ensure the tracking table exists.
	if err := r.ensureMigrationsTable(ctx, session); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	// 2. Read and sort migration files from disk.
	migrations, err := r.readMigrations()
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	if len(migrations) == 0 {
		log.Println("[migration] no migration files found")
		return nil
	}

	// 3. Determine which versions have already been applied.
	applied, err := r.getAppliedVersions(ctx, session)
	if err != nil {
		return fmt.Errorf("get applied versions: %w", err)
	}

	// 4. Apply each pending migration in order.
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}

		log.Printf("[migration] applying %s", m.name)
		if err := r.applyMigration(ctx, session, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		log.Printf("[migration] applied %s", m.name)
	}

	return nil
}

// sqlSession is the subset of *sql.DB and *sql.Conn the runner uses. Both
// types satisfy it, which lets Run pin a single connection on PostgreSQL while
// the other dialects keep using the pool.
type sqlSession interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// session returns the handle the run should use and a release function.
//
// On PostgreSQL it pins one connection and takes a database-wide advisory lock
// on it, so that only one process applies migrations at a time. Every worker
// migrates at boot and docker-compose.cluster.yml starts four at once; see
// migrationsLockKey for what that produced without the lock.
//
// Only PostgreSQL is covered. MySQL (GET_LOCK) and SQL Server (sp_getapplock)
// have equivalents, but cleat only ships a multi-worker topology for
// PostgreSQL, and untested locking code for the other two would be worse than
// none: there, this returns the pool unchanged and the behaviour is exactly
// what it was before.
func (r *Runner) session(ctx context.Context) (sqlSession, func(), error) {
	if r.dialect != DialectPostgres {
		return r.db, func() {}, nil
	}

	if r.schema != "" && !plainIdentifier.MatchString(r.schema) {
		return nil, nil, fmt.Errorf(
			"schema %q is not a plain identifier; --schema takes an unquoted "+
				"PostgreSQL schema name", r.schema)
	}

	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire migration lock: connection: %w", err)
	}
	// pg_advisory_lock blocks until the lock is free; ctx bounds the wait.
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationsLockKey); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	// The schema has to exist before search_path can usefully point at it.
	// SET against a schema that does not exist is NOT an error in PostgreSQL
	// -- resolution is lazy -- so skipping this would surface much later and
	// much less clearly, as "no schema has been selected to create in" from
	// the first CREATE TABLE. Creating it here rather than relying on
	// PostgresStoreFactory (engine/db.go) is deliberate: the runner is the
	// first thing that needs the schema and runs before the factory builds
	// anything, so depending on the factory would make boot order load-bearing.
	if r.schemaIdent() != "public" {
		if _, err := conn.ExecContext(ctx,
			"CREATE SCHEMA IF NOT EXISTS "+r.schemaIdent()); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("create schema %s: %w", r.schemaIdent(), err)
		}
	}
	// Pin the creation target for every unqualified name in every migration
	// file. This is the statement the files used to carry themselves; see
	// WithSchema for why it moved here.
	if _, err := conn.ExecContext(ctx, "SET search_path = "+r.searchPath()); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("pin search_path to %s: %w", r.schemaIdent(), err)
	}
	return conn, func() {
		free := context.WithoutCancel(ctx)
		// Undo the pin before letting go. *sql.Conn.Close() RETURNS the
		// connection to the pool rather than closing it, so the session-level
		// SET above would otherwise ride back into the pool and leave one
		// connection resolving unqualified names differently from every other
		// one in it -- a difference that only shows up under load, when that
		// particular connection happens to be the one handed out.
		// TestRunner_LeavesSearchPathUnchanged is the regression test, and it
		// caught exactly this when the pin moved here from the files.
		_, _ = conn.ExecContext(free, "RESET search_path")
		// Closing the connection releases the lock on its own, but unlocking
		// explicitly returns it promptly even if the driver keeps the
		// connection around. WithoutCancel so release still works when the
		// run was cut short by a cancelled context.
		_, _ = conn.ExecContext(free,
			"SELECT pg_advisory_unlock($1)", migrationsLockKey)
		conn.Close()
	}, nil
}

// ensureMigrationsTable creates the schema_migrations tracking table if it
// does not already exist, using the appropriate DDL for each dialect.
func (r *Runner) ensureMigrationsTable(ctx context.Context, session sqlSession) error {
	var ddl string
	switch r.dialect {
	case DialectPostgres:
		ddl = `
		CREATE TABLE IF NOT EXISTS ` + r.trackingTable() + ` (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
		`
	case DialectMySQL:
		ddl = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    VARCHAR(255) PRIMARY KEY,
			applied_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
		)
		`
	case DialectMSSQL:
		ddl = `
		IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'schema_migrations')
			CREATE TABLE schema_migrations (
				version    NVARCHAR(255) PRIMARY KEY,
				applied_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
			)
		`
	default:
		return fmt.Errorf("unsupported dialect: %s", r.dialect)
	}
	_, err := session.ExecContext(ctx, ddl)
	return err
}

// readMigrations scans the dialect-specific subdirectory for .sql files
// matching the NNN_name.sql naming convention and returns them sorted by
// version number. Files that do not match the convention are silently skipped.
func (r *Runner) readMigrations() ([]migration, error) {
	migDir := filepath.Join(r.migrationsDir, string(r.dialect))
	entries, err := os.ReadDir(migDir)
	if err != nil {
		return nil, fmt.Errorf("read directory %s: %w", migDir, err)
	}

	var migrations []migration
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}

		// Parse the leading numeric version from "NNN_name.sql".
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			// Not a versioned migration file; skip.
			continue
		}

		data, err := os.ReadFile(filepath.Join(migDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		migrations = append(migrations, migration{
			version: version,
			name:    name,
			sql:     string(data),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	return migrations, nil
}

// getAppliedVersions returns the set of version numbers that are already
// recorded in the schema_migrations table.
func (r *Runner) getAppliedVersions(ctx context.Context, session sqlSession) (map[int]bool, error) {
	rows, err := session.QueryContext(ctx, "SELECT version FROM "+r.trackingTable())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var versionStr string
		if err := rows.Scan(&versionStr); err != nil {
			return nil, err
		}
		version, err := strconv.Atoi(versionStr)
		if err != nil {
			// If the stored version isn't a plain integer (e.g. legacy),
			// skip it so it doesn't block future numeric migrations.
			continue
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return applied, nil
}

// applyMigration executes a single migration within its own transaction.
// On success the version is recorded in schema_migrations and the
// transaction is committed. On failure the transaction is rolled back
// and the error is returned.
func (r *Runner) applyMigration(ctx context.Context, session sqlSession, m migration) error {
	tx, err := session.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// Rollback is a no-op after a successful Commit.
		_ = tx.Rollback()
	}()

	// MySQL and MSSQL drivers require statement splitting for
	// multi-statement SQL. Postgres (pq) handles it natively.
	switch r.dialect {
	case DialectMySQL:
		for _, stmt := range splitSQL(m.sql) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("execute: %w", err)
			}
		}
	case DialectMSSQL:
		for _, batch := range splitMSSQL(m.sql) {
			if _, err := tx.ExecContext(ctx, batch); err != nil {
				return fmt.Errorf("execute: %w", err)
			}
		}
	default:
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("execute: %w", err)
		}
	}

	// Restore the runner's search_path, in case the migration file changed it.
	// A bare SET in a file is session-scoped, not transaction-scoped, so it
	// outlives this transaction and would change name resolution for every
	// later file on this pinned connection.
	//
	// This used to be RESET, and RESET is the wrong instruction now: it
	// returns the connection to the value from the startup packet, which for a
	// pool opened without search_path in its DSN is the default "$user",
	// public -- so from the second file onward the runner would be building
	// into a role-named schema again, which is the original cleat#1287 defect
	// wearing a different hat. Setting it back to the configured value is what
	// makes "which file am I in" stop mattering.
	if r.dialect == DialectPostgres {
		if _, err := tx.ExecContext(ctx, "SET search_path = "+r.searchPath()); err != nil {
			return fmt.Errorf("restore search_path: %w", err)
		}
	}

	versionStr := strconv.Itoa(m.version)
	var recordSQL string
	var recordArgs []interface{}
	switch r.dialect {
	case DialectPostgres:
		recordSQL = "INSERT INTO " + r.trackingTable() + " (version, applied_at) VALUES ($1, $2) ON CONFLICT (version) DO NOTHING"
		recordArgs = []interface{}{versionStr, time.Now()}
	case DialectMySQL:
		recordSQL = "INSERT IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)"
		recordArgs = []interface{}{versionStr, time.Now()}
	case DialectMSSQL:
		recordSQL = "IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = @p1) INSERT INTO schema_migrations (version, applied_at) VALUES (@p1, @p2)"
		recordArgs = []interface{}{versionStr, time.Now()}
	default:
		return fmt.Errorf("unsupported dialect: %s", r.dialect)
	}
	if _, err := tx.ExecContext(ctx, recordSQL, recordArgs...); err != nil {
		return fmt.Errorf("record version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// SplitSQL splits a multi-statement MySQL script into individual statements,
// honouring DELIMITER directives, quoted strings/identifiers, and comments.
// See the unexported splitSQL below for the parsing rules.
//
// It is exported so that other packages needing MySQL-correct statement
// splitting -- notably tests/plugin-harness's migration test setup, which
// used to carry its own copy that did not understand DELIMITER and silently
// mangled migrations/mysql/003_procedures.sql -- can reuse this rather than
// diverging from it again.
func SplitSQL(sql string) []string {
	return splitSQL(sql)
}

// SplitMSSQL splits a MSSQL SQL string into batches on GO lines.
//
// It is exported so that other packages needing MSSQL-correct statement
// splitting -- notably tests/plugin-harness's migration test setup, which
// used to carry its own copy that also split on every semicolon inside a
// batch -- can reuse this rather than diverging from it again. Splitting a
// stored procedure body's internal semicolons the same way a top-level
// statement separator would is the MSSQL analogue of what MySQL's DELIMITER
// directive guards against: it cuts CREATE OR ALTER PROCEDURE
// dbo.finalize_workflow_status (migrations/mssql/003_procedures.sql) into
// fragments and sends them to the server individually, which fails with
// "Incorrect syntax" partway through the body. GO is the only batch
// separator MSSQL recognises; everything else inside a batch, semicolons
// included, is the server's job to parse as one unit.
func SplitMSSQL(sql string) []string {
	return splitMSSQL(sql)
}

// splitMSSQL splits a MSSQL SQL string into batches on GO lines.
// GO must appear on its own line (case-insensitive, optional trailing whitespace).
func splitMSSQL(sql string) []string {
	var batches []string
	lines := strings.Split(sql, "\n")
	start := 0
	for i, line := range lines {
		if strings.TrimSpace(strings.ToUpper(line)) == "GO" {
			batch := strings.TrimSpace(strings.Join(lines[start:i], "\n"))
			if batch != "" {
				batches = append(batches, batch)
			}
			start = i + 1
		}
	}
	// Final batch after last GO
	batch := strings.TrimSpace(strings.Join(lines[start:], "\n"))
	if batch != "" {
		batches = append(batches, batch)
	}
	return batches
}

// splitSQL splits a multi-statement MySQL script into individual statements.
//
// It used to be `strings.Split(sql, ";")`, which cut on every semicolon in the
// file regardless of what that semicolon was part of. Neither shipped MySQL
// file survived it, and since every cleat-worker runs this at boot and exits
// when it fails, neither could a worker on a database that had not been
// schema'd by hand (IMPROVEMENT-PLAN 3.13):
//
//   - 001_schema.sql line 7 is the comment "-- CREATE INDEX has no IF NOT
//     EXISTS in MySQL 8.0; re-runs error harmlessly." The naive split sent
//     `re-runs error harmlessly.` to the server as a statement.
//   - 003_procedures.sql defines finalize_workflow_status, whose body is full
//     of semicolons, and wraps it in `DELIMITER //`. Splitting on ';' cuts the
//     procedure into fragments; `DELIMITER` itself is a client directive that
//     no server has ever accepted.
//
// So this tracks the four things a semicolon can be inside — a line comment, a
// block comment, a quoted string, a quoted identifier — and honours DELIMITER,
// which is what the file is asking the client to do.
//
// It is deliberately not a SQL parser. It knows just enough to find statement
// boundaries, which is the job; anything beyond that belongs to the server.
func splitSQL(sql string) []string {
	var (
		stmts     []string
		cur       strings.Builder
		delimiter = ";"
	)

	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		// A fragment that is only comments and whitespace is not a statement.
		// MySQL rejects an empty query outright (1065), which is how a
		// trailing comment block turns into a failed migration.
		if s != "" && !isAllComments(s) {
			stmts = append(stmts, s)
		}
	}

	for i := 0; i < len(sql); {
		// DELIMITER is recognised only at the start of a line, which is where
		// the client tools recognise it and where the shipped files put it.
		if atLineStart(sql, i) {
			if word, rest, ok := parseDelimiterDirective(sql[i:]); ok {
				flush()
				delimiter = word
				i += len(sql[i:]) - len(rest)
				continue
			}
		}

		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = len(sql) // unterminated: the rest is comment
				continue
			}
			cur.WriteString(" ")
			i += 2 + end + 2

		// MySQL requires whitespace (or end of line) after `--`; `--x` is not
		// a comment. `#` needs no such thing.
		case strings.HasPrefix(sql[i:], "#"),
			strings.HasPrefix(sql[i:], "--") && (i+2 >= len(sql) || isSpaceByte(sql[i+2])):
			nl := strings.IndexByte(sql[i:], '\n')
			if nl < 0 {
				i = len(sql)
				continue
			}
			cur.WriteString(" ")
			i += nl // leave the newline for atLineStart

		case sql[i] == '\'', sql[i] == '"', sql[i] == '`':
			lit, n := scanQuoted(sql[i:])
			cur.WriteString(lit)
			i += n

		case delimiter != "" && strings.HasPrefix(sql[i:], delimiter):
			flush()
			i += len(delimiter)

		default:
			cur.WriteByte(sql[i])
			i++
		}
	}
	flush()
	return stmts
}

// atLineStart reports whether i is at the beginning of a line (only whitespace
// behind it on that line).
func atLineStart(sql string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch sql[j] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		default:
			return false
		}
	}
	return true
}

// parseDelimiterDirective recognises a leading `DELIMITER <token>` line and
// returns the new delimiter along with the input remaining after that line.
func parseDelimiterDirective(s string) (delim, rest string, ok bool) {
	const kw = "delimiter"
	if len(s) < len(kw) || !strings.EqualFold(s[:len(kw)], kw) {
		return "", "", false
	}
	r := s[len(kw):]
	trimmed := strings.TrimLeft(r, " \t")
	if len(trimmed) == len(r) { // no separating whitespace: not the directive
		return "", "", false
	}
	line := trimmed
	if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
		line = trimmed[:nl]
		rest = trimmed[nl+1:]
	} else {
		rest = ""
	}
	delim = strings.TrimSpace(line)
	if delim == "" {
		return "", "", false
	}
	return delim, rest, true
}

// scanQuoted consumes one quoted string or identifier starting at s[0] and
// returns it verbatim along with how many bytes it used.
//
// Both escape forms are handled, because both appear in real schemas: a
// backslash escape, and a doubled quote character (” inside ”, “ inside “).
func scanQuoted(s string) (string, int) {
	q := s[0]
	var b strings.Builder
	b.WriteByte(q)
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && q != '`' && i+1 < len(s):
			// Backslash escapes do not apply inside backtick identifiers.
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i++
		case c == q && i+1 < len(s) && s[i+1] == q:
			b.WriteByte(c)
			b.WriteByte(c)
			i++
		case c == q:
			b.WriteByte(c)
			return b.String(), i + 1
		default:
			b.WriteByte(c)
		}
	}
	// Unterminated quote: hand back what there is and let the server complain,
	// which produces a better message than anything this function could.
	return b.String(), len(s)
}

// isAllComments reports whether a fragment carries no SQL at all -- only line
// and block comments and whitespace. Such a fragment is what a trailing
// comment block at the end of a file leaves behind, and sending it produces
// "Query was empty" (1065) rather than a no-op.
func isAllComments(s string) bool {
	for i := 0; i < len(s); {
		switch {
		case isSpaceByte(s[i]):
			i++
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return true
			}
			i += 2 + end + 2
		case strings.HasPrefix(s[i:], "#"),
			strings.HasPrefix(s[i:], "--") && (i+2 >= len(s) || isSpaceByte(s[i+2])):
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				return true
			}
			i += nl + 1
		default:
			return false
		}
	}
	return true
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
