package jobqueue

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Run starts the background worker goroutine. It polls the task_queue for
// pending jobs and logs dispatch events (actual workflow dispatch comes later).
// Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("jobqueue: no database, worker disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	p.logger.Info("jobqueue: worker started, poll interval=5s")

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("jobqueue: worker stopped")
			return nil

		case <-ticker.C:
			if err := p.pollPending(ctx); err != nil {
				p.logger.Error("jobqueue: poll failed", "error", err)
			}
		}
	}
}

// pollPending scans for pending jobs and dispatches them. For each pending job,
// it atomically claims the job (status -> 'running') and logs a dispatch event.
// Actual workflow dispatch integration comes in a later phase.
func (p *Plugin) pollPending(ctx context.Context) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT tenant_id, queue_name, job_id, payload
		FROM task_queue
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT 10
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tenantID  uuid.UUID
			queueName string
			jobID     uuid.UUID
			payload   []byte
		)
		if err := rows.Scan(&tenantID, &queueName, &jobID, &payload); err != nil {
			p.logger.Error("jobqueue: scan job", "error", err)
			continue
		}

		// Atomically claim the job. Only succeeds if still pending (avoids
		// double-dispatch when multiple workers poll concurrently).
		result, err := p.db.ExecContext(ctx, `
			UPDATE task_queue
			SET status = 'running', started_at = now()
			WHERE job_id = $1 AND tenant_id = $2 AND queue_name = $3 AND status = 'pending'
		`, jobID, tenantID, queueName)
		if err != nil {
			p.logger.Error("jobqueue: claim job", "job_id", jobID, "error", err)
			continue
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			// Another worker claimed it; skip.
			continue
		}

		p.logger.Info("jobqueue: would dispatch job",
			"job_id", jobID,
			"queue", queueName,
			"tenant", tenantID,
		)
	}

	return rows.Err()
}

