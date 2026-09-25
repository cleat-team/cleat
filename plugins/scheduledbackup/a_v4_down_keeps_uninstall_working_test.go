package scheduledbackup

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestUninstallSchedulerBackupOnEveryDialect is owner decision 1A, 2026-09-24
// (relayed by the coordinator): before this migration's v4 carried a minimal
// Down (see migrations.go's comment on it), `--uninstall-plugin
// scheduled-backup` refused outright on every dialect once v4 had run --
// migration_down.go's declaresDDL/downFor check treats an applied migration
// with DDL and an empty Down as a GAP, and refuses to reverse ANY of the
// plugin's migrations rather than leave the schema in a shape no later run
// can identify. This drives plugin.RunDownMigrations directly, the same
// function cmd/cleat-worker/main.go's --uninstall-plugin handler calls, end
// to end against real PostgreSQL, MySQL and SQL Server -- a table-driven unit
// test of migration_down.go alone could not see a mssql- or mysql-only
// regression in v4's own DownMySQL/DownMSSQL text.
func TestUninstallSchedulerBackupOnEveryDialect(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.td)
			ctx := context.Background()
			dialect := plugin.Dialect(string(tc.td))

			testutil.SetupFullSchema(t, db, tc.td)

			lp := &plugin.LoadedPlugin{Plugin: New(), Healthy: true}
			if err := plugin.RunMigrations(ctx, db, dialect, nil, []*plugin.LoadedPlugin{lp}); err != nil {
				t.Fatalf("apply v1-v5: %v", err)
			}

			// Precondition: v4's own indexes exist before uninstall, so their
			// absence afterward is this Down's doing and not an artifact of
			// the migration never having run.
			if !indexExists(t, ctx, db, dialect, "backup_config", "idx_backup_config_enabled_next") {
				t.Fatal("PRECONDITION FAILED: idx_backup_config_enabled_next does not exist after applying migrations")
			}
			if !indexExists(t, ctx, db, dialect, "backup_history", "idx_backup_history_config") {
				t.Fatal("PRECONDITION FAILED: idx_backup_history_config does not exist after applying migrations")
			}

			// The refusal this test exists to guard against: before v4 had a
			// Down, this returned "cannot reverse: version(s) 4 are applied
			// and declare no Down" and reversed NOTHING, on every dialect.
			//
			// RunDownMigrations has no "reverse down to version N" mode --
			// it always reverses everything applied, newest first -- so v4's
			// own Down (drop its two indexes, leave one behind on MySQL for
			// the FK, per its own comment) cannot be observed as an
			// intermediate state through this public API: v1's Down runs
			// right after it and drops both tables outright. What this DOES
			// prove, which is the actual content of owner decision 1A, is
			// that v4's Down runs without error on every dialect -- if it
			// still raised MySQL's Error 1553 or any other statement error,
			// this Fatalf is where it would surface, before ever reaching
			// v1.
			res, err := plugin.RunDownMigrations(ctx, db, dialect, lp, []*plugin.LoadedPlugin{lp})
			if err != nil {
				t.Fatalf("uninstall refused: %v", err)
			}
			if len(res.Reversed) != 5 {
				t.Fatalf("Reversed = %v, want all 5 versions", res.Reversed)
			}

			// v1's Down (DROP TABLE IF EXISTS, on every dialect since v1
			// added a DownMySQL for exactly this) is what actually removes
			// the schema; confirm it ran, not just that no error surfaced.
			if tableExistsSB(t, ctx, db, dialect, "backup_config") {
				t.Error("backup_config still exists after full uninstall")
			}
			if tableExistsSB(t, ctx, db, dialect, "backup_history") {
				t.Error("backup_history still exists after full uninstall")
			}

			// A plugin uninstalled this way must be re-installable from
			// scratch (migrations.go's comment on v4's Down: "a plugin
			// uninstalled this way and reinstalled starts from v1"). If this
			// fails, RunDownMigrations left debris a fresh v1 collides with.
			if err := plugin.RunMigrations(ctx, db, dialect, nil, []*plugin.LoadedPlugin{lp}); err != nil {
				t.Fatalf("re-apply v1-v5 after uninstall: %v", err)
			}
		})
	}
}

// indexExists asks each dialect's own catalogue directly, since there is no
// shared testutil helper for this and the three catalogues have nothing in
// common syntactically.
func indexExists(t *testing.T, ctx context.Context, db *sql.DB, dialect plugin.Dialect, table, index string) bool {
	t.Helper()
	var q string
	switch dialect {
	case plugin.DialectMySQL:
		q = `SELECT COUNT(*) FROM information_schema.statistics
			WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`
	case plugin.DialectMSSQL:
		q = `SELECT COUNT(*) FROM sys.indexes WHERE object_id = OBJECT_ID(@p1) AND name = @p2`
	default:
		q = `SELECT COUNT(*) FROM pg_indexes WHERE tablename = $1 AND indexname = $2`
	}
	var n int
	if err := db.QueryRowContext(ctx, plugin.Rebind(q, dialect), table, index).Scan(&n); err != nil {
		t.Fatalf("checking index %s on %s: %v", index, table, err)
	}
	return n > 0
}

// tableExistsSB is scoped to this file's own use (plugin/migration_down_test.go
// has a same-named helper in a different package, postgres-only) -- this one
// covers all three dialects, each via its own catalogue.
func tableExistsSB(t *testing.T, ctx context.Context, db *sql.DB, dialect plugin.Dialect, table string) bool {
	t.Helper()
	var q string
	switch dialect {
	case plugin.DialectMySQL:
		q = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`
	case plugin.DialectMSSQL:
		q = `SELECT COUNT(*) FROM sys.tables WHERE name = @p1`
	default:
		q = `SELECT COUNT(*) FROM pg_tables WHERE tablename = $1`
	}
	var n int
	if err := db.QueryRowContext(ctx, plugin.Rebind(q, dialect), table).Scan(&n); err != nil {
		t.Fatalf("checking table %s: %v", table, err)
	}
	return n > 0
}
