package plugintest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// CleanupPluginSchema drops a plugin's tables and clears its migration
// bookkeeping on conn, so a test can start from a clean slate regardless of
// what an earlier test in this binary left behind.
//
// WHY THIS IS SHARED RATHER THAN WRITTEN PER TEST. cleat#2664: two tests in
// cmd/cleat-worker each carried their own copy of this -- one exercising
// signalAwaiters, the other a legacy-awaiter replay -- and the replay test
// additionally hand-copied the production functions it meant to exercise
// rather than calling them, because those functions are unexported and it
// lived in the wrong package to reach them. Moving that test into
// plugins/eventtriggers to call the real functions left this helper with no
// home unless it moved too; promoting it here follows the precedent
// AssertMigrationsDoSomething already set for exactly this kind of
// drift-prone duplication.
//
// MSSQL DROPS ARE CHECKED IN GO AND ISSUED UNCONDITIONALLY, not
// "IF EXISTS(...) DROP ..." in one batch: measured directly against a real
// SQL Server (cleat#2239), the single-batch form raises 3701 ("does not
// exist or you do not have permission") on an object sys.security_policies
// confirms IS there, on the very connection running the check -- the IF
// guard and the DROP do not agree with each other inside one batch here, for
// a reason not worth chasing further when checking first in Go sidesteps it
// entirely.
//
// The security-policy name is derived as "<table>_tenant_isolation" --
// plugin/migration.go's applyTenantScopingMSSQL naming -- rather than
// enumerated separately, to avoid depending on that naming twice.
//
// admin.plugin_tables (registerTenantScopedTables, Postgres only) is cleared
// too, so a registry row naming a table that no longer exists cannot outlive
// this either -- see CLAUDE.md's "a resolver must be able to return UNKNOWN"
// family of notes on stale registry rows reading as confident wrong answers.
func CleanupPluginSchema(t *testing.T, conn *sql.DB, dialect testutil.Dialect, pluginName string, tables []string) {
	t.Helper()
	ctx := context.Background()

	exec := func(query string) {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			t.Errorf("CleanupPluginSchema(%s): %s: %v", pluginName, query, err)
		}
	}
	exists := func(query string, args ...any) bool {
		var n int
		if err := conn.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Errorf("CleanupPluginSchema(%s): existence check %s: %v", pluginName, query, err)
			return false
		}
		return n > 0
	}

	if dialect == testutil.DialectMSSQL {
		for _, tbl := range tables {
			policy := tbl + "_tenant_isolation"
			if exists(`SELECT COUNT(*) FROM sys.security_policies WHERE name = @p1`, policy) {
				exec(`DROP SECURITY POLICY dbo.` + policy)
			}
		}
		for _, tbl := range tables {
			if exists(`SELECT COUNT(*) FROM sys.tables WHERE name = @p1`, tbl) {
				exec(`DROP TABLE ` + tbl)
			}
		}
		// Same lazy-creation hazard as the Postgres/MySQL branch below --
		// plugin.RunMigrations creates plugin_migrations on first use, not
		// SetupMinimalSchema -- and unlike DROP TABLE above, DELETE has no
		// "IF EXISTS" form to fall back on: "Invalid object name
		// 'plugin_migrations'. (208)" on a database no plugin has ever
		// migrated.
		if exists(`SELECT COUNT(*) FROM sys.tables WHERE name = 'plugin_migrations'`) {
			exec(`DELETE FROM plugin_migrations WHERE plugin_name = '` + pluginName + `'`)
		}
		return
	}

	for _, tbl := range tables {
		exec(`DROP TABLE IF EXISTS ` + tbl)
	}
	// Unlike DROP TABLE, Postgres and MySQL have no "DELETE ... IF EXISTS":
	// plugin_migrations itself is created lazily, by plugin.RunMigrations'
	// own CREATE TABLE IF NOT EXISTS, not by testutil.SetupMinimalSchema. So
	// on a database no plugin has ever migrated -- a caller that runs this
	// BEFORE any migration, to guarantee a clean slate, hits a blind DELETE
	// that fails outright: "relation plugin_migrations does not exist"
	// (42P01 on Postgres, 1146 on MySQL). Guard it the same way the MSSQL
	// branch above already guards its DROPs.
	if migrationsTableExists(t, conn, dialect) {
		exec(`DELETE FROM plugin_migrations WHERE plugin_name = '` + pluginName + `'`)
	}
	if dialect == testutil.DialectPostgres && exists(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'admin' AND table_name = 'plugin_tables'`) {
		exec(`DELETE FROM admin.plugin_tables WHERE plugin_name = '` + pluginName + `'`)
	}
}

// migrationsTableExists reports whether plugin_migrations has been created
// yet on conn. It exists lazily -- plugin.RunMigrations creates it with
// CREATE TABLE IF NOT EXISTS on first use -- so a cleanup that runs before
// any migration has ever executed against this database must not assume it
// is there.
func migrationsTableExists(t *testing.T, conn *sql.DB, dialect testutil.Dialect) bool {
	t.Helper()
	ctx := context.Background()
	var query string
	switch dialect {
	case testutil.DialectMySQL:
		query = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'plugin_migrations'`
	default:
		query = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'plugin_migrations'`
	}
	var n int
	if err := conn.QueryRowContext(ctx, query).Scan(&n); err != nil {
		t.Errorf("CleanupPluginSchema: check plugin_migrations exists: %v", err)
		return false
	}
	return n > 0
}
