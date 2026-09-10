package jobqueue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// resetStuckJobsQuery resets running jobs that have been running for more than
// 5 minutes back to pending, so they can be picked up by another worker.
var resetStuckJobsQuery = plugin.Query{
	// task_queue's key is (tenant_id, queue_name, job_id). THERE IS NO `id`
	// COLUMN, and both of these arms were keyed on one until now.
	//
	// The PostgreSQL arm was `UPDATE ... LIMIT 1000`, which PostgreSQL rejects,
	// so the reaper had never run (cleat#1133). #1134 replaced it with a
	// subquery -- and keyed that subquery on `id`, copied from the MSSQL arm
	// beside it. The MSSQL arm had never run either: `Invalid column name 'id'`
	// is in the nightly's own allowlist. So the repair took its column name from
	// a statement whose never having executed was the thing being fixed, and the
	// reaper still did not run -- only the error changed shape, from
	// `syntax error at or near "LIMIT"` to `column "id" does not exist`
	// (cleat#1141).
	//
	// It passed review because it was "verified against a live server" -- on a
	// table created by hand for the test, with an `id` column, because the query
	// under test used one. The fixture was built to fit the assumption, so the
	// check could not have disagreed. Both statements below are now run against
	// the schema in migrations.go.
	//
	// PostgreSQL takes a row constructor in IN; SQL Server does not, so its arm
	// joins to a TOP subquery on the same three columns.
	Default: `UPDATE task_queue SET status = 'pending', started_at = NULL WHERE (tenant_id, queue_name, job_id) IN (SELECT tenant_id, queue_name, job_id FROM task_queue WHERE status = 'running' AND started_at < NOW() - INTERVAL '5 minutes' ORDER BY tenant_id, queue_name, job_id LIMIT 1000)`,
	MySQL:   `UPDATE task_queue SET status = 'pending', started_at = NULL WHERE status = 'running' AND started_at < NOW() - INTERVAL 5 MINUTE LIMIT 1000`,
	MSSQL:   `UPDATE tq SET status = 'pending', started_at = NULL FROM task_queue tq INNER JOIN (SELECT TOP 1000 tenant_id, queue_name, job_id FROM task_queue WHERE status = 'running' AND started_at < DATEADD(minute, -5, SYSUTCDATETIME()) ORDER BY tenant_id, queue_name, job_id) s ON tq.tenant_id = s.tenant_id AND tq.queue_name = s.queue_name AND tq.job_id = s.job_id`,
}

// runReaper resets stuck running jobs back to pending. Returns the number of
// jobs reset, or -1 on error (the error is logged internally).
func (p *Plugin) runReaper(ctx context.Context) int {
	n, err := p.db.Exec(ctx, plugin.Rebind(resetStuckJobsQuery.For(p.dialect), p.dialect))
	if err != nil {
		p.logger.Error("jobqueue: reaper failed",
			"plugin", p.Info().Name,
			"error", err,
		)
		return -1
	}
	return int(n)
}

// Run starts the background worker goroutine. It polls the task_queue for
// pending jobs and runs a periodic reaper to unstuck jobs left running by
// crashed workers. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("jobqueue: no database, worker disabled")
		<-ctx.Done()
		return nil
	}

	pollTicker := time.NewTicker(5 * time.Second)
	defer pollTicker.Stop()

	reaperTicker := time.NewTicker(60 * time.Second)
	defer reaperTicker.Stop()

	p.logger.Info("jobqueue: worker started, poll interval=5s, reaper interval=60s")

	// Run reaper once immediately at startup so stuck jobs from previous crashes
	// are cleared before the first poll cycle.
	if n := p.runReaper(ctx); n >= 0 {
		p.logger.Info("jobqueue: initial reaper cycle completed",
			"plugin", p.Info().Name,
			"jobs_reset", n,
		)
	}

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("jobqueue: worker stopped")
			return nil

		case <-pollTicker.C:
			start := time.Now()
			claimed, dispatched, failed, err := p.pollPending(ctx)
			if err != nil {
				p.logger.Error("jobqueue: poll failed",
					"plugin", p.Info().Name,
					"error", err,
				)
				continue
			}
			p.logger.Info("jobqueue: work cycle completed",
				"plugin", p.Info().Name,
				"duration_ms", time.Since(start).Milliseconds(),
				"jobs_claimed", claimed,
				"jobs_dispatched", dispatched,
				"jobs_failed", failed,
			)

		case <-reaperTicker.C:
			start := time.Now()
			if n := p.runReaper(ctx); n >= 0 {
				p.logger.Info("jobqueue: reaper cycle completed",
					"plugin", p.Info().Name,
					"duration_ms", time.Since(start).Milliseconds(),
					"jobs_reset", n,
				)
			}
		}
	}
}

// pollPending scans for pending jobs and dispatches them. For each pending job,
// it atomically claims the job (status -> 'running') and dispatches the
// referenced workflow. Jobs without a def_name are marked completed immediately.
// Returns (claimed, dispatched, failed, error).
func (p *Plugin) pollPending(ctx context.Context) (int, int, int, error) {
	rows, err := p.db.Query(ctx, queryPendingJobs.For(p.dialect))
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()

	var claimed, dispatched, failed int

	for rows.Next() {
		var (
			tenantID  uuid.UUID
			queueName string
			jobID     uuid.UUID
			payload   []byte
			defName   *string
			input     json.RawMessage
		)
		if err := plugin.ScanRow(rows, &tenantID, &queueName, &jobID, &payload, &defName, &input); err != nil {
			p.logger.Error("jobqueue: scan job", "error", err)
			continue
		}

		// Atomically claim the job. Only succeeds if still pending (avoids
		// double-dispatch when multiple workers poll concurrently).
		rowsAffected, err := p.db.Exec(ctx, plugin.Rebind(`
				UPDATE task_queue
				SET status = 'running', started_at = now()
				WHERE job_id = $1 AND tenant_id = $2 AND queue_name = $3 AND status = 'pending'
			`, p.dialect), jobID, tenantID, queueName)
		if err != nil {
			p.logger.Error("jobqueue: claim job", "job_id", jobID, "error", err)
			continue
		}
		if rowsAffected == 0 {
			// Another worker claimed it; skip.
			continue
		}
		claimed++

		if defName != nil && *defName != "" {
			// Dispatch as a workflow.
			if input == nil {
				input = json.RawMessage("{}")
			}

			runID, err := p.env.StartWorkflow(ctx, *defName, input)
			if err != nil {
				p.logger.Error("jobqueue: dispatch workflow",
					"job_id", jobID,
					"def_name", *defName,
					"error", err,
				)
				failed++
				if _, updateErr := p.db.Exec(ctx, plugin.Rebind(`
						UPDATE task_queue
						SET status = 'failed', completed_at = now()
						WHERE job_id = $1 AND tenant_id = $2 AND queue_name = $3
					`, p.dialect), jobID, tenantID, queueName); updateErr != nil {
					p.logger.Error("jobqueue: mark failed", "job_id", jobID, "error", updateErr)
				}
				continue
			}

			dispatched++
			p.logger.Info("jobqueue: dispatched workflow",
				"job_id", jobID,
				"def_name", *defName,
				"run_id", runID,
			)

			if _, updateErr := p.db.Exec(ctx, plugin.Rebind(`
					UPDATE task_queue
					SET status = 'completed', completed_at = now(), run_id = $4
					WHERE job_id = $1 AND tenant_id = $2 AND queue_name = $3
				`, p.dialect), jobID, tenantID, queueName, runID); updateErr != nil {
				p.logger.Error("jobqueue: mark completed", "job_id", jobID, "error", updateErr)
			}
		} else {
			p.logger.Info("jobqueue: job has no workflow target, marking completed",
				"job_id", jobID,
				"queue", queueName,
				"tenant", tenantID,
			)
			if _, updateErr := p.db.Exec(ctx, plugin.Rebind(`
					UPDATE task_queue
					SET status = 'completed', completed_at = now()
					WHERE job_id = $1 AND tenant_id = $2 AND queue_name = $3
				`, p.dialect), jobID, tenantID, queueName); updateErr != nil {
				p.logger.Error("jobqueue: mark completed", "job_id", jobID, "error", updateErr)
			}
		}
	}

	return claimed, dispatched, failed, rows.Err()
}

// The next batch of pending jobs to dispatch.
//
// THIS IS WHY "the reaper works now" DID NOT MEAN "jobqueue works now".
// cleat#1134 and cleat#1141 fixed the reaper's UPDATE, twice. This SELECT sits
// eight lines above one of the plugin.Query values they were fixing and was
// never part of either: a raw literal with `LIMIT 10`, issued to all three
// backends. So after both repairs the reaper was correct and had nothing to
// reap on SQL Server, because no job could reach `running` there.
//
// The general point, and the reason a guard over plugin.Query declarations
// cannot close cleat#1133: plugin.Query was never the boundary of the defect,
// only the boundary of the fix. A check has to anchor on where SQL is
// EXECUTED, not on where a dialect table is DECLARED.
var queryPendingJobs = plugin.Query{
	Default: `SELECT tenant_id, queue_name, job_id, payload, def_name, input
FROM task_queue
WHERE status = 'pending'
ORDER BY created_at ASC
LIMIT 10`,
	MySQL: `SELECT tenant_id, queue_name, job_id, payload, def_name, input
FROM task_queue
WHERE status = 'pending'
ORDER BY created_at ASC
LIMIT 10`,
	MSSQL: `SELECT TOP 10 tenant_id, queue_name, job_id, payload, def_name, input
FROM task_queue
WHERE status = 'pending'
ORDER BY created_at ASC`,
}
