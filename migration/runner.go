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

const (
	maxMigrationRetries = 3
	migrationRetryDelay = 1 * time.Second
)

// Runner applies pending SQL migrations to a database.
type Runner struct {
	db            *sql.DB
	dialect       engine.Dialect
	migrationsDir string
	lockTimeout   time.Duration
	// applyMigrationFn is the function called to apply a single migration.
	// Defaults to Runner.applyMigration; overridden in tests.
	applyMigrationFn func(ctx context.Context, m migration) error
}

// migration represents a single versioned SQL migration file.
type migration struct {
	version int
	name    string
	sql     string
}

// NewRunner creates a migration runner that reads .sql files from the
// dialect-specific subdirectory under dir and applies pending ones against db.
// lockTimeout is the per-migration lock timeout (0 = no timeout).
func NewRunner(db *sql.DB, dialect engine.Dialect, dir string, lockTimeout time.Duration) *Runner {
	r := &Runner{
		db:            db,
		dialect:       dialect,
		migrationsDir: dir,
		lockTimeout:   lockTimeout,
	}
	r.applyMigrationFn = r.applyMigration
	return r
}

// Run applies all pending migrations in version order within individual
// transactions. It creates the schema_migrations tracking table if it
// does not already exist. Run returns the first error encountered;
// no further migrations are attempted after a failure.
func (r *Runner) Run(ctx context.Context) error {
	// 1. Ensure the tracking table exists.
	if err := r.ensureMigrationsTable(ctx); err != nil {
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
	applied, err := r.getAppliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("get applied versions: %w", err)
	}

	// 4. Apply each pending migration in order, retrying up to
	//    maxMigrationRetries times on failure.
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}

		if err := runMigrationWithRetry(ctx, m.name, func(ctx context.Context) error {
			return r.applyMigrationFn(ctx, m)
		}); err != nil {
			return err
		}
	}

	return nil
}

// ensureMigrationsTable creates the schema_migrations tracking table if it
// does not already exist, using the appropriate DDL for each dialect.
func (r *Runner) ensureMigrationsTable(ctx context.Context) error {
	var ddl string
	switch r.dialect {
	case engine.DialectPostgres:
		ddl = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
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
	_, err := r.db.ExecContext(ctx, ddl)
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
func (r *Runner) getAppliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT version FROM schema_migrations")
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

// runMigrationWithRetry applies a single migration via applyFn, retrying
// up to maxMigrationRetries times with migrationRetryDelay between attempts.
// On final failure it returns an error including the migration name and
// attempt count.
func runMigrationWithRetry(ctx context.Context, name string, applyFn func(context.Context) error) error {
	log.Printf("[migration] applying %s", name)
	var lastErr error
	for attempt := 1; attempt <= maxMigrationRetries; attempt++ {
		if err := applyFn(ctx); err != nil {
			lastErr = err
			if attempt < maxMigrationRetries {
				log.Printf("[migration] attempt %d/%d for %s failed: %v — retrying in %v",
					attempt, maxMigrationRetries, name, err, migrationRetryDelay)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(migrationRetryDelay):
				}
				continue
			}
		} else {
			log.Printf("[migration] applied %s", name)
			return nil
		}
	}
	return fmt.Errorf("migration %s failed after %d attempts: %w", name, maxMigrationRetries, lastErr)
}

// lockTimeoutSQL returns the dialect-specific SET statement for the
// configured lock timeout. Returns empty string for unknown dialects.
func (r *Runner) lockTimeoutSQL() string {
	switch r.dialect {
	case engine.DialectPostgres:
		return fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", r.lockTimeout.Milliseconds())
	case engine.DialectMySQL:
		return fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", int(r.lockTimeout.Seconds()))
	case engine.DialectMSSQL:
		return fmt.Sprintf("SET LOCK_TIMEOUT %d", r.lockTimeout.Milliseconds())
	default:
		return ""
	}
}

// applyMigration executes a single migration within its own transaction.
// On success the version is recorded in schema_migrations and the
// transaction is committed. On failure the transaction is rolled back
// and the error is returned.
func (r *Runner) applyMigration(ctx context.Context, m migration) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// Rollback is a no-op after a successful Commit.
		_ = tx.Rollback()
	}()

	// Set per-migration lock timeout so a blocked DDL statement fails
	// quickly instead of hanging indefinitely.
	if r.lockTimeout > 0 {
		if _, err := tx.ExecContext(ctx, r.lockTimeoutSQL()); err != nil {
			return fmt.Errorf("set lock timeout: %w", err)
		}
	}

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

	versionStr := strconv.Itoa(m.version)
	var recordSQL string
	var recordArgs []interface{}
	switch r.dialect {
	case engine.DialectPostgres:
		recordSQL = "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2) ON CONFLICT (version) DO NOTHING"
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
