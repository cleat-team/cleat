package webhookingest

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL pins cleat-review's
// finding on #2221: migration v8 (webhook_sources.deleted_at, this PR's own
// migration) had a bare `ALTER TABLE ... ADD COLUMN` for its MySQL arm, not
// idempotent the way its PostgreSQL (`IF NOT EXISTS`) and SQL Server (a
// sys.columns guard) arms already were. MySQL DDL is not transactional, so a
// crash between that ALTER and plugin_migrations recording version 8 leaves
// a worker that re-runs it on its next start and gets
// `ERROR 1060 (42S21): Duplicate column name 'deleted_at'` -- fatal to boot,
// on every start thereafter, since the tracking row that would make
// RunMigrations skip it was never written.
//
// That is exactly what plugin.RunMigrations' own version-tracking skip
// cannot exercise: calling it twice just skips v8 the second time, which is
// the mechanism the crash defeats. This runs the migration's real UpMySQL
// text -- read from p.Migrations(), not hand-copied -- directly, after
// RunMigrations has already applied it once, the same state a crashed-then-
// restarted worker would find the column in.
func TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL(t *testing.T) {
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

			var v8SQL string
			for _, m := range p.Migrations() {
				if m.Version == 8 {
					v8SQL = m.UpMySQL
				}
			}
			if v8SQL == "" {
				t.Fatal("migration v8's UpMySQL is empty -- nothing to re-run")
			}

			// The column already exists at this point (RunMigrations applied
			// v8 above, inside its own transaction). Re-running the same text
			// against an ordinary connection -- not the migration
			// framework's tx.ExecContext, and not a multi-statement DSN --
			// is the crash scenario, and also the more exacting one: the
			// PREPARE/EXECUTE/DEALLOCATE sequence's session variables (@col,
			// @ddl) have to survive being split into separate Exec calls the
			// same way plugin.RunMigrations' own execSQLStatements would
			// split them, which only holds if they land on one connection.
			runStatements(t, ctx, be, v8SQL)

			// And a second extra run, for good measure -- the guard should
			// not be order- or count-sensitive.
			runStatements(t, ctx, be, v8SQL)

			var colCount int
			if err := be.DB.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM information_schema.columns
				WHERE table_schema = DATABASE() AND table_name = 'webhook_sources' AND column_name = 'deleted_at'
			`).Scan(&colCount); err != nil {
				t.Fatalf("count deleted_at columns: %v", err)
			}
			if colCount != 1 {
				t.Errorf("webhook_sources.deleted_at column count after two extra re-runs: got %d, want 1", colCount)
			}
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
