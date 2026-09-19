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
// finalStatus is "done" or "failed" -- the two terminal statuses
// FinalizeWorkflowSegment can report; "ready" (suspend) never reaches here,
// filtered by the caller (cmd/cleat-worker/setup.go).
//
// status IN ('dispatched', 'abandoned') IN THE WHERE CLAUSE, not just
// 'dispatched': a genuine, authoritative outcome arriving late -- after the
// abandonment sweep already gave up and marked the row 'abandoned' on an
// inference from absence -- is stronger evidence than that inference and is
// allowed to overwrite it. It is NOT allowed to overwrite an existing
// 'completed' or 'failed': ObserveFinalize is called at most once per run in
// the ordinary path, but a crash-and-retry of the caller must not be able to
// re-fire this and flip an already-recorded outcome.
func (p *Plugin) ObserveFinalize(ctx context.Context, runID, finalStatus string) error {
	jqStatus := "failed"
	if finalStatus == "done" {
		jqStatus = "completed"
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
