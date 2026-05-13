package jobqueue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/plugin"
)

// resetStuckJobsQuery resets running jobs that have been running for more than
// 5 minutes back to pending, so they can be picked up by another worker.
var resetStuckJobsQuery = plugin.Query{
	Default: `UPDATE task_queue SET status = 'pending', started_at = NULL WHERE status = 'running' AND started_at < NOW() - INTERVAL '5 minutes' LIMIT 1000`,
	MySQL:   `UPDATE task_queue SET status = 'pending', started_at = NULL WHERE status = 'running' AND started_at < NOW() - INTERVAL 5 MINUTE LIMIT 1000`,
	MSSQL:   `UPDATE task_queue SET status = 'pending', started_at = NULL WHERE id IN (SELECT id FROM task_queue WHERE status = 'running' AND started_at < DATEADD(minute, -5, SYSUTCDATETIME()) ORDER BY id OFFSET 0 ROWS FETCH NEXT 1000 ROWS ONLY)`,
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
	rows, err := p.db.Query(ctx, `
			SELECT tenant_id, queue_name, job_id, payload, def_name, input
			FROM task_queue
			WHERE status = 'pending'
			ORDER BY created_at ASC
			LIMIT 10
		`)
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
		if err := rows.Scan(&tenantID, &queueName, &jobID, &payload, &defName, &input); err != nil {
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
