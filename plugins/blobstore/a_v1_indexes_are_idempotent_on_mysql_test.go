package blobstore

import (
	"context"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestV1IndexesAreIdempotentOnMySQL is the regression test for cleat#2880:
// migration v1's idx_blob_tenant_created and idx_blob_expires indexes on
// blob_index had a bare `CREATE INDEX` for their MySQL arm, not idempotent
// the way their PostgreSQL (`IF NOT EXISTS`) arm already was. MySQL has no
// `CREATE INDEX IF NOT EXISTS`, and the `CREATE TABLE IF NOT EXISTS` beside
// each is idempotent while the index creation right after it is not -- so a
// crash between a successful v1 apply and plugin_migrations recording
// version 1 leaves a worker that re-runs it on its *very first boot* and
// gets `ERROR 1061 (42000): Duplicate key name 'idx_blob_tenant_created'`
// -- fatal to boot, on every start thereafter.
//
// Mirrors plugins/webhookingest's TestWebhookSourcesDeletedAtMigrationIsIdempotentOnMySQL
// (cleat#2221) exact shape: plugin.RunMigrations' version-tracking skip
// cannot exercise this, since calling it twice just skips v1 the second
// time -- the mechanism a crash defeats. This re-runs the migration's real
// UpMySQL text, read from p.Migrations() rather than hand-copied, directly
// after RunMigrations has already applied it once.
func TestV1IndexesAreIdempotentOnMySQL(t *testing.T) {
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

			var v1SQL string
			for _, m := range p.Migrations() {
				if m.Version == 1 {
					v1SQL = m.UpMySQL
				}
			}
			if v1SQL == "" {
				t.Fatal("migration v1's UpMySQL is empty -- nothing to re-run")
			}

			runStatements(t, ctx, be, v1SQL)
			runStatements(t, ctx, be, v1SQL)

			for _, idx := range []string{"idx_blob_tenant_created", "idx_blob_expires"} {
				var got int
				if err := be.DB.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM information_schema.statistics
					WHERE table_schema = DATABASE() AND table_name = 'blob_index' AND index_name = ?
				`, idx).Scan(&got); err != nil {
					t.Fatalf("count %s: %v", idx, err)
				}
				if got == 0 {
					t.Errorf("%s missing after two extra re-runs", idx)
				}
			}
		})
	}
}
