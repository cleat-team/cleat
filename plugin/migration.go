package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
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

// migrationSession is the subset of *sql.DB and *sql.Conn that RunMigrations
// uses. Both satisfy it, which lets the run pin one connection on PostgreSQL
// while the other dialects keep using the pool.
type migrationSession interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// pluginMigrationSession pins a connection for a plugin migration run and
// returns it with a release function. Two things are set up on it:
//
//   - A database-wide advisory lock, so that only one process migrates at a
//     time. Every worker migrates at boot and docker-compose.cluster.yml
//     starts four at once; without this, three of the four died with
//
//     plugin: create migrations table: pq: type "plugin_migrations"
//     already exists (42710)
//
//     CREATE TABLE IF NOT EXISTS is not atomic against another session
//     creating the same table, so "IF NOT EXISTS" buys nothing here.
//
//   - search_path = public, because plugin DDL is written unqualified
//     (CREATE TABLE kv_store ...). The default search_path is "$user", public
//     and migrations/postgres/001_schema.sql creates a schema named "cleat"
//     while the shipped compose connects as POSTGRES_USER=cleat -- so
//     unqualified DDL landed in the role's schema, not public, on exactly the
//     configuration cleat ships. The core migration files pin this the same
//     way; see the header of 001_schema.sql.
//
// PostgreSQL only, deliberately: MySQL and SQL Server have lock equivalents
// (GET_LOCK, sp_getapplock) and no schema of this shape, but cleat ships no
// multi-worker topology for them and untested code here would be worse than
// none. There this returns the pool unchanged.
func pluginMigrationSession(ctx context.Context, db *sql.DB, dialect Dialect, schema string) (migrationSession, func(), error) {
	if dialect != DialectPostgres {
		return db, func() {}, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin: acquire migration lock: connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pluginMigrationsLockKey); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("plugin: acquire migration lock: %w", err)
	}
	if schema != "public" {
		// Core migrations create it (migration.Runner.session), and they run
		// first at boot -- but plugin migrations are also run directly by
		// tests and by plugins/plugintest, so not depending on that ordering
		// is cheaper than documenting it.
		if _, err := conn.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
			_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pluginMigrationsLockKey)
			conn.Close()
			return nil, nil, fmt.Errorf("plugin: create schema %s: %w", schema, err)
		}
	}
	// pg_temp last, matching migration.Runner.searchPath: PostgreSQL searches
	// pg_temp FIRST when it is not named, and naming it last is the standard
	// hardening. Plugin DDL is unqualified, so this is what decides where
	// every plugin table lands.
	if _, err := conn.ExecContext(ctx, "SET search_path = "+schema+", pg_temp"); err != nil {
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pluginMigrationsLockKey)
		conn.Close()
		return nil, nil, fmt.Errorf("plugin: pin search_path to %s: %w", schema, err)
	}
	return conn, func() {
		// Reset before releasing so the connection returns to the pool
		// configured like every other one.
		free := context.WithoutCancel(ctx)
		// *sql.Conn.Close() RETURNS the connection to the pool rather than
		// closing it, so the pin above would otherwise ride back into the pool
		// and leave one connection resolving unqualified names differently
		// from every other one in it.
		_, _ = conn.ExecContext(free, "RESET search_path")
		_, _ = conn.ExecContext(free, "SELECT pg_advisory_unlock($1)", pluginMigrationsLockKey)
		conn.Close()
	}, nil
}

// MigrationOption configures a RunMigrations call.
type MigrationOption func(*migrationOptions)

type migrationOptions struct {
	schema string
}

// WithSchema directs plugin migrations at a PostgreSQL schema other than
// public, which is what cmd/cleat-worker's --schema flag names. cleat#1287.
//
// It mirrors migration.Runner.WithSchema, and for the same reason: until
// #1287 the schema was a literal here too -- pluginMigrationSession pinned
// `SET search_path = public` unconditionally, with no reference to any
// configuration. Core migrations stopped doing that in #1353; this is the
// other half, and without it a non-default --schema puts core tables in the
// configured schema and plugin tables in public, where the plugin's own
// queries -- unqualified, on a runtime pool whose DSN carries search_path --
// cannot see them.
//
// An empty schema, or "public", leaves the behaviour exactly as it was.
//
// A variadic option rather than a parameter because RunMigrations has some
// forty call sites, all but two of them tests that want the default. The cost
// of that choice is that forgetting the call is silent, and what it silently
// restores is this bug -- so cmd/cleat-worker has a guard test asserting every
// call there passes it, in the same shape as the one #1353 added for
// migration.NewRunner.
func WithSchema(schema string) MigrationOption {
	return func(o *migrationOptions) { o.schema = schema }
}

// plainIdentifier matches a PostgreSQL identifier that needs no quoting.
//
// Declared here as well as in the migration package: the two are peers, not a
// shared layer, and a dependency between them exists only to avoid four tokens
// of regexp.
var plainIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RunMigrations runs core migrations and plugin migrations in order.
// Core migrations are run first, then plugins in dependency order.
// Each plugin's migrations are tracked in a plugin_migrations table
// so they run only once.
func RunMigrations(ctx context.Context, db *sql.DB, dialect Dialect, coreMigrations []Migration, plugins []*LoadedPlugin, opts ...MigrationOption) error {
	if db == nil {
		return nil
	}

	var cfg migrationOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.schema == "" {
		cfg.schema = "public"
	}
	if !plainIdentifier.MatchString(cfg.schema) {
		return fmt.Errorf(
			"plugin: schema %q is not a plain identifier; --schema takes an "+
				"unquoted PostgreSQL schema name", cfg.schema)
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
	session, release, err := pluginMigrationSession(ctx, db, dialect, cfg.schema)
	if err != nil {
		return err
	}
	defer release()

	// Ensure the plugin_migrations tracking table exists.
	ddl := createPluginMigrationsTableSQL(dialect)
	if err := execSQLStatements(ctx, session.ExecContext, ddl); err != nil {
		return fmt.Errorf("plugin: create migrations table: %w", err)
	}

	// Run core migrations first (caller handles tracking).
	for _, m := range coreMigrations {
		if err := execSQLStatements(ctx, session.ExecContext, m.Up); err != nil {
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
			err := session.QueryRowContext(ctx,
				checkPluginMigrationSQL(dialect),
				name, m.Version).Scan(&exists)
			if err != nil {
				return fmt.Errorf("plugin %s migration v%d check: %w", name, m.Version, err)
			}
			if exists {
				continue
			}

			// Run migration in a transaction.
			tx, err := session.BeginTx(ctx, nil)
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

			// Tenant isolation for the tables this migration declared. In
			// the same transaction as the CREATE TABLE, so a table is never
			// briefly visible without its policy.
			if err := applyTenantScoping(ctx, tx.ExecContext, dialect, m.TenantScoped); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("plugin %s migration v%d tenant scoping: %w", name, m.Version, err)
			}

			// And record them, in the same transaction and for the same
			// reason: admin.drop_tenant reads admin.plugin_tables to find
			// the plugin tables a dropped tenant owns rows in, so a table
			// that has a policy but no registry row is one whose rows
			// survive the tenant that owns them -- unreadable, because the
			// policy names a tenant that no longer exists, and undeleted.
			// cleat#1289.
			//
			// This is what admin.plugin_tables was built for in
			// 001_schema.sql and never got: RegisterPluginTables has
			// existed to fill it since then with no production caller, and
			// its doc still describes a call from plugin Init that does not
			// happen. TenantScoped is the declaration that is actually
			// maintained, because a policy depends on it, so it is the one
			// worth deriving from.
			if err := registerTenantScopedTables(ctx, tx.ExecContext, dialect, cfg.schema, name, m.TenantScoped); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("plugin %s migration v%d register tables: %w", name, m.Version, err)
			}

			if _, err := tx.ExecContext(ctx,
				insertPluginMigrationSQL(dialect),
				name, m.Version); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("plugin %s migration v%d record: %w", name, m.Version, err)
			}

			// Restore the pin before the next migration. A bare SET inside a
			// plugin's own SQL is session-scoped, so it would outlive this
			// transaction and decide where every later plugin's tables land --
			// and the plugin_migrations bookkeeping is unqualified, so it
			// would move too. The core runner does the same after each file
			// and for the same reason; see migration/runner.go.
			if dialect == DialectPostgres {
				if _, err := tx.ExecContext(ctx,
					"SET search_path = "+cfg.schema+", pg_temp"); err != nil {
					_ = tx.Rollback()
					return fmt.Errorf("plugin %s migration v%d restore search_path: %w",
						name, m.Version, err)
				}
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("plugin %s migration v%d commit: %w", name, m.Version, err)
			}
		}
	}

	return nil
}

// RegisterPluginTables inserts entries into admin.plugin_tables so that
// admin.grant_plugin_to_tenant and admin.revoke_plugin_from_tenant know which
// tables to GRANT on.
//
// It has no production caller. The line that used to sit here -- "Called
// during plugin Init after migrations run" -- described a call that has never
// existed, which is how a registry with no producer read as a registry that
// was being filled. cleat#1277 records the same about admin.plugin_tables
// itself.
//
// Registration for TENANT DELETION is separate and does happen:
// registerTenantScopedTables writes the tables a migration declares
// TenantScoped, in the migration's own transaction, and admin.drop_tenant
// reads those rows. That path derives from a declaration something else
// already depends on, rather than from a call someone has to remember.
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

// applyTenantScoping enables row-level security on each declared table and
// installs a policy filtering it on the statement's tenant. cleat#1277.
//
// The predicate is cleat.assert_tenant_set(), the same one every engine table
// uses, and it RAISES rather than filtering when no tenant is set. That is the
// deliberate choice: a policy that silently returns no rows turns a missing
// tenant context into an empty result, which reads as "no data" and is exactly
// the failure this is meant to prevent. An exception names the problem.
//
// FORCE is required as well as ENABLE. Without it row-level security is
// silently bypassed for the table's owner, which is whoever ran the migration
// -- see the comment above the engine's own FORCE block in 001_schema.sql.
//
// Non-PostgreSQL dialects return nil without doing anything; see the
// TenantScoped field for why, and note that it is a real gap rather than an
// omission.
func applyTenantScoping(ctx context.Context, exec func(ctx context.Context, query string, args ...any) (sql.Result, error), dialect Dialect, tables []string) error {
	if len(tables) == 0 || dialect != DialectPostgres {
		return nil
	}
	for _, table := range tables {
		if !isPlainIdentifier(table) {
			return fmt.Errorf("tenant-scoped table %q is not a plain identifier", table)
		}
		policy := table + "_tenant_isolation"
		for _, stmt := range []string{
			fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", table),
			fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", table),
			fmt.Sprintf("DROP POLICY IF EXISTS %s ON %s", policy, table),
			// cleat.tenant_row_is_visible rather than the inline
			// `tenant_id = cleat.assert_tenant_set()` this used to emit.
			// Same answer when no bypass is named, including the RAISE on an
			// unset tenant; it additionally admits a sweep that named itself
			// through plugin.AcrossAllTenants. Defined in
			// migrations/postgres/063, which is a CORE migration and so has
			// already run by the time any plugin migration does --
			// cmd/cleat-worker/main.go runs migration.Runner at :680 and
			// plugin.RunMigrations at :686. cleat#1278.
			fmt.Sprintf("CREATE POLICY %s ON %s FOR ALL USING (cleat.tenant_row_is_visible(tenant_id))",
				policy, table),
		} {
			if _, err := exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
	}
	return nil
}

// registerTenantScopedTables records each tenant-scoped table in
// admin.plugin_tables, so admin.drop_tenant can find it.
//
// PostgreSQL only, matching applyTenantScoping: on the other two dialects
// TenantScoped installs no policy and admin.plugin_tables does not exist, so
// there is nothing to register and nothing that would read it.
//
// The schema is recorded alongside the name because --schema puts plugin
// tables somewhere other than public while admin.plugin_tables stays in the
// admin schema, and because admin.drop_tenant carries no search_path of its
// own (cleat#1363) -- an unqualified name there would resolve against the
// caller's.
//
// ON CONFLICT ... DO UPDATE rather than DO NOTHING: a plugin that adds
// TenantScoped to a table in a later migration version must be able to flip
// an existing row, which a registration made for GRANT purposes would
// otherwise pin at false.
func registerTenantScopedTables(ctx context.Context, exec func(ctx context.Context, query string, args ...any) (sql.Result, error), dialect Dialect, schema, pluginName string, tables []string) error {
	if len(tables) == 0 || dialect != DialectPostgres {
		return nil
	}
	for _, table := range tables {
		if !isPlainIdentifier(table) {
			return fmt.Errorf("tenant-scoped table %q is not a plain identifier", table)
		}
		if _, err := exec(ctx, `INSERT INTO admin.plugin_tables (plugin_name, schema_name, table_name, tenant_scoped)
			VALUES ($1, $2, $3, true)
			ON CONFLICT (plugin_name, schema_name, table_name) DO UPDATE SET tenant_scoped = true`,
			pluginName, schema, table); err != nil {
			return fmt.Errorf("register %s.%s: %w", schema, table, err)
		}
	}
	return nil
}

// isPlainIdentifier reports whether s is safe to interpolate into DDL.
//
// The table names come from plugin source rather than from a request, so this
// is not the primary defence against injection -- it is there so that a
// mistake in a plugin becomes a migration error naming the table, instead of
// arbitrary DDL executing under the migration's privileges.
func isPlainIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
