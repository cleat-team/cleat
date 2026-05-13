package scheduler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/rcownie/cleat/internal/plugin"
)

// dueSchedulesQuery provides dialect-specific FOR UPDATE SKIP LOCKED equivalents.
var dueSchedulesQuery = plugin.Query{
	Default: `
		SELECT id, tenant_id, name, cron, workflow_name, input, next_run_at
		FROM schedules
		WHERE enabled = true AND next_run_at <= now()
		FOR UPDATE SKIP LOCKED`,
	MySQL: `
		SELECT id, tenant_id, name, cron, workflow_name, input, next_run_at
		FROM schedules
		WHERE enabled = true AND next_run_at <= NOW()
		FOR UPDATE SKIP LOCKED`,
	MSSQL: `
		SELECT id, tenant_id, name, cron, workflow_name, input, next_run_at
		FROM schedules WITH (UPDLOCK, READPAST, ROWLOCK)
		WHERE enabled = true AND next_run_at <= now()`,
}

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

// dueSchedule holds a schedule row claimed from the database.
type dueSchedule struct {
	id           uuid.UUID
	tenantID     uuid.UUID
	name         string
	cron         string
	workflowName string
	input        []byte
}

// runDueSchedules finds schedules where enabled=true AND next_run_at <= now(),
// atomically claims them via FOR UPDATE SKIP LOCKED inside a transaction, then
// starts the corresponding workflows.  The transaction is committed before
// StartWorkflow is called so that row locks are not held across external calls.
// Returns (schedulesDue, workflowsStarted, workflowsFailed).
func (p *Plugin) runDueSchedules(ctx context.Context) (int, int, int) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		p.logger.Error("scheduler: begin transaction", "error", err)
		return 0, 0, 0
	}
	defer tx.Rollback() // no-op after Commit

	rows, err := tx.Query(ctx, plugin.Rebind(dueSchedulesQuery.For(p.dialect), p.dialect))
	if err != nil {
		p.logger.Error("scheduler: query due schedules", "error", err)
		return 0, 0, 0
	}

	var due []dueSchedule
	for rows.Next() {
		var s dueSchedule
		var nextRunAt *time.Time
		if err := rows.Scan(&s.id, &s.tenantID, &s.name, &s.cron,
			&s.workflowName, &s.input, &nextRunAt); err != nil {
			p.logger.Error("scheduler: scan due schedule", "error", err)
			continue
		}
		due = append(due, s)

		// Advance next_run_at under the transaction lock so other
		// workers skip this row even if this worker crashes before
		// calling StartWorkflow.
		now := time.Now()
		next := nextRun(s.cron, now)
		var nextRunAtUpdate *time.Time
		if !next.IsZero() {
			nextRunAtUpdate = &next
		}
		if _, err := tx.Exec(ctx, plugin.Rebind(`
			UPDATE schedules
			SET last_run_at = $1, next_run_at = $2, updated_at = now()
			WHERE id = $3
		`, p.dialect), now, nextRunAtUpdate, s.id); err != nil {
			p.logger.Error("scheduler: update schedule after claim",
				"id", s.id, "error", err)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		p.logger.Error("scheduler: rows iteration error", "error", err)
	}

	if err := tx.Commit(); err != nil {
		p.logger.Error("scheduler: commit transaction", "error", err)
		return 0, 0, 0
	}

	// Start workflows outside the transaction so locks are not held
	// across potentially slow external calls.
	var schedulesDue, workflowsStarted, workflowsFailed int
	for _, s := range due {
		schedulesDue++
		p.logger.Info("scheduler: triggering schedule",
			"id", s.id, "tenant", s.tenantID,
			"name", s.name, "workflow", s.workflowName)

		runID, startErr := p.env.StartWorkflow(ctx, s.workflowName, json.RawMessage(s.input))
		if startErr != nil {
			p.logger.Error("scheduler: start workflow failed",
				"id", s.id, "name", s.name,
				"workflow", s.workflowName, "error", startErr)
			workflowsFailed++
		} else {
			workflowsStarted++
			p.logger.Info("scheduler: workflow started",
				"id", s.id, "name", s.name,
				"workflow", s.workflowName, "run_id", runID)
		}
	}

	return schedulesDue, workflowsStarted, workflowsFailed
}
