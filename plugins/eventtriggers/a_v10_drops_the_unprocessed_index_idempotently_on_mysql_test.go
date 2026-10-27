package eventtriggers

import (
	"context"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestV10DropsTheUnprocessedIndexIdempotentlyOnMySQL is the regression test
// for cleat#2870's MySQL arm: Version 10's UpMySQL guards
// `DROP INDEX idx_ingested_events_unprocessed` through information_schema,
// the same discipline Version 6/7/8/9 established for this table, because
// MySQL DDL is not transactional -- a migration that failed partway must be
// safe to re-run. Without the guard, a crash between a successful DROP and
// plugin_migrations recording Version 10 leaves a worker that re-runs it on
// its next start and gets `ERROR 1091 (42000): Can't DROP 'idx_...';
// check that column/key exists` -- fatal to boot, on every start thereafter.
// Mirrors TestV1V3IndexesAreIdempotentOnMySQL's shape exactly, one version
// lower in the file and one direction reversed (DROP, not CREATE).
func TestV10DropsTheUnprocessedIndexIdempotentlyOnMySQL(t *testing.T) {
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

			var upMySQL10 string
			for _, m := range p.Migrations() {
				if m.Version == 10 {
					upMySQL10 = m.UpMySQL
				}
			}
			if upMySQL10 == "" {
				t.Fatalf("migration v10's UpMySQL is empty -- nothing to re-run")
			}

			// RunMigrations above already applied v10 once, dropping the
			// index. Re-running its guarded DROP two more times must not
			// error, the same way TestV1V3IndexesAreIdempotentOnMySQL
			// re-runs v1's guarded CREATE.
			runStatements(t, ctx, be, upMySQL10)
			runStatements(t, ctx, be, upMySQL10)

			var got int
			if err := be.DB.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM information_schema.statistics
				WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_unprocessed'
			`).Scan(&got); err != nil {
				t.Fatalf("count idx_ingested_events_unprocessed: %v", err)
			}
			if got != 0 {
				t.Errorf("idx_ingested_events_unprocessed still exists after v10's drop and two extra idempotent re-runs")
			}
		})
	}
}
