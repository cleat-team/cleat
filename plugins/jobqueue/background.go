package jobqueue

import (
	"context"
	"encoding/json"
	"fmt"
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

// abandonedJobsQuery finds jobs stuck in "dispatched" whose run is no longer
// in flight -- the run finished (or was reaped, terminated, cancelled or
// dead-lettered by a path other than FinalizeWorkflowSegment) and no
// ObserveFinalize write-back ever arrived to say what happened. cleat#1715.
//
// SAME SHAPE AS plugins/blobstore/queries.go's staleWorkflowRefs on the two
// dialects that share its arm: admin.in_flight_workflow_ids() (migration 073)
// on PostgreSQL, because workflow_instances there carries RLS that a
// tenant-less background sweep cannot satisfy directly and that function is
// the SECURITY DEFINER, no-argument, RLS-exempt escape hatch built for
// exactly this. MySQL reads workflow_instances directly -- it has no RLS on
// that table, so it never needed the function.
//
// SQL SERVER TAKES MYSQL'S SHAPE, NOT POSTGRES'S, cleat#2125 -- unlike
// staleWorkflowRefs, not because no single statement can express it, but
// because task_queue (unlike workflow_blob_refs) IS declared TenantScoped.
// sweepAbandonedJobsPerTenant below loops this same statement once per
// tenant instead of impersonating a bypass principal: task_queue's own
// row-level security scopes the UPDATE, and dbo.fn_tenant_filter scopes the
// subquery, to the SAME tenant each time round, so nothing here needs
// gathering across tenants first the way blobstore's does -- and with a
// tenant in SESSION_CONTEXT for every call, reading workflow_instances
// directly is exactly as safe here as MySQL's arm already is, which has no
// RLS to work around in the first place. This is not the mistake
// staleWorkflowRefs's doc warns about: that one read under
// AcrossAllTenants, which sets no tenant at all.
//
// Marked ABANDONED, not "failed" and not "completed" -- see plugin.go's
// design note (cleat#1715's design decision) for why: this sweep has no
// evidence about the run's actual outcome, only that it is gone. Asserting
// either "completed" or "failed" here would be the exact same unsupported
// claim this issue exists to remove, moved one state to the right.
var abandonedJobsQuery = plugin.Query{
	Default: `UPDATE task_queue
SET status = 'abandoned', completed_at = now()
WHERE status = 'dispatched'
  AND run_id NOT IN (SELECT id FROM admin.in_flight_workflow_ids())`,
	MySQL: `UPDATE task_queue
SET status = 'abandoned', completed_at = now()
WHERE status = 'dispatched'
  AND run_id NOT IN (SELECT id FROM workflow_instances WHERE status IN ('ready', 'running'))`,
	MSSQL: `UPDATE task_queue
SET status = 'abandoned', completed_at = SYSUTCDATETIME()
WHERE status = 'dispatched'
  AND run_id NOT IN (SELECT id FROM workflow_instances WHERE status IN ('ready', 'running'))`,
}

// sweepAbandonedJobs marks abandoned jobs whose run vanished with no recorded
// outcome. Returns the number swept, or -1 on error (logged internally).
//
// A POSITIVE CONTROL BELONGS WITH EVERY CALLER OF THIS FUNCTION, not just
// with this function's own test. This reaper has silently no-opped twice
// before on this exact table (cleat#1133, #1134, #1141) -- "the reaper ran
// without error" and "the reaper reaped anything" are different claims, and
// only a caller that engineers a real abandoned row and checks the count
// went to 1 can tell them apart. See background_test.go.
//
// ctx must carry NO AcrossAllTenants marker on SQL Server: this loops
// plugin.ForTenant per tenant there, and ForTenant on top of an
// already-cross-tenant ctx is a no-op (beginTenantTx checks the bypass
// first). Run passes its unmarked base context here for exactly that
// reason, even though every other call in this file gets the marked one.
func (p *Plugin) sweepAbandonedJobs(ctx context.Context) int {
	if p.dialect == plugin.DialectMSSQL {
		return p.sweepAbandonedJobsPerTenant(ctx)
	}
	across := plugin.AcrossAllTenants(ctx,
		"jobqueue abandonment sweep: run_id visibility spans every tenant")
	n, err := p.db.Exec(across, plugin.Rebind(abandonedJobsQuery.For(p.dialect), p.dialect))
	if err != nil {
		p.logger.Error("jobqueue: abandonment sweep failed",
			"plugin", p.Info().Name,
			"error", err,
		)
		return -1
	}
	return int(n)
}

// sweepAbandonedJobsPerTenant is SQL Server's half of cleat#2125. See
// abandonedJobsQuery's doc for why a per-tenant loop is safe here in a way it
// is not for blobstore's staleWorkflowRefs: task_queue is TenantScoped, so
// looping the whole statement scopes both the UPDATE and its subquery to one
// tenant at a time, with nothing left unscoped in between.
//
// ctx MUST NOT carry AcrossAllTenants's marker -- see plugin.IsCrossTenant's
// doc for why a marked ctx makes every ForTenant below a silent no-op that
// runs each "per-tenant" statement under the bypass instead, against every
// tenant. Checked here rather than trusted, because the failure is a
// one-token slip in a caller (ctx instead of baseCtx) that every test using a
// correctly-built context would miss. cleat#2141,
// TestSweepAbandonedJobsPerTenant_RejectsACrossTenantContext.
func (p *Plugin) sweepAbandonedJobsPerTenant(ctx context.Context) int {
	if plugin.IsCrossTenant(ctx) {
		p.logger.Error("jobqueue: abandonment sweep: ctx is cross-tenant-marked; "+
			"ForTenant on top of it would be a silent no-op and every tenant's "+
			"statement would run under the bypass instead",
			"plugin", p.Info().Name)
		return -1
	}
	tenants, err := plugin.AllTenantIDs(ctx, p.db, p.dialect)
	if err != nil {
		p.logger.Error("jobqueue: abandonment sweep: list tenants failed",
			"plugin", p.Info().Name, "error", err)
		return -1
	}
	total := 0
	for _, tid := range tenants {
		id, perr := uuid.Parse(tid)
		if perr != nil {
			p.logger.Error("jobqueue: abandonment sweep: tenant id is not a UUID",
				"plugin", p.Info().Name, "tenant_id", tid, "error", perr)
			return -1
		}
		tctx := plugin.ForTenant(ctx, id)
		n, err := p.db.Exec(tctx, plugin.Rebind(abandonedJobsQuery.For(p.dialect), p.dialect))
		if err != nil {
			p.logger.Error("jobqueue: abandonment sweep failed for tenant",
				"plugin", p.Info().Name, "tenant_id", tid, "error", err)
			return -1
		}
		total += int(n)
	}
	return total
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

	// baseCtx carries no tenant marker at all -- kept for sweepAbandonedJobs,
	// which on SQL Server (cleat#2125) layers plugin.ForTenant on top of it
	// per tenant. beginTenantTx checks for a cross-tenant marker before it
	// checks for a tenant, so ForTenant on top of the AcrossAllTenants-marked
	// ctx below would be a no-op: the bypass wins.
	baseCtx := ctx

	// Mark the whole background loop cross-tenant. cleat#1278.
	//
	// task_queue became tenant-scoped in migration version 3, and that policy
	// RAISES when no tenant is set rather than returning fewer rows. A tenant
	// reaches this plugin only through the HTTP middleware, so nothing below
	// this line has a tenant in context and none of it can obtain one -- the
	// poller and the reaper serve every tenant's queue by definition. Without
	// this the loop does not degrade, it fails outright on its first statement.
	//
	// Marked ONCE here rather than at the six call sites it covers, because
	// every statement reachable from this function is cross-tenant for the
	// same reason -- except sweepAbandonedJobs's SQL Server arm, which takes
	// baseCtx instead and marks (or scopes) its own statements itself. The
	// four handlers in routes.go are deliberately NOT marked: they run on
	// r.Context(), which carries the request's tenant, and marking them would
	// silently widen a per-tenant read to every tenant -- the exact class of
	// answer this mechanism exists to make impossible.
	ctx = plugin.AcrossAllTenants(ctx,
		"jobqueue background worker: the poller and the stuck-job reaper both operate on every tenant's queue")

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
	// Same ticker, a second sweep. cleat#1715 asked for the abandonment
	// sweep to extend the reaper rather than add a goroutine -- this is
	// that: no new ticker, no new mechanism, one more statement per cycle.
	if n := p.sweepAbandonedJobs(baseCtx); n >= 0 {
		p.logger.Info("jobqueue: initial abandonment sweep completed",
			"plugin", p.Info().Name,
			"jobs_abandoned", n,
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
			if n := p.sweepAbandonedJobs(baseCtx); n >= 0 {
				p.logger.Info("jobqueue: abandonment sweep completed",
					"plugin", p.Info().Name,
					"duration_ms", time.Since(start).Milliseconds(),
					"jobs_abandoned", n,
				)
			}
		}
	}
}

// pollPending scans for pending jobs and dispatches them. For each pending job,
// it atomically claims the job (status -> 'running') and dispatches the
// referenced workflow to status -> 'dispatched'; a job's terminal status
// ('completed' or 'failed') is written back later, by ObserveFinalize, when
// the workflow it started actually finishes. cleat#1715. Jobs without a
// def_name have no run to wait for and are marked completed immediately.
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
			inputCol  plugin.JSONColumn
		)
		// plugin.JSONColumn, not a bare json.RawMessage: json.RawMessage is a
		// named []byte type, and database/sql's convertAssign fast path
		// doesn't convert a driver string into one -- go-mssqldb returns
		// NVARCHAR as string, so this failed on every row, NULL included,
		// and SQL Server never dispatched a job. See plugin.JSONColumn.
		// cleat#2206.
		if err := plugin.ScanRow(rows, &tenantID, &queueName, &jobID, &payload, &defName, &inputCol); err != nil {
			p.logger.Error("jobqueue: scan job", "error", err)
			continue
		}
		input := inputCol.Raw

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

			// KEYED ON THE JOB ROW. The claim above is an UPDATE guarded by
			// rowsAffected, so only one worker reaches here per job -- but a
			// crash between that claim and this call, or a retry of the
			// dispatch, would otherwise start the job's workflow twice. The
			// job id names the unit of work and is stable across both.
			// cleat#1555.
			req := plugin.StartRequest{
				DefName:        *defName,
				Input:          input,
				IdempotencyKey: fmt.Sprintf("jobqueue:%s", jobID),
				TenantID:       tenantID.String(),
			}
			runID, err := p.env.StartWorkflow(ctx, req)
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

			// "dispatched", not "completed" -- cleat#1715. The workflow was
			// STARTED here; whether it succeeds is unknown until
			// ObserveFinalize (plugin.go) writes back "completed" or
			// "failed", or the abandonment sweep (below) concludes the run
			// is gone with no write-back ever having arrived. completed_at
			// is not set here for the same reason: it means when the run
			// actually finished, and that is not this moment.
			// $1 (run_id) before $2-$4 (WHERE): MySQL's ? binds by TEXT
			// APPEARANCE, not by number (CLAUDE.md), so the placeholder
			// numbers here must ascend in the order they are WRITTEN, not in
			// the order the WHERE clause would read more naturally. This
			// UPDATE previously numbered run_id $4, after job_id/tenant_id/
			// queue_name at $1-$3 in the WHERE clause below it -- correct on
			// Postgres and MSSQL, which bind $N/@pN by number, but on MySQL
			// the rebound query's four "?" get these four args in POSITIONAL
			// order regardless: run_id took jobID's value, job_id took
			// tenantID's, and so on. The WHERE clause then matched no row,
			// and an UPDATE matching zero rows is not an error, so the job
			// stayed "running" forever on MySQL. Found by cleat#2257's
			// real-dialect pollPending test.
			if _, updateErr := p.db.Exec(ctx, plugin.Rebind(`
					UPDATE task_queue
					SET status = 'dispatched', run_id = $1
					WHERE job_id = $2 AND tenant_id = $3 AND queue_name = $4
				`, p.dialect), runID, jobID, tenantID, queueName); updateErr != nil {
				p.logger.Error("jobqueue: mark dispatched", "job_id", jobID, "error", updateErr)
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
