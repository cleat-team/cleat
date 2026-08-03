package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
)

// Dialect identifies the SQL dialect of the backing database.
type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectMySQL    Dialect = "mysql"
	DialectMSSQL    Dialect = "mssql"
)

// NOTE: Callers must update their invocation of RunMigrations to pass a dialect parameter.
// The signature changed from:
//
//	RunMigrations(ctx, db, coreMigrations, plugins)
//
// to:
//
//	RunMigrations(ctx, db, dialect, coreMigrations, plugins)
//
// Plugins can provide dialect-specific migration SQL via UpMySQL and
// UpMSSQL fields. If a plugin lacks the dialect-specific SQL for the
// active backend, the migration is skipped with a warning (the version
// is recorded so it won't block future migrations). Plugins that are
// inherently PostgreSQL-only (e.g., pgvector) simply leave those fields
// empty and work only with PostgreSQL.
//
// RegisterPluginTables remains PostgreSQL-specific and has not yet been
// made dialect-aware.

// createPluginMigrationsTableSQL returns the dialect-specific SQL for
// creating the plugin_migrations tracking table.
func createPluginMigrationsTableSQL(d Dialect) string {
	switch d {
	case DialectPostgres:
		return `CREATE TABLE IF NOT EXISTS plugin_migrations (
			plugin_name  TEXT NOT NULL,
			version      INTEGER NOT NULL,
			applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (plugin_name, version)
		)`
	case DialectMySQL:
		return `CREATE TABLE IF NOT EXISTS plugin_migrations (
			plugin_name VARCHAR(255) NOT NULL,
			version INTEGER NOT NULL,
			applied_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			PRIMARY KEY (plugin_name, version)
		)`
	case DialectMSSQL:
		return `IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'plugin_migrations')
		CREATE TABLE plugin_migrations (
			plugin_name NVARCHAR(255) NOT NULL,
			version INTEGER NOT NULL,
			applied_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
			PRIMARY KEY (plugin_name, version)
		)`
	default:
		return `CREATE TABLE IF NOT EXISTS plugin_migrations (
			plugin_name  TEXT NOT NULL,
			version      INTEGER NOT NULL,
			applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (plugin_name, version)
		)`
	}
}

// checkPluginMigrationSQL returns the dialect-specific SQL for checking
// whether a plugin migration has already been applied.
func checkPluginMigrationSQL(d Dialect) string {
	switch d {
	case DialectPostgres:
		return `SELECT EXISTS(SELECT 1 FROM plugin_migrations WHERE plugin_name = $1 AND version = $2)`
	case DialectMySQL:
		return `SELECT EXISTS(SELECT 1 FROM plugin_migrations WHERE plugin_name = ? AND version = ?)`
	case DialectMSSQL:
		return `SELECT CASE WHEN EXISTS(SELECT 1 FROM plugin_migrations WHERE plugin_name = @p1 AND version = @p2) THEN 1 ELSE 0 END`
	default:
		return `SELECT EXISTS(SELECT 1 FROM plugin_migrations WHERE plugin_name = $1 AND version = $2)`
	}
}

// insertPluginMigrationSQL returns the dialect-specific SQL for recording
// an applied plugin migration in the tracking table.
func insertPluginMigrationSQL(d Dialect) string {
	switch d {
	case DialectPostgres:
		return `INSERT INTO plugin_migrations (plugin_name, version) VALUES ($1, $2)`
	case DialectMySQL:
		return `INSERT INTO plugin_migrations (plugin_name, version) VALUES (?, ?)`
	case DialectMSSQL:
		return `INSERT INTO plugin_migrations (plugin_name, version) VALUES (@p1, @p2)`
	default:
		return `INSERT INTO plugin_migrations (plugin_name, version) VALUES ($1, $2)`
	}
}

// splitStatements splits SQL text on semicolons, discarding empty fragments.
func splitStatements(sql string) []string {
	parts := strings.Split(sql, ";")
	var stmts []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			stmts = append(stmts, trimmed)
		}
	}
	return stmts
}

// execSQLStatements splits multi-statement SQL and executes each statement
// via the provided exec function.
func execSQLStatements(ctx context.Context, execFn func(ctx context.Context, query string, args ...any) (sql.Result, error), sqlStr string) error {
	for _, stmt := range splitStatements(sqlStr) {
		if _, err := execFn(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// pluginMigrationsLockKey is the pg_advisory_lock key that serialises plugin
// migration runs across processes. Arbitrary, but must never change: it is the
// identity of the lock.
const pluginMigrationsLockKey int64 = 7215842093104562

// lockPluginMigrations takes a database-wide advisory lock for the duration of
// a plugin migration run and returns its release function.
//
// The lock lives on a connection of its own, because a session-level advisory
// lock belongs to the connection that took it and RunMigrations uses the pool
// throughout. That means the pool must allow at least two connections; the
// worker's plugin pool is configured well above that.
//
// PostgreSQL only, deliberately: MySQL and SQL Server have equivalents
// (GET_LOCK, sp_getapplock) but cleat ships no multi-worker topology for them,
// and untested locking would be worse than none. There this is a no-op and
// behaviour is unchanged.
func lockPluginMigrations(ctx context.Context, db *sql.DB, dialect Dialect) (func(), error) {
	if dialect != DialectPostgres {
		return func() {}, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("plugin: acquire migration lock: connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pluginMigrationsLockKey); err != nil {
		conn.Close()
		return nil, fmt.Errorf("plugin: acquire migration lock: %w", err)
	}
	return func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", pluginMigrationsLockKey)
		conn.Close()
	}, nil
}

// RunMigrations runs core migrations and plugin migrations in order.
// Core migrations are run first, then plugins in dependency order.
// Each plugin's migrations are tracked in a plugin_migrations table
// so they run only once.
func RunMigrations(ctx context.Context, db *sql.DB, dialect Dialect, coreMigrations []Migration, plugins []*LoadedPlugin) error {
	if db == nil {
		return nil
	}

	// Serialise against other processes doing the same thing. Every worker
	// runs plugin migrations at boot, and docker-compose.cluster.yml starts
	// four workers at once, so without this they race on the CREATE TABLE IF
	// NOT EXISTS below and on each plugin's own DDL:
	//
	//	plugin: create migrations table: pq: type "plugin_migrations"
	//	already exists (42710)
	//
	// which is fatal to worker startup. Same defect, same shape, and the same
	// fix as migration.Runner -- see migrationsLockKey there. The key differs
	// so that core and plugin migrations do not block each other needlessly.
	unlock, err := lockPluginMigrations(ctx, db, dialect)
	if err != nil {
		return err
	}
	defer unlock()

	// Ensure the plugin_migrations tracking table exists.
	ddl := createPluginMigrationsTableSQL(dialect)
	if err := execSQLStatements(ctx, db.ExecContext, ddl); err != nil {
		return fmt.Errorf("plugin: create migrations table: %w", err)
	}

	// Run core migrations first (caller handles tracking).
	for _, m := range coreMigrations {
		if err := execSQLStatements(ctx, db.ExecContext, m.Up); err != nil {
			return fmt.Errorf("core migration v%d: %w", m.Version, err)
		}
	}

	// Run plugin migrations.
	for _, lp := range plugins {
		if !lp.Healthy {
			continue
		}
		p, ok := lp.Plugin.(HasMigrations)
		if !ok {
			continue
		}

		name := lp.Plugin.Info().Name
		migrations := p.Migrations()
		sort.Slice(migrations, func(i, j int) bool {
			return migrations[i].Version < migrations[j].Version
		})

		for _, m := range migrations {
			// Check if already applied.
			var exists bool
			err := db.QueryRowContext(ctx,
				checkPluginMigrationSQL(dialect),
				name, m.Version).Scan(&exists)
			if err != nil {
				return fmt.Errorf("plugin %s migration v%d check: %w", name, m.Version, err)
			}
			if exists {
				continue
			}

			// Run migration in a transaction.
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("plugin %s migration v%d begin: %w", name, m.Version, err)
			}

			// Select dialect-appropriate SQL.
			sql := m.Up
			switch dialect {
			case DialectMySQL:
				if m.UpMySQL != "" {
					sql = m.UpMySQL
				} else {
					log.Printf("[plugin] %s v%d: no MySQL migration — skipping", name, m.Version)
					if _, err := tx.ExecContext(ctx, insertPluginMigrationSQL(dialect), name, m.Version); err != nil {
						_ = tx.Rollback()
						return fmt.Errorf("plugin %s migration v%d record skip: %w", name, m.Version, err)
					}
					_ = tx.Commit()
					continue
				}
			case DialectMSSQL:
				if m.UpMSSQL != "" {
					sql = m.UpMSSQL
				} else {
					log.Printf("[plugin] %s v%d: no MSSQL migration — skipping", name, m.Version)
					if _, err := tx.ExecContext(ctx, insertPluginMigrationSQL(dialect), name, m.Version); err != nil {
						_ = tx.Rollback()
						return fmt.Errorf("plugin %s migration v%d record skip: %w", name, m.Version, err)
					}
					_ = tx.Commit()
					continue
				}
			}

			// Run the selected migration SQL.
			if err := execSQLStatements(ctx, tx.ExecContext, sql); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("plugin %s migration v%d: %w", name, m.Version, err)
			}

			if _, err := tx.ExecContext(ctx,
				insertPluginMigrationSQL(dialect),
				name, m.Version); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("plugin %s migration v%d record: %w", name, m.Version, err)
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("plugin %s migration v%d commit: %w", name, m.Version, err)
			}
		}
	}

	return nil
}

// RegisterPluginTables inserts entries into admin.plugin_tables so that
// the tenant provisioning system knows which tables to GRANT.
// Called during plugin Init after migrations run.
func RegisterPluginTables(ctx context.Context, db *sql.DB, pluginName string, tableNames []string) error {
	for _, tableName := range tableNames {
		_, err := db.ExecContext(ctx,
			`INSERT INTO admin.plugin_tables (plugin_name, table_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			pluginName, tableName)
		if err != nil {
			return fmt.Errorf("register plugin table %s.%s: %w", pluginName, tableName, err)
		}
	}
	return nil
}
