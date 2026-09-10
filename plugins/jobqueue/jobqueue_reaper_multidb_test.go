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
	"database/sql"
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

// TestReaperTouchesNothingItShouldNot is the negative control the test above
// does not have, and the gap is not hypothetical: with one stuck job and one
// assertion that it moved, this passes
//
//	UPDATE task_queue SET status = 'pending', started_at = NULL
//
// with no WHERE clause at all. "The stuck one moved" is satisfied by a reaper
// that resets every row in the table, on every tick, including the ones a
// worker is actively running.
//
// The 5-minute threshold is the reaper's entire discretion -- it is the only
// thing separating "this worker died" from "this worker is busy" -- and until
// this test nothing exercised it in either direction. Three of the four rows
// here exist to fail if the WHERE clause is wrong rather than absent: a
// `running` job younger than the threshold, and rows in the two states the
// reaper has no business rewriting.
func TestReaperTouchesNothingItShouldNot(t *testing.T) {
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
			queue := "reaper-controls"
			t.Cleanup(func() {
				_, _ = be.DB.ExecContext(context.Background(),
					plugin.Rebind(`DELETE FROM task_queue WHERE tenant_id = $1`, dialect), tenant)
			})

			// 30 minutes and 1 minute sit far enough either side of the
			// 5-minute threshold that skew between this process's clock and
			// the server's cannot decide the outcome. An assertion that
			// depends on wall-clock proximity is one that fails sometimes,
			// which is worse than one that cannot fail.
			now := time.Now().UTC()
			rows := []struct {
				name      string
				status    string
				startedAt time.Time
				want      string
			}{
				{"stale_running", "running", now.Add(-30 * time.Minute), "pending"},
				{"fresh_running", "running", now.Add(-1 * time.Minute), "running"},
				{"stale_pending", "pending", now.Add(-30 * time.Minute), "pending"},
				{"stale_done", "done", now.Add(-30 * time.Minute), "done"},
			}

			ids := make(map[string]uuid.UUID, len(rows))
			for _, r := range rows {
				id := uuid.New()
				ids[r.name] = id
				if _, err := be.DB.ExecContext(ctx, plugin.Rebind(
					`INSERT INTO task_queue (tenant_id, queue_name, job_id, status, started_at)
					 VALUES ($1, $2, $3, $4, $5)`, dialect),
					tenant, queue, id, r.status, r.startedAt); err != nil {
					t.Fatalf("insert %s on %s: %v", r.name, be.Name, err)
				}
			}

			n := p.runReaper(ctx)
			if n < 0 {
				t.Fatalf("runReaper reported failure on %s; the statement did not execute", be.Name)
			}

			for _, r := range rows {
				var status string
				var startedAt sql.NullTime
				if err := be.DB.QueryRowContext(ctx, plugin.Rebind(
					`SELECT status, started_at FROM task_queue
					 WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3`, dialect),
					tenant, queue, ids[r.name]).Scan(&status, &startedAt); err != nil {
					t.Fatalf("read back %s on %s: %v", r.name, be.Name, err)
				}
				if status != r.want {
					t.Errorf("on %s, %s is %q after the reaper, want %q.\n\n"+
						"Only a `running` job older than 5 minutes is eligible. A reaper "+
						"that takes more than that returns work a live worker is holding, "+
						"which is worse than one that never runs: the job is executed "+
						"twice and nothing reports it.", be.Name, r.name, status, r.want)
				}
				// The reaper nulls started_at so the next worker to claim the
				// row records its own. A row left `pending` with a stale
				// started_at is claimable and lies about when it began.
				if r.name == "stale_running" && startedAt.Valid {
					t.Errorf("on %s, stale_running kept started_at = %v; the reaper should have "+
						"cleared it", be.Name, startedAt.Time)
				}
			}

			// One row was eligible. The per-row checks above would catch a
			// statement that took the other three, but this also catches one
			// that reached rows outside this tenant -- which no per-row check
			// can see, because it does not know they exist.
			if n != 1 {
				t.Errorf("on %s the reaper reset %d rows; exactly one was eligible.\n\n"+
					"A count above one means the statement matched rows this test did not "+
					"create -- another tenant's, or another queue's.", be.Name, n)
			}
		})
	}
}
