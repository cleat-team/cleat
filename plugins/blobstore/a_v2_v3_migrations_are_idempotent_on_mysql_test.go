package blobstore

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestV2V3MigrationsAreIdempotentOnMySQL is the regression test for
// cleat#2880: v2 (blob_content.storage_backend, s3_key) and v3
// (blob_index.deleted_at) each had a bare `ALTER TABLE ... ADD COLUMN` for
// their MySQL arm, not idempotent the way their PostgreSQL (`IF NOT
// EXISTS`) and SQL Server (a sys.columns guard) arms already were. MySQL
// DDL is not transactional, so a crash between one of those ALTERs and
// plugin_migrations recording the version leaves a worker that re-runs it
// on its next start and gets `ERROR 1060 (42S21): Duplicate column name`
// -- fatal to boot, on every start thereafter.
//
// Mirrors plugins/webhookingest's TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL
// (cleat#2221) exact shape: plugin.RunMigrations' version-tracking skip
// cannot exercise this, since calling it twice just skips the version the
// second time -- the mechanism a crash defeats. This re-runs each
// migration's real UpMySQL text, read from p.Migrations() rather than
// hand-copied, directly after RunMigrations has already applied it once.
func TestV2V3MigrationsAreIdempotentOnMySQL(t *testing.T) {
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

			upMySQL := map[int]string{}
			for _, m := range p.Migrations() {
				upMySQL[m.Version] = m.UpMySQL
			}
			for _, v := range []int{2, 3} {
				if upMySQL[v] == "" {
					t.Fatalf("migration v%d's UpMySQL is empty -- nothing to re-run", v)
				}
			}

			for _, v := range []int{2, 3} {
				runStatements(t, ctx, be, upMySQL[v])
				runStatements(t, ctx, be, upMySQL[v])
			}

			assertColumnCount := func(table, column string, want int) {
				t.Helper()
				var got int
				if err := be.DB.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM information_schema.columns
					WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?
				`, table, column).Scan(&got); err != nil {
					t.Fatalf("count %s.%s columns: %v", table, column, err)
				}
				if got != want {
					t.Errorf("%s.%s column count after two extra re-runs: got %d, want %d", table, column, got, want)
				}
			}
			// v2
			assertColumnCount("blob_content", "storage_backend", 1)
			assertColumnCount("blob_content", "s3_key", 1)
			// v3
			assertColumnCount("blob_index", "deleted_at", 1)
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
