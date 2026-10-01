package notifications

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestV5MigrationIsIdempotentOnMySQL is the regression test for cleat#2880:
// migration v5 (adding webhook_config.secret_configured, dropping
// webhook_config.secret) had bare `ALTER TABLE ... ADD/DROP COLUMN`
// statements for its MySQL arm, not idempotent the way its PostgreSQL (`IF
// NOT EXISTS`/`IF EXISTS`) and SQL Server (a sys.columns guard) arms already
// were. MySQL DDL is not transactional, so a crash between one of those
// ALTERs and plugin_migrations recording version 5 leaves a worker that
// re-runs it on its next start and gets `ERROR 1060` (duplicate column) or
// `ERROR 1091` (no such column to drop) -- fatal to boot, on every start
// thereafter.
//
// Mirrors plugins/webhookingest's TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL
// (cleat#2221) exact shape: plugin.RunMigrations' version-tracking skip
// cannot exercise this, since calling it twice just skips v5 the second
// time -- the mechanism a crash defeats. This re-runs the migration's real
// UpMySQL text, read from p.Migrations() rather than hand-copied, directly
// after RunMigrations has already applied it once.
func TestV5MigrationIsIdempotentOnMySQL(t *testing.T) {
	// cleat#2880 (same shape fixed in cleat#2249/#2883): testutil.NewPluginTestBackends
	// only includes a MySQL backend when CLEAT_TEST_MYSQL is set -- PostgreSQL is the
	// only one it attempts unconditionally. So when CLEAT_TEST_MYSQL is unset, the
	// returned slice has no MySQL entry, this loop's `continue` fires on every
	// iteration, the t.Run below is never reached, and the function returns having
	// asserted nothing -- a genuine PASS with no subtests and no t.Skip anywhere to
	// make that visible to the skip-budget/skip-ledger guards, which only see t.Skip.
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	for _, be := range testutil.NewPluginTestBackends(t) {
		if be.Dialect != testutil.DialectMySQL {
			continue
		}
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: plugin.DialectMySQL}
			if err := plugin.RunMigrations(ctx, be.DB, plugin.DialectMySQL, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}

			var v5SQL string
			for _, m := range p.Migrations() {
				if m.Version == 5 {
					v5SQL = m.UpMySQL
				}
			}
			if v5SQL == "" {
				t.Fatal("migration v5's UpMySQL is empty -- nothing to re-run")
			}

			runStatements(t, ctx, be, v5SQL)
			runStatements(t, ctx, be, v5SQL)

			assertColumnCount := func(column string, want int) {
				t.Helper()
				var got int
				if err := be.DB.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM information_schema.columns
					WHERE table_schema = DATABASE() AND table_name = 'webhook_config' AND column_name = ?
				`, column).Scan(&got); err != nil {
					t.Fatalf("count webhook_config.%s columns: %v", column, err)
				}
				if got != want {
					t.Errorf("webhook_config.%s column count after two extra re-runs: got %d, want %d", column, got, want)
				}
			}
			assertColumnCount("secret_configured", 1)
			assertColumnCount("secret", 0)
		})
	}
}

// runStatements executes sqlText one statement at a time, on one pinned
// connection, mirroring plugin.RunMigrations' own execSQLStatements closely
// enough to exercise the same hazard it would: MySQL user-defined session
// variables (@col, @ddl) and a PREPAREd statement do not survive being sent
// on different pooled connections. A naive split on ';' is safe for this
// specific text -- checked by eye, and it is short and entirely this
// package's own -- but is not a general-purpose SQL splitter.
func runStatements(t *testing.T, ctx context.Context, be testutil.PluginTestBackend, sqlText string) {
	t.Helper()
	conn, err := be.DB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()

	for _, stmt := range strings.Split(sqlText, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}
