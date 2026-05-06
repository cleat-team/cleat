package scheduler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Run starts the background scheduler loop. Every 60 seconds it queries the
// schedules table for enabled schedules whose next_run_at <= now() and
// triggers the corresponding workflow. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("scheduler: no database, background loop disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	p.logger.Info("scheduler: background loop started, interval=60s")

	// Run once immediately on startup.
	start := time.Now()
	schedulesDue, workflowsStarted, workflowsFailed := p.runDueSchedules(ctx)
	p.logger.Info("scheduler: work cycle completed",
		"plugin", p.Info().Name,
		"duration_ms", time.Since(start).Milliseconds(),
		"schedules_due", schedulesDue,
		"workflows_started", workflowsStarted,
		"workflows_failed", workflowsFailed,
	)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("scheduler: background loop stopped")
			return nil

		case <-ticker.C:
			start := time.Now()
			schedulesDue, workflowsStarted, workflowsFailed := p.runDueSchedules(ctx)
			p.logger.Info("scheduler: work cycle completed",
				"plugin", p.Info().Name,
				"duration_ms", time.Since(start).Milliseconds(),
				"schedules_due", schedulesDue,
				"workflows_started", workflowsStarted,
				"workflows_failed", workflowsFailed,
			)
		}
	}
}

// runDueSchedules finds schedules where enabled=true AND next_run_at <= now()
// and triggers the corresponding workflow. After triggering, it calculates
// the next future run from the cron expression and updates next_run_at and
// last_run_at. Returns (schedulesDue, workflowsStarted, workflowsFailed).
func (p *Plugin) runDueSchedules(ctx context.Context) (int, int, int) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, cron, workflow_name, input, next_run_at
		FROM schedules
		WHERE enabled = true AND next_run_at <= now()
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		p.logger.Error("scheduler: query due schedules", "error", err)
		return 0, 0, 0
	}
	defer rows.Close()

	var schedulesDue, workflowsStarted, workflowsFailed int

	for rows.Next() {
		var (
			id           uuid.UUID
			tenantID     uuid.UUID
			name         string
			cron         string
			workflowName string
			input        []byte
			nextRunAt    *time.Time
		)
		if err := rows.Scan(&id, &tenantID, &name, &cron, &workflowName, &input, &nextRunAt); err != nil {
			p.logger.Error("scheduler: scan due schedule", "error", err)
			continue
		}

		schedulesDue++

		p.logger.Info("scheduler: triggering schedule",
			"id", id,
			"tenant", tenantID,
			"name", name,
			"workflow", workflowName,
			"input", string(input),
		)

		// Actually start the workflow run.
		runID, startErr := p.env.StartWorkflow(ctx, workflowName, json.RawMessage(input))
		if startErr != nil {
			p.logger.Error("scheduler: start workflow failed",
				"id", id,
				"name", name,
				"workflow", workflowName,
				"error", startErr,
			)
			workflowsFailed++
			// Continue to update next_run_at so a single bad schedule
			// does not block the background worker.
		} else {
			workflowsStarted++
			p.logger.Info("scheduler: workflow started",
				"id", id,
				"name", name,
				"workflow", workflowName,
				"run_id", runID,
			)
		}

		// Calculate the next run from the cron expression.
		// Use the current time as the baseline. If the cron expression cannot
		// produce a future match, leave next_run_at as NULL (schedule will
		// no longer fire until updated).
		now := time.Now()
		next := nextRun(cron, now)
		var nextRunAtUpdate *time.Time
		if !next.IsZero() {
			nextRunAtUpdate = &next
		}

		_, err := p.db.ExecContext(ctx, `
			UPDATE schedules
			SET last_run_at = $1, next_run_at = $2, updated_at = now()
			WHERE id = $3
		`, now, nextRunAtUpdate, id)
		if err != nil {
			p.logger.Error("scheduler: update schedule after trigger",
				"id", id, "error", err)
			continue
		}

		p.logger.Info("scheduler: completed trigger",
			"id", id,
			"name", name,
			"next_run_at", next,
		)
	}

	if err := rows.Err(); err != nil {
		p.logger.Error("scheduler: rows iteration error", "error", err)
	}

	return schedulesDue, workflowsStarted, workflowsFailed
}
