// Multi-backend test for the reaper, against real databases.
//
// The reaper had never run on ANY dialect, and two rounds of fixing did not
// change that:
//
//   - Its PostgreSQL arm was `UPDATE ... LIMIT 1000`, which PostgreSQL rejects
//     outright (cleat#1133).
//   - cleat#1134 replaced that with a subquery and keyed it on `id`, copied
//     from the MSSQL arm alongside. task_queue has no `id` column -- its key is
//     (tenant_id, queue_name, job_id) -- and the MSSQL arm had never run either.
//     So the repair took its column name from a statement whose never having
//     executed was the defect being repaired, and only the error changed shape
//     (cleat#1141).
//
// WHY NEITHER ROUND WAS CAUGHT, which is what this file is for. runReaper logs
// its error and returns -1; nothing upstream fails. A broken statement is
// indistinguishable from "no stuck jobs" to every caller, and to a green
// nightly. And the existing suites drive the plugin through a fake driver that
// pattern-matches the query string, so they accept SQL no database would.
//
// The #1134 fix was also "verified against a live server" -- on a table created
// by hand for the check, with an `id` column, because the query under test used
// one. A fixture built to fit the assumption cannot disagree with it. This file
// builds its schema from the plugin's own migrations, so a column that does not
// exist cannot pass.
package jobqueue

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestReaperResetsAStuckJob_MultiBackend(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			p := &Plugin{dialect: dialect}

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("jobqueue migrations on %s: %v", be.Name, err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB}
			p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

			tenant := uuid.New()
			job := uuid.New()
			t.Cleanup(func() {
				_, _ = be.DB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM task_queue WHERE tenant_id = $1`, dialect), tenant)
			})

			stuckSince := time.Now().UTC().Add(-30 * time.Minute)
			if _, err := be.DB.ExecContext(ctx, plugin.Rebind(
				`INSERT INTO task_queue (tenant_id, queue_name, job_id, status, started_at)
				 VALUES ($1, $2, $3, 'running', $4)`, dialect),
				tenant, "reaper-test", job, stuckSince); err != nil {
				t.Fatalf("insert stuck job on %s: %v", be.Name, err)
			}

			// ASSERT THE EFFECT, NOT THE ABSENCE OF AN ERROR. runReaper logs and
			// returns -1, so a statement that does not run looks exactly like a
			// queue with nothing stuck in it -- which is how this reached a
			// nightly that reported success, twice.
			if n := p.runReaper(ctx); n < 0 {
				t.Fatalf("runReaper reported failure on %s; the statement did not execute", be.Name)
			}

			var status string
			if err := be.DB.QueryRowContext(ctx, plugin.Rebind(
				`SELECT status FROM task_queue WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3`,
				dialect), tenant, "reaper-test", job).Scan(&status); err != nil {
				t.Fatalf("read back on %s: %v", be.Name, err)
			}
			if status != "pending" {
				t.Errorf("on %s the stuck job is still %q, want \"pending\".\n\n"+
					"A job left `running` by a crashed worker is never picked up again. "+
					"The reaper exists to return it to the queue, and until cleat#1141 it "+
					"had never executed on any dialect -- first a syntax error, then a "+
					"column that does not exist, both invisible because runReaper logs "+
					"and returns.", be.Name, status)
			}
		})
	}
}
