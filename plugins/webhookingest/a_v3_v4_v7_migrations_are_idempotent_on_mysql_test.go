package webhookingest

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

// v9CappedPlugin exposes only migrations with Version <= 9, so this test can
// check v3/v4's added columns while they still exist. v10 (cleat#2689) drops
// two of v3's columns (signal_workflow_id, signal_name) and two of v4's
// (retry_count, last_retry_at) as part of retiring the legacy push-delivery
// path -- running the full migration chain, as v8's own idempotency test
// does, would leave nothing there to assert on. Filtered by Version rather
// than sliced by index, unlike eventtriggers' v7OnlyPlugin: this plugin's
// versions are 1, 3, 4, 5, 6, 7, 8, 9, 10 (no 2), so an index slice would
// stop one migration short of where its name says.
//
// Info() is NOT overridden, so it returns the real Plugin's name
// ("webhook-ingest") via Go method promotion -- the same plugin_migrations
// tracking key every other test in this package's `go test` invocation
// shares. That is exactly why this test gives itself an ISOLATED database
// below (cleat#2871 fixed the identical hazard in plugins/eventtriggers):
// TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL, elsewhere in this
// package, applies the REAL unwrapped plugin's full 1..10 chain against
// testutil.NewPluginTestBackends' shared MySQL database, and migrations run
// under a shared plugin_name are tracked identically regardless of which Go
// wrapper issued them -- so on a shared database, whichever of these two
// tests runs second would find versions already recorded by the first and
// silently skip them, with v10 having already dropped the very columns this
// test needs present.
type v9CappedPlugin struct{ *Plugin }

func (v v9CappedPlugin) Migrations() []plugin.Migration {
	var capped []plugin.Migration
	for _, m := range v.Plugin.Migrations() {
		if m.Version <= 9 {
			capped = append(capped, m)
		}
	}
	return capped
}

// freshMySQLDatabase creates a uniquely-named MySQL database on the same
// server CLEAT_TEST_MYSQL points at, builds the package's full test schema
// in it, and returns a *sql.DB connected to that database alone. Mirrors
// plugin/a_concurrent_plugin_migrators_are_serialised_test.go's identical
// pattern (cleat#2117) rather than introducing a new one.
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

	name := fmt.Sprintf("cleat_test_2223_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		adb.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	// Registered before the drop below, so t.Cleanup's LIFO order runs the
	// drop FIRST and closes adb LAST -- the reverse leaves the drop trying
	// to Exec on an already-closed connection ("sql: database is closed"),
	// silently, if its own error is discarded the way this pattern's
	// precedent (plugin/a_concurrent_plugin_migrators_are_serialised_test.go)
	// does; logged here instead so a real failure to drop is not silent too.
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

// TestV3V4V7MigrationsAreIdempotentOnMySQL is the regression test for
// cleat#2223: migrations v3 (webhook_sources.signal_workflow_id,
// signal_name), v4 (webhook_events.retry_count, last_retry_at, status,
// error_msg) and v7 (webhook_sources.secret_configured added, secret
// dropped) all had a bare `ALTER TABLE ... ADD/DROP COLUMN` for their MySQL
// arm -- not idempotent the way their PostgreSQL (`IF NOT EXISTS`) and SQL
// Server (a sys.columns guard) arms already were. MySQL DDL is not
// transactional, so a crash between one of those ALTERs and
// plugin_migrations recording the version leaves a worker that re-runs it on
// its next start and gets `ERROR 1060` (duplicate column) or `ERROR 1091`
// (no such column to drop) -- fatal to boot, on every start thereafter,
// since the tracking row that would make RunMigrations skip it was never
// written.
//
// Mirrors TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL's (v8,
// #2221) exact shape and reuses its runStatements helper: calling
// plugin.RunMigrations twice only ever skips an already-applied version,
// which is precisely the mechanism a crash defeats, so this re-runs each
// migration's real UpMySQL text -- read from p.Migrations(), not
// hand-copied -- directly, after RunMigrations has already applied it once.
func TestV3V4V7MigrationsAreIdempotentOnMySQL(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	db := freshMySQLDatabase(t)
	be := testutil.PluginTestBackend{Name: "mysql", Dialect: testutil.DialectMySQL, DB: db}

	ctx := context.Background()

	real := &Plugin{dialect: plugin.DialectMySQL}
	if err := plugin.RunMigrations(ctx, db, plugin.DialectMySQL, nil,
		[]*plugin.LoadedPlugin{{Plugin: v9CappedPlugin{real}, Healthy: true}}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	p := real

	upMySQL := map[int]string{}
	for _, m := range p.Migrations() {
		upMySQL[m.Version] = m.UpMySQL
	}
	for _, v := range []int{3, 4, 7} {
		if upMySQL[v] == "" {
			t.Fatalf("migration v%d's UpMySQL is empty -- nothing to re-run", v)
		}
	}

	// Two extra re-runs of each, for good measure -- the guard should not
	// be order- or count-sensitive.
	for _, v := range []int{3, 4, 7} {
		runStatements(t, ctx, be, upMySQL[v])
		runStatements(t, ctx, be, upMySQL[v])
	}

	assertColumnCount := func(table, column string, want int) {
		t.Helper()
		var got int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?
		`, table, column).Scan(&got); err != nil {
			t.Fatalf("count %s.%s columns: %v", table, column, err)
		}
		if got != want {
			t.Errorf("%s.%s column count after two extra re-runs: got %d, want %d", table, column, got, want)
		}
	}

	// v3
	assertColumnCount("webhook_sources", "signal_workflow_id", 1)
	assertColumnCount("webhook_sources", "signal_name", 1)
	// v4
	assertColumnCount("webhook_events", "retry_count", 1)
	assertColumnCount("webhook_events", "last_retry_at", 1)
	assertColumnCount("webhook_events", "status", 1)
	assertColumnCount("webhook_events", "error_msg", 1)
	// v7: added, and dropped
	assertColumnCount("webhook_sources", "secret_configured", 1)
	assertColumnCount("webhook_sources", "secret", 0)
}
