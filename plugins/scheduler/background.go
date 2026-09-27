package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
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
	// enabled is BIT here, and T-SQL has no boolean literal: a bare `true` parses
	// as an identifier and the query fails with "Invalid column name 'true'".
	// now() needs no such treatment -- plugin.Rebind rewrites it to
	// SYSUTCDATETIME() on this dialect, which is why only the boolean was wrong.
	MSSQL: `
		SELECT id, tenant_id, name, cron, workflow_name, input, next_run_at
		FROM schedules WITH (UPDLOCK, READPAST, ROWLOCK)
		WHERE enabled = 1 AND next_run_at <= now()`,
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

	// dueAt is the occurrence this claim represents -- the next_run_at that
	// was already in the row, not the time the sweep noticed it.
	//
	// IT IS THE IDEMPOTENCY KEY'S SECOND HALF and that is the only reason it
	// is kept: the column was always selected and scanned into a local that
	// was thrown away. Keying on the dispatch clock instead would produce a
	// different key on every retry, which deduplicates nothing and looks
	// exactly like having no key at all. cleat#1555.
	dueAt time.Time
}

// scheduleStartKey names one firing of one schedule.
//
// A NAMED FUNCTION SO THE PROPERTY IS TESTABLE. What makes this key worth
// having is that it is IDENTICAL for two dispatches of the same occurrence and
// DIFFERENT for consecutive occurrences of the same schedule. Written inline it
// is a format string nobody can assert on; the failure it guards -- a key built
// from the dispatch clock, which differs on every attempt -- deduplicates
// nothing and is invisible, because duplicate runs look exactly as they do with
// no key at all. cleat#1555.
//
// UTC and UnixNano rather than a formatted time: the same instant must produce
// the same key regardless of the session time zone a worker happens to hold.
func scheduleStartKey(id uuid.UUID, dueAt time.Time) string {
	return fmt.Sprintf("scheduler:%s:%d", id, dueAt.UTC().UnixNano())
}

// runDueSchedules finds schedules where enabled=true AND next_run_at <= now(),
// atomically claims them via FOR UPDATE SKIP LOCKED inside a transaction, then
// starts the corresponding workflows.  The transaction is committed before
// StartWorkflow is called so that row locks are not held across external calls.
// Returns (schedulesDue, workflowsStarted, workflowsFailed).
func (p *Plugin) runDueSchedules(ctx context.Context) (int, int, int) {
	// CROSS-TENANT, and unlike eventtriggers' retry loop this one cannot be
	// narrowed per row. cleat#1512.
	//
	// The discrimination used elsewhere in this rollout is "no tenant to be
	// had is a bypass; a tenant that went missing is not" -- and on a first
	// reading this looks like the second case, because every row scanned
	// carries a tenant_id and could be handled under plugin.ForTenant.
	//
	// It is not, because the SCAN AND THE UPDATES SHARE ONE TRANSACTION and
	// that is load-bearing. The claim is FOR UPDATE SKIP LOCKED, and
	// next_run_at is advanced under the same transaction lock so that another
	// worker skips these rows even if this one crashes before StartWorkflow.
	// Splitting the updates into per-tenant transactions would release the
	// lock between claiming and advancing, which is the property the lock
	// exists to provide. The comment further down records what happened the
	// last time the shape of this transaction was got wrong: every update was
	// lost, on every dialect, and schedules fired on every poll regardless of
	// their cron.
	//
	// So the unit of work genuinely spans tenants, and the bypass is the
	// honest description rather than the convenient one. The cost is that the
	// UPDATE ... WHERE id = $3 below is not policy-checked -- acceptable here
	// because the id comes from the SELECT in this same transaction rather
	// than from a caller.
	ctx = plugin.AcrossAllTenants(ctx,
		"scheduler due-schedule claim: one FOR UPDATE SKIP LOCKED transaction claims and advances every tenant's due rows together")

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
		// plugin.GUID, not uuid.UUID: SQL Server returns UNIQUEIDENTIFIER in
		// mixed-endian byte order, which scans without error into a different
		// id. See its doc comment.
		var id, tenantID plugin.GUID
		if err := rows.Scan(&id, &tenantID, &s.name, &s.cron,
			&s.workflowName, &s.input, &nextRunAt); err != nil {
			p.logger.Error("scheduler: scan due schedule", "error", err)
			continue
		}
		s.id, s.tenantID = id.UUID, tenantID.UUID
		if nextRunAt != nil {
			s.dueAt = *nextRunAt
		}
		due = append(due, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		p.logger.Error("scheduler: rows iteration error", "error", err)
	}

	// Advance next_run_at under the transaction lock so other workers skip
	// these rows even if this worker crashes before calling StartWorkflow.
	//
	// This runs after the cursor is drained and closed, and used to run inside
	// the scan loop above. A transaction holds a single connection, so issuing
	// an Exec on it while its own Rows are still open fails outright -- "pq:
	// there is already a query being processed on this connection". Every one
	// of these updates was therefore lost, on every dialect, and the error went
	// to the log and nowhere else. A schedule that is never advanced stays due,
	// so it fired again on the next poll and its cron expression had no effect
	// on how often it ran.
	for _, s := range due {
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

		// KEYED ON THE OCCURRENCE, NOT THE ATTEMPT. (schedule id, due time)
		// names this firing uniquely and identically on every retry of it, so
		// a crash between the commit above and this call no longer costs the
		// run: the retry presents the same key and the engine returns the
		// existing instance instead of creating a second.
		//
		// The commit-before-start ordering is deliberate and documented above;
		// it traded a lost run for never double-firing, because without a key
		// those were the only two options. cleat#1555.
		req := plugin.StartRequest{
			DefName:        s.workflowName,
			Input:          json.RawMessage(s.input),
			IdempotencyKey: scheduleStartKey(s.id, s.dueAt),
			TenantID:       s.tenantID.String(),
		}
		runID, startErr := p.env.StartWorkflow(ctx, req)
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
