package testutil

// This file is the seam between engine/testutil (which every dialect's tests
// bootstrap through) and migration.Runner (what every cleat-worker actually
// executes at boot, cmd/cleat-worker/main.go). Before this file existed, each
// dialect had its own path to a schema:
//
//   - PostgreSQL read a curated, hand-maintained list of migration files
//     (schema.go's old postgresSchemaFiles) rather than the whole directory,
//     and the list silently fell one migration behind twice: it never
//     gained 023/024 until they were added by hand, and it was still missing
//     031_rls_gap_concurrency_and_update_requests.sql when this file was
//     written -- so every PostgreSQL test ran without the RLS policies that
//     migration adds to concurrency_keys and workflow_update_requests.
//   - SQL Server read a similar curated list (mssql_schema.go's old
//     mssqlSchemaFiles) which had fallen behind in the same way: it was
//     missing 021_schedule_timezone.sql, 022_schedule_policies.sql, and
//     031_workflow_promises_security_policy.sql, papering over the first two
//     with a hand-duplicated ALTER TABLE (migrateMSSQLWorkflowDefsTenantID)
//     and never wiring the third in at all -- so no MSSQL test could observe
//     workflow_promises' tenant policy existing or failing to.
//   - MySQL had no connection to the shipped files whatsoever: mysql_schema.go
//     was a second, hand-written schema, missing entire tables the shipped
//     one has (tenants, tenant_roles, workflow_tags, workflow_routing,
//     plugin_tables) and disagreeing on nullability for the three columns
//     migration 030 exists to fix. Every MySQL test in the repo ran against a
//     schema production never uses.
//
// Routing every dialect through the same Runner the worker itself runs
// removes the class rather than the individual instances: a new migration
// file is picked up the moment it is added to migrations/<dialect>/, because
// nothing here names files by hand.
import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// migrationsRoot returns the repo's migrations/ directory (the Runner
// appends the dialect subdirectory itself).
//
// Computed from this source file's own location via runtime.Caller rather
// than a hardcoded relative path, for the reason schema.go's old
// postgresSchemaFiles gave: this package is exercised from more than one
// `go test` working directory (engine/, which imports testutil, and
// engine/testutil/ itself), and a single ".."-relative path cannot be
// correct for both.
func migrationsRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("migrationsRoot: runtime.Caller failed")
	}
	// thisFile is .../engine/testutil/migrations.go; the repo root is two
	// levels up, and the migrations live under migrations/.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

// toMigrationDialect converts a testutil.Dialect to the migration.Dialect
// that migration.NewRunner takes. The two types carry identical string
// values ("postgres", "mysql", "mssql") but are declared separately, and
// deliberately so: migration.Dialect exists specifically so this package
// does not need engine.Dialect. engine (production code) imports plugin
// (engine/app.go, engine/plugin_loader.go and others), and both engine's and
// plugin's own *_test.go files import engine/testutil -- so if this package
// imported anything from engine, building plugin's test binary would need
// testutil, which would need engine, which would need plugin, which is the
// package already being built: "import cycle not allowed in test". See
// migration.Dialect's doc comment for the fuller version of this.
func toMigrationDialect(d Dialect) migration.Dialect {
	switch d {
	case DialectPostgres:
		return migration.DialectPostgres
	case DialectMySQL:
		return migration.DialectMySQL
	case DialectMSSQL:
		return migration.DialectMSSQL
	default:
		panic("toMigrationDialect: unknown dialect: " + string(d))
	}
}

// applyMigrations runs the real migration.Runner against db for dialect,
// applying every pending file in migrations/<dialect>/ in version order --
// the exact code path cmd/cleat-worker/main.go executes at boot. There is no
// curated file list here on purpose: see this file's header.
//
// Idempotent and safe to call once per Setup*Schema entry point. The Runner
// tracks applied versions in a schema_migrations table and only executes
// migrations not yet recorded there, so a second call against an
// already-migrated database does at most one query per dialect (read the
// directory, diff against schema_migrations) rather than replaying DDL.
//
// Concurrency: the Runner takes a database-wide advisory lock for PostgreSQL
// (migration.Runner.session), so concurrent callers against one PostgreSQL
// database -- across processes, not just goroutines -- are safe, the same
// property applyPostgresSchemaFile's advisory lock used to provide. MySQL and
// SQL Server have no such lock (migration/runner.go's Runner.session says why:
// "untested locking code for the other two would be worse than none"), so two
// `go test` processes applying MySQL or SQL Server migrations to the same
// database at the same instant can race on a CREATE that has no IF NOT EXISTS
// equivalent for the object being created. This is the same reason CLAUDE.md
// asks for `-p 1` when more than one database-backed package runs against one
// database: that discipline now also covers migration application, not only
// CleanupPostgresTestData's unqualified DELETE.
//
// A MySQL race of that shape leaves more than a failed test run behind: MySQL
// DDL (CREATE TABLE, CREATE INDEX, ...) implicitly commits, and cannot be
// rolled back, independently of the transaction migration.Runner wraps around
// each file. So a migration that fails partway through -- one racing Runner
// loses a CREATE INDEX to the other with "Error 1061 (Duplicate key name)" --
// leaves its already-executed CREATE TABLE/CREATE INDEX statements permanently
// committed while the file's own "record this version as applied" INSERT never
// runs (it comes after the failing statement in the same loop). The database
// is left with the shape but not the schema_migrations row, and every later
// Run() sees the version as pending and tries 001_schema.sql again from
// scratch, hitting the same duplicate-object error forever. Observed once
// during this package's own migration to this file (a stale per-tenant MySQL
// database, `cleat_<tenant-uuid>`, left in exactly this state by an unrelated
// earlier run sharing this repo's common test tenant UUIDs) -- CLAUDE.md's
// "when a schema migration lands, recreate your test databases" applies here
// too: drop and recreate rather than debugging the code, because the code is
// not what is broken.
func applyMigrations(t *testing.T, db *sql.DB, dialect Dialect) {
	t.Helper()
	r := migration.NewRunner(db, toMigrationDialect(dialect), migrationsRoot())
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("apply %s migrations from %s: %v", dialect, migrationsRoot(), err)
	}
}
