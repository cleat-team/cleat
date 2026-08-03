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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// Runner applies pending SQL migrations to a database.
type Runner struct {
	db            *sql.DB
	dialect       engine.Dialect
	migrationsDir string
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
// rather than stylistic. The migration files begin with
//
//	SET search_path = public;
//
// (see the header of migrations/postgres/001_schema.sql for why). A bare SET
// is session-scoped, not transaction-scoped, so it outlives the transaction
// that applyMigration runs the file in and changes name resolution for every
// later statement on that pooled connection -- including this runner's own
// bookkeeping. Unqualified, the tracking table was created under the default
// search_path ("$user", public) before any migration ran, and then looked up
// under the changed one afterwards:
//
//	[migration] applying 001_schema.sql
//	ERROR: relation "schema_migrations" does not exist (42P01)
//	  INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)
//
// which aborted the transaction, rolled 001 back, and failed the worker's
// boot outright. Qualifying the name makes the runner independent of whatever
// the migration files do to search_path.
//
// SET LOCAL is not an alternative: the same files are also applied by
// docker-entrypoint-initdb.d via psql, where each statement runs in its own
// implicit transaction and a LOCAL setting would be discarded immediately.
func (r *Runner) trackingTable() string {
	if r.dialect == engine.DialectPostgres {
		return "public.schema_migrations"
	}
	return "schema_migrations"
}

// NewRunner creates a migration runner that reads .sql files from the
// dialect-specific subdirectory under dir and applies pending ones against db.
func NewRunner(db *sql.DB, dialect engine.Dialect, dir string) *Runner {
	return &Runner{
		db:            db,
		dialect:       dialect,
		migrationsDir: dir,
	}
}

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
	if r.dialect != engine.DialectPostgres {
		return r.db, func() {}, nil
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
	return conn, func() {
		// Closing the connection releases the lock on its own, but unlocking
		// explicitly returns it promptly even if the driver keeps the
		// connection around. WithoutCancel so release still works when the
		// run was cut short by a cancelled context.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", migrationsLockKey)
		conn.Close()
	}, nil
}

// ensureMigrationsTable creates the schema_migrations tracking table if it
// does not already exist, using the appropriate DDL for each dialect.
func (r *Runner) ensureMigrationsTable(ctx context.Context, session sqlSession) error {
	var ddl string
	switch r.dialect {
	case engine.DialectPostgres:
		ddl = `
		CREATE TABLE IF NOT EXISTS ` + r.trackingTable() + ` (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
		`
	case engine.DialectMySQL:
		ddl = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    VARCHAR(255) PRIMARY KEY,
			applied_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
		)
		`
	case engine.DialectMSSQL:
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
	case engine.DialectMySQL:
		for _, stmt := range splitSQL(m.sql) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("execute: %w", err)
			}
		}
	case engine.DialectMSSQL:
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

	// Undo any session-level SET the migration file performed, so that this
	// connection goes back into the pool configured the same way as every
	// other one. The files do set search_path (see trackingTable), and a pool
	// where one connection resolves unqualified names differently from the
	// rest is a source of failures that only reproduce under load.
	if r.dialect == engine.DialectPostgres {
		if _, err := tx.ExecContext(ctx, "RESET search_path"); err != nil {
			return fmt.Errorf("reset search_path: %w", err)
		}
	}

	versionStr := strconv.Itoa(m.version)
	var recordSQL string
	var recordArgs []interface{}
	switch r.dialect {
	case engine.DialectPostgres:
		recordSQL = "INSERT INTO " + r.trackingTable() + " (version, applied_at) VALUES ($1, $2) ON CONFLICT (version) DO NOTHING"
		recordArgs = []interface{}{versionStr, time.Now()}
	case engine.DialectMySQL:
		recordSQL = "INSERT IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)"
		recordArgs = []interface{}{versionStr, time.Now()}
	case engine.DialectMSSQL:
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

// splitSQL splits a multi-statement SQL string into individual statements
// on semicolons, ignoring trailing whitespace and empty statements.
func splitSQL(sql string) []string {
	var stmts []string
	for _, s := range strings.Split(sql, ";") {
		s = strings.TrimSpace(s)
		if s != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}
