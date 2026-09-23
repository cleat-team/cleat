package jobqueue

import (
	"context"

	"github.com/cleat-team/cleat/plugin"
)

// ObserveFinalize implements plugin.HasFinalizeObserver. cleat#1715.
//
// Writes back the run's ACTUAL outcome onto the job row that started it, so
// "dispatched" (background.go's write at dispatch time) does not stand in
// for "succeeded" forever. A no-op for every run this plugin did not start:
// the WHERE clause matches on run_id, and nothing but a job this plugin
// dispatched ever has this run's id in that column.
//
// finalStatus is one of the five terminal values workflow_instances.status
// actually takes in production (engine/store_lifecycle.go's own census):
// "done", "failed", "dead_lettered", "terminated", "cancelled" -- "ready"
// (suspend) never reaches here, filtered by the caller
// (cmd/cleat-worker/setup.go). Before cleat#1976, only "done" and "failed"
// ever arrived; the other three terminal paths never called this at all, so
// jobqueue's abandonment sweep -- not this write-back -- was what eventually
// recovered a job whose run failed, cancelled or was force-terminated, and it
// recovered it as 'abandoned' rather than the run's real outcome.
//
// jqStatus maps 1:1 for "done" and "dead_lettered" -- the latter because an
// operator asking "did this job's run fail or is it sitting in the
// dead-letter queue for redrive?" needs the distinction task_queue.status is
// for, and jqStatus is a free-form TEXT column with no CHECK constraint, so
// adding a third value costs no migration. Everything else -- "failed",
// "terminated", "cancelled" -- buckets to "failed": a job whose run was
// force-cancelled or force-terminated by an operator did not complete, and
// task_queue has no separate lifecycle for those two, unlike
// workflow_instances. That bucketing is a decision, not a gap: cleat#1976
// left it open for the implementer to make and state.
func (p *Plugin) ObserveFinalize(ctx context.Context, runID, finalStatus string) error {
	jqStatus := "failed"
	switch finalStatus {
	case "done":
		jqStatus = "completed"
	case "dead_lettered":
		jqStatus = "dead_lettered"
	}

	ctx = plugin.AcrossAllTenants(ctx,
		"jobqueue finalize observer: the run that just finished names its job by run_id, not by the tenant this call happens to have in context (there usually is none -- this fires from the worker's own finalize path, not from an HTTP request)")

	// $1 IS status AND $2 IS run_id, matching the order each appears in the
	// text below -- not the more natural runID-then-status. MySQL's arm of
	// plugin.Rebind erases the number and binds each `?` by TEXT POSITION,
	// so a query whose numbering disagrees with its own appearance order
	// binds correctly on PostgreSQL and SQL Server (both keep $N/@pN as
	// named placeholders) and silently swaps the two arguments on MySQL
	// alone. Measured: with $2 written first (`SET status = $2 ... WHERE
	// run_id = $1`) and args passed (runID, jqStatus), the MySQL arm of
	// TestObserveFinalize_MultiBackend left the row unchanged -- the UPDATE
	// ran, matched zero rows, and returned no error, because run_id was
	// being compared against jqStatus and status was being set to runID.
	_, err := p.db.Exec(ctx, plugin.Rebind(`
		UPDATE task_queue
		SET status = $1, completed_at = now()
		WHERE run_id = $2 AND status IN ('dispatched', 'abandoned')
	`, p.dialect), jqStatus, runID)
	return err
}
