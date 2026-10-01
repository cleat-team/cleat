package scheduledbackup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// truncatedScheduledBackupMigrations restricts Migrations() to a prefix, the
// same device a_v8_backfill_preserves_the_old_sweep_selection_test.go
// (plugins/eventtriggers) and TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL's
// sibling tests use. Required here, specifically: a LATER scheduledbackup
// migration (the "v8 lesson" FK/index rework) DROPS backup_config.tenant_id
// and backup_history.tenant_id entirely, replacing idx_backup_config_tenant_enabled_next
// / idx_backup_history_tenant_config with tenant_id-free successors. Running
// v1's own UpMySQL text AFTER the full chain -- as every other plugin's v1
// index test in this PR safely does -- would have v1's guard try to index a
// column that migration has since removed, failing with MySQL error 1072
// ("Key column 'tenant_id' doesn't exist in table") for a reason that says
// nothing about v1's own guard: in production v1 only ever retries BEFORE
// any later version has run, never after. Restricting to v1 keeps this test
// inside that same real precondition.
type truncatedScheduledBackupMigrations struct {
	*Plugin
}

func (p *truncatedScheduledBackupMigrations) Migrations() []plugin.Migration {
	all := p.Plugin.Migrations()
	return all[:1]
}

// freshMySQLDatabase creates a uniquely-named MySQL database on the same
// server CLEAT_TEST_MYSQL points at, builds the package's full test schema
// in it, and returns a *sql.DB connected to that database alone. Mirrors
// plugins/webhookingest's identical helper (cleat#2223) and
// plugins/eventtriggers' (cleat#2880 slice 1), both citing
// plugin/a_concurrent_plugin_migrators_are_serialised_test.go's pattern
// (cleat#2117) rather than introducing a new one.
//
// Required here for a SEPARATE reason from truncatedScheduledBackupMigrations
// above: this package's own TestRunDueBackupsDispatchesExactlyOnceAndAdvancesNextRunAt
// (a_run_due_backups_dispatches_once_test.go, which sorts before this file
// alphabetically) applies the real, unwrapped Plugin's full migration chain
// against testutil.NewPluginTestBackends' SHARED MySQL database with no
// cleanup -- so on that shared database, by the time this test runs,
// plugin_migrations already records this plugin past v8, and
// truncatedScheduledBackupMigrations' RunMigrations call becomes a silent
// no-op against a table whose tenant_id column is already gone: the same
// ERROR 1072 this truncation exists to avoid, reached via a different door
// (cleat#2871's shared-database shape, confirmed by running this package's
// full test set together -- the isolated `-run` for this test alone passed
// with the truncation alone, and only the full-package run reproduced the
// failure). A fresh database makes the truncation's precondition ("v1 has
// just run, nothing later has") actually hold.
func freshMySQLDatabase(t *testing.T) *sql.DB {
	t.Helper()
	admin := os.Getenv("CLEAT_TEST_MYSQL")
	if admin == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	adb, err := sql.Open("mysql", admin)
	if err != nil {
		t.Fatalf("open admin mysql connection: %v", err)
	}
	if err := adb.Ping(); err != nil {
		adb.Close()
		t.Fatalf("ping admin mysql connection: %v", err)
	}

	cfg, err := mysql.ParseDSN(admin)
	if err != nil {
		adb.Close()
		t.Fatalf("parse CLEAT_TEST_MYSQL: %v", err)
	}

	name := fmt.Sprintf("cleat_test_2880_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		adb.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	// Registered before the drop below, so t.Cleanup's LIFO order runs the
	// drop FIRST and closes adb LAST -- the reverse leaves the drop trying
	// to Exec on an already-closed connection ("sql: database is closed").
	t.Cleanup(func() { adb.Close() })
	t.Cleanup(func() {
		if _, err := adb.Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
			t.Logf("cleanup: drop database %s: %v", name, err)
		}
	})

	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("open fresh mysql database %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping fresh mysql database %s: %v", name, err)
	}
	testutil.SetupMySQLFullSchema(t, db)
	return db
}

// TestV1IndexesAreIdempotentOnMySQL is the regression test for cleat#2880:
// migration v1's idx_backup_config_tenant_enabled_next and
// idx_backup_history_tenant_config indexes had a bare `CREATE INDEX` for
// their MySQL arm, not idempotent the way their PostgreSQL (`IF NOT
// EXISTS`) arm already was. MySQL has no `CREATE INDEX IF NOT EXISTS`, and
// the `CREATE TABLE IF NOT EXISTS` beside each is idempotent while the
// index creation right after it is not -- so a crash between a successful
// v1 apply and plugin_migrations recording version 1 leaves a worker that
// re-runs it on its very first boot and gets `ERROR 1061 (42000):
// Duplicate key name 'idx_backup_config_tenant_enabled_next'` -- fatal to
// boot, on every start thereafter.
//
// Mirrors plugins/webhookingest's TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL
// (cleat#2221) exact shape: plugin.RunMigrations' version-tracking skip
// cannot exercise this, since calling it twice just skips v1 the second
// time -- the mechanism a crash defeats. This re-runs the migration's real
// UpMySQL text, read from p.Migrations() rather than hand-copied, directly
// after RunMigrations has already applied it once -- restricted to v1 alone
// (see truncatedScheduledBackupMigrations) so the table still has the shape
// v1's own guard expects, on a database of this test's own (see
// freshMySQLDatabase) so no sibling test's full migration run can have
// already advanced plugin_migrations past v1 first.
func TestV1IndexesAreIdempotentOnMySQL(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}

	ctx := context.Background()
	db := freshMySQLDatabase(t)

	p := &truncatedScheduledBackupMigrations{Plugin: &Plugin{dialect: plugin.DialectMySQL}}
	if err := plugin.RunMigrations(ctx, db, plugin.DialectMySQL, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	var v1SQL string
	for _, m := range p.Migrations() {
		if m.Version == 1 {
			v1SQL = m.UpMySQL
		}
	}
	if v1SQL == "" {
		t.Fatal("migration v1's UpMySQL is empty -- nothing to re-run")
	}

	runStatements(t, ctx, db, v1SQL)
	runStatements(t, ctx, db, v1SQL)

	assertIndexExists := func(table, idx string) {
		t.Helper()
		var got int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.statistics
			WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?
		`, table, idx).Scan(&got); err != nil {
			t.Fatalf("count %s on %s: %v", idx, table, err)
		}
		if got == 0 {
			t.Errorf("%s on %s missing after two extra re-runs", idx, table)
		}
	}
	assertIndexExists("backup_config", "idx_backup_config_tenant_enabled_next")
	assertIndexExists("backup_history", "idx_backup_history_tenant_config")
}
