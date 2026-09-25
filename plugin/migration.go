package plugin

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cleat-team/cleat/internal/pinnedtx"
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
// MySQL and SQL Server are serialised too (pluginLockedSession). This comment used
// to say they were not, deliberately -- "cleat ships no multi-worker topology for
// them" -- and cleat#2117 measured what that cost: concurrent migrators on an
// empty database failed three in four on both (see migration/runner.go), and
// migration is now a deploy step run alongside straggling workers.
func pluginMigrationSession(ctx context.Context, db *sql.DB, dialect Dialect, schema string) (migrationSession, func(), error) {
	if dialect != DialectPostgres {
		return pluginLockedSession(ctx, db, dialect)
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
	// Bound the wait for every lock the plugin migrations take, exactly as
	// migration.Runner.session does for the core ones. cleat#1775.
	//
	// THIS FILE IS THE SECOND MIGRATION RUNNER, and it is the reason the bound
	// is here as well as there. Plugin migrations run DDL against tables the
	// worker also serves, on the same boot path, with the same consequence if
	// they block: the worker stops heartbeating and the reaper starts
	// collecting its runs. Bounding one runner and not the other would leave
	// the property true of the instance and false of the class, which is the
	// shape cleat#1769 was filed about.
	//
	// A LITERAL, not migration.DefaultLockTimeout, and the reason is narrower
	// than "import cycle" -- it took two tries to state correctly.
	// `go list -deps` shows neither package importing the other, and
	// `go build ./...` with the import succeeds. `go vet` does not:
	//
	//   package cleat/migration
	//     imports cleat/engine from internal_test.go
	//     imports cleat/plugin from app.go
	//
	// The cycle exists only in migration's TEST build, which is invisible to
	// `go build` and is why the first check said the import was fine.
	//
	// Two runners bounded by two literals is how they drift, so
	// TestBothMigrationRunnersBoundTheirLockWait reads both files and fails if
	// the numbers stop agreeing.
	if _, err := conn.ExecContext(ctx, "SET lock_timeout = 30000"); err != nil {
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pluginMigrationsLockKey)
		conn.Close()
		return nil, nil, fmt.Errorf("plugin: set lock_timeout: %w", err)
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
		// And the lock bound, for the same reason as the pin above it.
		_, _ = conn.ExecContext(free, "RESET lock_timeout")
		_, _ = conn.ExecContext(free, "SELECT pg_advisory_unlock($1)", pluginMigrationsLockKey)
		conn.Close()
	}, nil
}

// PluginSchemaState is what VerifyMigrations found.
type PluginSchemaState struct {
	// TableMissing means plugin_migrations does not exist. That is only a finding
	// if some healthy plugin ships a migration: with none, nothing needs it.
	TableMissing bool
	// Pending are "plugin vN" for every shipped migration of a healthy plugin that is
	// not recorded as applied.
	Pending []string
}

// Behind reports whether a plugin migration is not applied.
func (s PluginSchemaState) Behind() bool { return len(s.Pending) > 0 }

// PluginSchemaBehindError is what a worker start reports when Behind; the message is
// the remediation, for the same reason as migration.SchemaBehindError.
type PluginSchemaBehindError struct{ State PluginSchemaState }

func (e *PluginSchemaBehindError) Error() string {
	shown := strings.Join(e.State.Pending, ", ")
	if len(e.State.Pending) > 4 {
		shown = strings.Join(e.State.Pending[:4], ", ") + fmt.Sprintf(", ... (%d in all)", len(e.State.Pending))
	}
	return "the database's plugin schema is behind this worker: plugin migration(s) not applied: " + shown + ".\n" +
		"A worker does not migrate the database on start. Run the migrations as a deploy step:\n\n" +
		"    cleat-worker --migrate-only --db <dsn> [--migrate-db <owner dsn>]\n\n" +
		"and then start the workers. (--migrate-on-start restores the old behaviour for a single-node install.)"
}

// VerifyMigrations reports whether every migration the healthy plugins ship is
// recorded as applied, WITHOUT changing anything: no table is created, no lock taken,
// nothing run. It is the plugin half of a normal worker start (cleat#2117); the same
// healthy-plugin and HasMigrations rules as RunMigrations decide which migrations count.
//
// A migration with no arm for this dialect is recorded as applied when skipped
// (see RunMigrations), so it is not reported pending here either.
func VerifyMigrations(ctx context.Context, db *sql.DB, dialect Dialect, plugins []*LoadedPlugin, opts ...MigrationOption) (PluginSchemaState, error) {
	var st PluginSchemaState
	if db == nil {
		return st, nil
	}
	var cfg migrationOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.schema == "" {
		cfg.schema = "public"
	}
	if !plainIdentifier.MatchString(cfg.schema) {
		return st, fmt.Errorf("plugin: schema %q is not a plain identifier", cfg.schema)
	}

	type shipped struct {
		name    string
		version int
	}
	var want []shipped
	for _, lp := range plugins {
		if !lp.Healthy {
			continue
		}
		p, ok := lp.Plugin.(HasMigrations)
		if !ok {
			continue
		}
		name := lp.Plugin.Info().Name
		for _, m := range p.Migrations() {
			want = append(want, shipped{name, m.Version})
		}
	}
	if len(want) == 0 {
		return st, nil
	}

	// One connection, so the search_path pin (PostgreSQL) covers the table lookup and
	// the reads that follow it, and is reset before the connection goes back to the pool.
	conn, err := db.Conn(ctx)
	if err != nil {
		return st, fmt.Errorf("plugin: verify migrations: connection: %w", err)
	}
	defer conn.Close()
	if dialect == DialectPostgres {
		if _, err := conn.ExecContext(ctx, "SET search_path = "+cfg.schema+", pg_temp"); err != nil {
			return st, fmt.Errorf("plugin: verify migrations: pin search_path: %w", err)
		}
		defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "RESET search_path") }()
	}

	var n int
	switch dialect {
	case DialectMySQL:
		err = conn.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables "+
			"WHERE table_schema = DATABASE() AND table_name = 'plugin_migrations'").Scan(&n)
	case DialectMSSQL:
		err = conn.QueryRowContext(ctx, "SELECT count(*) FROM sys.tables WHERE name = 'plugin_migrations'").Scan(&n)
	default:
		err = conn.QueryRowContext(ctx, "SELECT CASE WHEN to_regclass('plugin_migrations') IS NULL THEN 0 ELSE 1 END").Scan(&n)
	}
	if err != nil {
		return st, fmt.Errorf("plugin: verify migrations: check for plugin_migrations: %w", err)
	}
	if n == 0 {
		st.TableMissing = true
		for _, w := range want {
			st.Pending = append(st.Pending, fmt.Sprintf("%s v%d", w.name, w.version))
		}
		return st, nil
	}

	for _, w := range want {
		var exists bool
		if err := conn.QueryRowContext(ctx, checkPluginMigrationSQL(dialect), w.name, w.version).Scan(&exists); err != nil {
			return st, fmt.Errorf("plugin: verify migrations: %s v%d: %w", w.name, w.version, err)
		}
		if !exists {
			st.Pending = append(st.Pending, fmt.Sprintf("%s v%d", w.name, w.version))
		}
	}
	return st, nil
}

// The plugin migration lock on MySQL and SQL Server. Same mechanism and the same
// reasons as migration.Runner.lockedSession, which is the one to read: a
// SESSION-scoped lock, so the connection is pinned and the release runs on it in a
// defer, and a release that fails discards the connection so the lock cannot
// outlive its holder. Duplicated rather than shared because migration's test
// build imports this package, and the two names differ so core and plugin
// migrations do not queue behind each other needlessly.
const (
	pluginMySQLLockPrefix = "cleat.plugin_migrations."
	pluginMSSQLLock       = "cleat.plugin_migrations"
	pluginLockWait        = 15 * time.Minute
)

func pluginLockedSession(ctx context.Context, db *sql.DB, dialect Dialect) (migrationSession, func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin: acquire migration lock: connection: %w", err)
	}
	secs := int(pluginLockWait / time.Second)
	var release string
	switch dialect {
	case DialectMySQL:
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx,
			"SELECT GET_LOCK(CONCAT('"+pluginMySQLLockPrefix+"', MD5(DATABASE())), ?)", secs).Scan(&got); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("plugin: acquire migration lock: GET_LOCK: %w", err)
		}
		if !got.Valid || got.Int64 != 1 {
			conn.Close()
			return nil, nil, fmt.Errorf("plugin: acquire migration lock: held elsewhere for %s "+
				"(GET_LOCK returned %v); is a --migrate-only run stuck?", pluginLockWait, got)
		}
		release = "SELECT RELEASE_LOCK(CONCAT('" + pluginMySQLLockPrefix + "', MD5(DATABASE())))"
	case DialectMSSQL:
		var code int
		if err := conn.QueryRowContext(ctx,
			"DECLARE @r int; EXEC @r = sp_getapplock @Resource = N'"+pluginMSSQLLock+
				"', @LockMode = N'Exclusive', @LockOwner = N'Session', @LockTimeout = @p1; SELECT @r",
			secs*1000).Scan(&code); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("plugin: acquire migration lock: sp_getapplock: %w", err)
		}
		if code < 0 {
			conn.Close()
			return nil, nil, fmt.Errorf("plugin: acquire migration lock: sp_getapplock returned %d "+
				"(-1 = timed out after %s: is a --migrate-only run stuck?)", code, pluginLockWait)
		}
		release = "DECLARE @r int; EXEC @r = sp_releaseapplock @Resource = N'" + pluginMSSQLLock +
			"', @LockOwner = N'Session'; SELECT @r"
	default:
		conn.Close()
		return nil, nil, fmt.Errorf("plugin: unsupported dialect %q", dialect)
	}
	return conn, func() {
		free := context.WithoutCancel(ctx)
		var out sql.NullInt64
		if err := conn.QueryRowContext(free, release).Scan(&out); err != nil ||
			(dialect == DialectMySQL && (!out.Valid || out.Int64 != 1)) ||
			(dialect == DialectMSSQL && out.Int64 < 0) {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
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
			// pinnedtx.Begin: see its package comment (cleat#2215).
			tx, err := pinnedtx.Begin(ctx, session, nil)
			if err != nil {
				return fmt.Errorf("plugin %s migration v%d begin: %w", name, m.Version, err)
			}
			// A safety net, not the mechanism: every path below rolls back or commits explicitly, and
			// Rollback after Commit is a no-op (sql.ErrTxDone). It is here because pinnedtx.Begin starts
			// this transaction on a context that outlives ctx, so a path that forgets to end it no longer
			// gets cleaned up by cancellation -- it leaves an open transaction on the pinned connection and
			// the next statement waits behind it. Measured by removing one explicit Rollback: the plugin
			// suite hung to its timeout (cleat#2215 review).
			defer func() { _ = tx.Rollback() }()

			// A migration may carry NO SQL AT ALL and exist only to declare
			// something -- TenantScoped, SweepTables. cleat#1512 established
			// that idiom and slacknotify's v2 states the reason: a recorded
			// migration never runs again, so protecting an EXISTING database
			// means adding a new version rather than editing v1, and that new
			// version has nothing to execute.
			//
			// The dialect skip below is for a migration whose SQL does not
			// port. A declaration-only migration has no SQL that could fail to
			// port, and skipping it records the version as applied while
			// installing nothing -- so the table never gets another chance.
			//
			// Measured on a freshly migrated SQL Server database while adding
			// applyTenantScopingMSSQL: 25 declared tables, 2 policies. The
			// other 23 are covered by 17 declaration-only migrations, every one
			// of which took this branch. cleat#1552.
			//
			// This changes nothing on MySQL beyond removing a misleading log
			// line: falling through runs an empty statement list, and all three
			// of the declaration helpers below are no-ops there.
			//
			// WHAT THE SKIP BELOW ALSO DROPS, which this comment did not say
			// and which is not obvious from reading it: the skip `continue`s,
			// so applyTenantScoping, grantSweepTables and
			// registerTenantScopedTables are ALL bypassed for that migration on
			// that dialect. A migration carrying SQL *and* a TenantScoped
			// declaration therefore loses its POLICY there, not just its DDL --
			// and it records the version as applied, so it never gets another
			// chance. The only trace is the log line.
			//
			// That is reachable in principle and not in practice today, and the
			// difference is worth stating rather than trusting. Exactly one
			// migration in the tree has the shape (SQL, a TenantScoped
			// declaration, and a missing dialect arm): pgvector's v1, declaring
			// pgvector_embeddings with an Up arm only. pgvector is deliberately
			// not linked into cleat-worker -- see the import block in
			// cmd/cleat-worker/main.go, which explains that its
			// `embedding vector(1536)` column is fatal on a PostgreSQL without
			// the extension -- so it runs on no dialect there at all.
			//
			// And the entry to the hazard is guarded rather than merely
			// unoccupied. TestEveryLinkedPluginSupportsEveryDialectTheWorker-
			// RunsOn reports a LINKED plugin whose migration lacks UpMySQL or
			// UpMSSQL, naming the plugin and version -- and its two exemptions
			// (a declaration-only migration, and one marked DialectSpecific)
			// BOTH require an empty Up, which this shape by definition does not
			// have. So linking pgvector, or any plugin reaching this shape,
			// goes red before it can lose a policy silently.
			//
			// Note what that does and does not cover: a plugin nobody links is
			// checked by nothing here, which is exactly why the population
			// below is worth re-deriving rather than trusting this paragraph:
			//
			//	git grep -n 'TenantScoped:' -- plugins/
			//
			// and check each hit's Migration literal for UpMySQL and UpMSSQL.
			declarationOnly := m.Up == "" && m.UpMySQL == "" && m.UpMSSQL == ""

			// Select dialect-appropriate SQL.
			sql := m.Up
			switch dialect {
			case DialectMySQL:
				if m.UpMySQL != "" {
					sql = m.UpMySQL
				} else if !declarationOnly {
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
				} else if !declarationOnly {
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

			// And the grants for tables a sweep touches but which carry no
			// tenant column. Same transaction, same reason: a sweep must never
			// observe a state where its table exists and it cannot reach it.
			if err := grantSweepTables(ctx, tx.ExecContext, dialect, m.SweepTables); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("plugin %s migration v%d sweep grants: %w", name, m.Version, err)
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
// SQL SERVER gets the same protection by a different mechanism; see
// applyTenantScopingMSSQL. MySQL returns nil without doing anything: it has no
// row-level security, and its tenancy is a database boundary rather than a
// policy.
func applyTenantScoping(ctx context.Context, exec func(ctx context.Context, query string, args ...any) (sql.Result, error), dialect Dialect, tables []string) error {
	if len(tables) == 0 {
		return nil
	}
	if dialect == DialectMSSQL {
		return applyTenantScopingMSSQL(ctx, exec, tables)
	}
	if dialect != DialectPostgres {
		return nil
	}
	for _, table := range tables {
		if !isPlainIdentifier(table) {
			return fmt.Errorf("tenant-scoped table %q is not a plain identifier", table)
		}
		policy := table + "_tenant_isolation"
		sweepPolicy := table + "_cross_tenant"
		for _, stmt := range []string{
			fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", table),
			fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", table),
			fmt.Sprintf("DROP POLICY IF EXISTS %s ON %s", policy, table),
			fmt.Sprintf("DROP POLICY IF EXISTS %s ON %s", sweepPolicy, table),
			// TWO policies, not one CASE. Permissive policies are OR-ed, so
			// the tenant predicate stays a plain equality the planner can turn
			// into an Index Cond, and the sweep gets its own policy carrying
			// no tenant predicate at all.
			//
			// This used to emit cleat.tenant_row_is_visible(tenant_id) (a
			// CASE, migration 063), which lands as a Filter rather than an
			// Index Cond: measured at 400000 rows over 400 tenants, 603.654 ms
			// with 399000 rows removed by the filter, against 0.415 ms for the
			// equality. Neither plan is a Seq Scan -- both are Index Only
			// Scans -- so an EXPLAIN grepped for "Seq Scan" shows nothing and
			// reads as already-fixed. cleat#1490.
			fmt.Sprintf("CREATE POLICY %s ON %s FOR ALL TO PUBLIC USING (tenant_id = cleat.assert_tenant_set())",
				policy, table),
			// TO PUBLIC above, deliberately, and not a parent role every
			// application role is granted: admin.create_tenant_role creates
			// per-tenant roles NOINHERIT (001_schema.sql:67), and a NOINHERIT
			// member does not match a `TO <parent>` policy. Measured, such a
			// role reads 0 rows -- no error, just an empty result, which is
			// the failure the RAISE in assert_tenant_set exists to prevent.
			fmt.Sprintf("CREATE POLICY %s ON %s FOR ALL TO cleat_sweep USING (true)",
				sweepPolicy, table),
			// cleat_sweep is entered with SET LOCAL ROLE (see
			// engine/plugindb_tenant.go), so it needs privileges of its own;
			// membership does not lend them.
			fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO cleat_sweep", table),
		} {
			if _, err := exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
	}
	return nil
}

// mssqlPluginTenantFilter is the predicate every plugin policy binds to.
//
// A SEPARATE FUNCTION FROM THE ENGINE'S dbo.fn_tenant_filter, and the reason is
// not tidiness. A SECURITY POLICY holds a hard dependency on the function under
// it, so CREATE OR ALTER on that function fails while any policy references it
// -- migrations/mssql/001_schema.sql has a long header about exactly this, and
// works around it by dropping every policy first, which it can do because it
// owns them all. A plugin migration owns only its own tables and cannot know
// what else is bound to the engine's function. Binding plugin policies to the
// engine's predicate would therefore make migration 012 -- which does
// CREATE OR ALTER dbo.fn_tenant_filter -- fail on any database with plugins
// installed.
const mssqlPluginTenantFilter = "fn_plugin_tenant_filter"

// applyTenantScopingMSSQL installs a security policy per declared table.
// cleat#1552.
//
// THE PREDICATE CARRIES NO ADMIN DISJUNCT. migration 012 added
// `OR IS_ROLEMEMBER(N'cleat_admin') = 1` to the engine's predicate, and #1491
// measured what that costs: a query that does not carry its own tenant equality
// loses its seek and scans. cleat#1541 is removing it. Plugin tables are not
// adopting it on the way past -- the cross-tenant key below serves the same
// purpose, and is tested once per query rather than once per row.
//
// THE SECOND KEY IS THE SWEEP BYPASS, mirroring PostgreSQL's cleat.cross_tenant
// GUC. SQL Server has no SET ROLE, so PostgreSQL's newer `TO cleat_sweep`
// policy has no counterpart; engine/plugindb_tenant.go's markCrossTenantOnTx
// sets this key for the life of one transaction. It is emphatically NOT the
// sentinel 012 refuses: that refuses a magic tenant_id VALUE, which would let
// anything that can call sp_set_session_context assume a particular tenant.
// This key names no tenant, and dbo.fn_tenant_filter does not read it, so it
// cannot widen any engine table.
//
// BLOCK PREDICATES AS WELL AS A FILTER, and this is the half that reading the
// PostgreSQL arm does not suggest. A FILTER PREDICATE hides rows from reads; it
// does not refuse writes. Measured against SQL Server 2022 on a table carrying
// only a filter: an INSERT with no session context SUCCEEDS, and the row is
// then invisible to the writer.
//
// PostgreSQL does not have that hole, because `FOR ALL ... USING` defaults its
// WITH CHECK to the USING expression. Measured on the shipped shape as a
// NOSUPERUSER NOBYPASSRLS role -- a first attempt measured it as a SUPERUSER,
// which bypasses RLS unconditionally and duly reported every write as
// permitted, which is what that fixture was always going to say:
//
//	                                        postgres        sql server
//	                                                    filter   filter+block
//	INSERT own tenant's row                 ok          ok       ok
//	INSERT another tenant's row             REFUSED     ok (!)   REFUSED
//	UPDATE moving a row to another tenant   REFUSED     ok (!)   REFUSED
//	INSERT with no tenant set               REFUSED     ok (!)   REFUSED
//
// AFTER INSERT and AFTER UPDATE are the two that matter. BEFORE UPDATE and
// BEFORE DELETE would be redundant: the filter predicate already hides another
// tenant's rows, so there is nothing for them to match.
//
// THE TABLE MUST BE TWO-PART QUALIFIED. `ON <table>` fails with "Cannot schema
// bind security policy ... Names must be in two-part format", so this emits
// `ON dbo.<table>`. WithSchema is PostgreSQL-only, so dbo is where a plugin's
// CREATE TABLE put them.
//
// WHAT IT DOES NOT MATCH: PostgreSQL's cleat.assert_tenant_set() RAISES when no
// tenant is set, so a statement that forgot one fails loudly. A SQL Server
// filter predicate must be an inline table-valued function, which has no
// procedural body to raise from -- BEGIN ... THROW is a syntax error there. So
// a READ with no tenant returns an empty result rather than an error. Writes
// are covered by the block predicates above; reads are not, and that asymmetry
// is real.
//
// AND A DELETE IS A READ FOR THIS PURPOSE, which this comment did not say and
// is the sharper half. The filter predicate hides rows from DELETE exactly as
// it hides them from SELECT, so a DELETE issued with no tenant key removes
// nothing and reports success -- "(0 rows affected)", which is the one outcome
// indistinguishable from "already clean". Measured as sa with
// IS_SRVROLEMEMBER('sysadmin') = 1, because privilege is not what gets you
// past a security policy on this dialect. The block predicates above do not
// help: they refuse a write that names the WRONG tenant, and this write names
// the right one on a connection that has not said who it is.
//
// That is what made a hand-written tenant cleanup silently do nothing, and it
// is why migrations/mssql/074's admin.drop_tenant sets the tenant key before
// its first DELETE. cleat#1635, and
// TestADeleteWithoutTheTenantKeyRemovesNothingOnSQLServer pins the behaviour
// so a change in it is visible. cleat#1552 carries the options that were measured and rejected,
// including a CONVERT trip-wire whose message SQL Server redacts inside a
// security predicate.
func applyTenantScopingMSSQL(ctx context.Context, exec func(ctx context.Context, query string, args ...any) (sql.Result, error), tables []string) error {
	// Guarded creation rather than CREATE OR ALTER, for the dependency reason
	// above: once a policy binds this function it cannot be altered, and it
	// never needs to be. Re-running is a no-op, measured.
	createFilter := fmt.Sprintf(`IF OBJECT_ID('dbo.%[1]s', 'IF') IS NULL
	EXEC('CREATE FUNCTION dbo.%[1]s(@tenant_id UNIQUEIDENTIFIER)
	      RETURNS TABLE WITH SCHEMABINDING AS
	      RETURN SELECT 1 AS access
	      WHERE @tenant_id = CAST(SESSION_CONTEXT(N''tenant_id'') AS UNIQUEIDENTIFIER)
	         OR CAST(SESSION_CONTEXT(N''cross_tenant'') AS NVARCHAR(4000)) <> N''''')`,
		mssqlPluginTenantFilter)
	if _, err := exec(ctx, createFilter); err != nil {
		return fmt.Errorf("create dbo.%s: %w", mssqlPluginTenantFilter, err)
	}

	for _, table := range tables {
		if !isPlainIdentifier(table) {
			return fmt.Errorf("tenant-scoped table %q is not a plain identifier", table)
		}
		policy := table + "_tenant_isolation"
		stmt := fmt.Sprintf(`IF NOT EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'%[1]s')
	CREATE SECURITY POLICY %[1]s
		ADD FILTER PREDICATE dbo.%[2]s(tenant_id) ON dbo.%[3]s,
		ADD BLOCK PREDICATE dbo.%[2]s(tenant_id) ON dbo.%[3]s AFTER INSERT,
		ADD BLOCK PREDICATE dbo.%[2]s(tenant_id) ON dbo.%[3]s AFTER UPDATE
	WITH (STATE = ON)`, policy, mssqlPluginTenantFilter, table)
		if _, err := exec(ctx, stmt); err != nil {
			return fmt.Errorf("create security policy %s: %w", policy, err)
		}
	}
	return nil
}

// grantSweepTables gives cleat_sweep privileges on tables a cross-tenant sweep
// touches that carry no tenant column, declared in Migration.SweepTables.
//
// WHY A GRANT AND NOT A POLICY. These tables have no tenant_id, so there is
// nothing for a policy to filter on; what they need is for the role a sweep
// runs as to be able to reach them at all. beginTenantTx issues
// SET LOCAL ROLE cleat_sweep for a plugin.AcrossAllTenants transaction
// (cleat#1490), and a role switch changes privileges for EVERY table in the
// transaction -- so a sweep that deletes from an unscoped table gets
// "permission denied for table X (42501)" unless it is named here.
//
// Measured when this was found: 2 of 24 plugin packages needed it --
// blobstore (blob_index scoped, blob_content not) and notifications
// (webhook_config scoped, webhook_delivery not). The other 22 sweeps touch
// only tables they had already declared.
//
// Non-PostgreSQL dialects return nil: no role is switched there, so no grant
// is required.
func grantSweepTables(ctx context.Context, exec func(ctx context.Context, query string, args ...any) (sql.Result, error), dialect Dialect, tables []string) error {
	if len(tables) == 0 || dialect != DialectPostgres {
		return nil
	}
	for _, table := range tables {
		if !isPlainIdentifier(table) {
			return fmt.Errorf("sweep table %q is not a plain identifier", table)
		}
		stmt := fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO cleat_sweep", table)
		if _, err := exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// registerTenantScopedTables records each tenant-scoped table in
// admin.plugin_tables, so admin.drop_tenant can find it.
//
// PostgreSQL only. This USED to say "matching applyTenantScoping", and since
// cleat#1552 that is no longer true: SQL Server gets a policy and no registry
// row. MySQL gets neither, and needs neither.
//
// THE REASON GIVEN HERE WAS WRONG, and it is corrected rather than quietly
// replaced because it was written confidently. This comment said
// admin.plugin_tables "does not exist" on SQL Server. It has existed since
// migrations/mssql/001_schema.sql:117; what is true is narrower -- it carries
// the pre-066 two-column shape, with no schema_name and no tenant_scoped, and
// nothing on that dialect has ever written a row to it.
//
// THE GAP THIS COMMENT DESCRIBED IS CLOSED, and closed without a registry.
// migrations/mssql/074 defines admin.drop_tenant there, and it finds
// tenant-owned tables by asking sys.columns which ones carry a tenant_id
// column -- which reaches core tables and plugin tables in one query, because
// on SQL Server both live in dbo (WithSchema is PostgreSQL-only). So there is
// nothing for a SQL Server arm of this function to do: a registry would be a
// second thing to keep in step with a question the catalogue already answers,
// and PostgreSQL needs one only because --schema can put its plugin tables in
// a schema the procedure would otherwise have to guess. cleat#1635.
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

// unregisterTenantScopedTables removes the rows registerTenantScopedTables
// wrote when the plugin's migrations were applied, so a full uninstall leaves
// admin.plugin_tables without a stale row naming a table that no longer
// exists. PostgreSQL only, mirroring registerTenantScopedTables: MySQL and
// SQL Server never populate the table (see that function's own comment).
//
// cleat#2343: RunDownMigrations never touched admin.plugin_tables, so the
// registry survived a full uninstall unchanged, and admin.drop_tenant /
// admin.grant_plugin_to_tenant -- which read plugin_tables to find
// tenant-owned rows per table -- would see a stale row naming a dropped table.
func unregisterTenantScopedTables(ctx context.Context, exec func(ctx context.Context, query string, args ...any) (sql.Result, error), dialect Dialect, schema, pluginName string, tables []string) error {
	if len(tables) == 0 || dialect != DialectPostgres {
		return nil
	}
	for _, table := range tables {
		if !isPlainIdentifier(table) {
			return fmt.Errorf("tenant-scoped table %q is not a plain identifier", table)
		}
		if _, err := exec(ctx, `DELETE FROM admin.plugin_tables
			WHERE plugin_name = $1 AND schema_name = $2 AND table_name = $3`,
			pluginName, schema, table); err != nil {
			return fmt.Errorf("unregister %s.%s: %w", schema, table, err)
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
