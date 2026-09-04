package engine

// Admin force-resolve: taking a workflow out of a stuck state by operator
// action rather than by the worker that owns it.
//
// Until this file existed, AdminForceComplete and AdminForceFail returned
// "not implemented yet" on all three dialects while cmd/cleat-worker routed
// POST /api/admin/instances/{id}/force-{complete,fail} to them. The route,
// its X-Confirm guard and its ownership check were all real; the operation
// underneath them was not. See IMPROVEMENT-PLAN.md 3.20.
//
// The three implementations are kept side by side rather than one per dialect
// file, because the interesting content is what differs between them: the
// placeholder syntax, the clock function, and MSSQL's rollback-guaranteed
// retry. Splitting them across three files is how the schema definitions in
// 2.60 drifted apart.
//
// # What a force-resolve is
//
// Four things, in one transaction:
//
//  1. A terminal status write fenced on generation, but NOT on assigned_to.
//     That is the entire point: the workflow being force-resolved is usually
//     one whose worker is gone or wedged, so there is no owner to match.
//     Generation is what makes a stale operator request fail rather than
//     silently resolve a workflow that has since moved on.
//  2. A generation bump, so the write fences off any worker that still
//     believes it owns the run -- ReapStaleInstances bumps for the same
//     reason. assigned_to = NULL alone would already make CompleteWorkflow
//     fail, but the bump also stops a heartbeat or segment finalize.
//  3. An admin_action event appended to the workflow's history, in the same
//     transaction and through the same appendEventsInTx the engine uses, so
//     the audit record joins the checksum chain instead of sitting beside it.
//  4. The same post-commit cleanup a normal terminal write does: sticky
//     worker, concurrency keys, parent close policy.
//
// force-complete also clears error_msg / error_code / error_op. A workflow
// that has already failed can be force-completed -- that is a repair an
// operator is entitled to make -- and leaving the old failure on a row now
// marked done produces a state nothing else in the engine can create, which
// every reader of those columns would have to know to ignore. The reverse is
// not symmetric: force-fail leaves result alone, because a result that was
// genuinely produced is still a fact about the run.
//
// # Tenant scoping
//
// Every statement here filters tenant_id explicitly, on all three dialects.
// PostgreSQL's RLS would cover the first of them, but engine tests connect as
// the owner and RLS is bypassed for superusers -- so on a PostgreSQL-only
// enforcement the cross-tenant test in admin_force_resolve_test.go would pass
// against a store that has no filter at all. MySQL and SQL Server have no RLS
// under them at all (1.7 residual). The Go-level filter is the layer the test
// exercises; on PostgreSQL, RLS is a second one under it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	adminActionForceComplete = "force_complete"
	adminActionForceFail     = "force_fail"
	adminActionReReplay      = "re_replay"
)

// reReplayableStatuses are the terminal statuses a workflow can be re-replayed
// out of: it stopped with work left, and resetting it to 'ready' resumes from
// recorded history rather than starting over.
//
// 'done' is excluded deliberately. Replay would walk a complete history to its
// end and finalize again, doing nothing but writing a second terminal
// transition. An operator who wants a finished workflow to run again wants a
// new run -- which is what the dead-letter reprocess path does
// (cmd/cleat-worker/app.go), from the definition and input rather than from
// history. The two are different operations and this is the one that preserves
// completed steps.
//
// The non-terminal statuses are excluded because the dispatcher already owns
// them: re-replaying a 'ready' or 'running' workflow would bump its generation
// out from under whichever worker holds it.
var reReplayableStatuses = []string{"failed", "terminated", "dead_lettered"}

// ErrAdminOpNotImplemented marks an admin operation the store genuinely does
// not implement, as opposed to one that failed.
//
// The distinction is the whole reason it exists: cmd/cleat-worker mapped every
// error from these methods to 500, so "this endpoint was never built" and "the
// database is broken" were the same answer to a caller.
//
// **No store in this repo returns it any more.** It was introduced for
// AdminReReplay, which was a stub on all three dialects; that body landed with
// IMPROVEMENT-PLAN 3.20's third piece, so force-complete, force-fail and
// re-replay are all real now. The error and handleAdminOpError's 501 branch are
// kept because WorkflowStore is a public interface: an out-of-tree store that
// implements some of it and not the rest has the same problem this solved, and
// 501 is still the honest answer for it.
var ErrAdminOpNotImplemented = errors.New("not implemented")

// adminForce is one force-resolve request: the terminal state to write, and
// the audit event to record beside it.
type adminForce struct {
	action    string // adminActionForceComplete | adminActionForceFail
	result    string // force-complete only; must be valid JSON (see ForceComplete)
	errorMsg  string // force-fail only
	errorCode string // force-fail only
	operator  string
}

// reason is the human-readable detail stored on the audit event.
func (a adminForce) reason() string {
	if a.action != adminActionForceFail {
		return ""
	}
	if a.errorCode != "" && a.errorMsg != "" {
		return a.errorCode + ": " + a.errorMsg
	}
	if a.errorCode != "" {
		return a.errorCode
	}
	return a.errorMsg
}

// auditEvent builds the admin_action record appended to the workflow's
// history. It goes through EventRecordFromEvent rather than being assembled
// by hand so the operator/action/reason mapping stays in one place -- the
// same one EventFromRecord reverses when the history is read back.
func (a adminForce) auditEvent(step int) EventRecord {
	rec := EventRecordFromEvent(AdminActionEvent{
		step:     step,
		Action:   a.action,
		Operator: a.operator,
		Reason:   a.reason(),
	})
	rec.TimestampMs = time.Now().UnixMilli()
	return rec
}

// adminNotFound and adminGenerationMismatch are the two outcomes a zero-row
// UPDATE has to be resolved into. The HTTP layer maps them to 404 and 409 by
// substring (handleAdminOpError), so the wording is load-bearing.
func adminNotFound(action, workflowID string) error {
	return fmt.Errorf("admin %s: workflow %s not found", action, workflowID)
}

func adminGenerationMismatch(action, workflowID string, stored, requested int64) error {
	return fmt.Errorf("admin %s: generation mismatch for workflow %s: stored generation is %d, request carried %d",
		action, workflowID, stored, requested)
}

// adminAuditCollision is returned when the audit event could not be appended
// because another writer took the step number first.
//
// It matters because the alternative is silent: every dialect's append is an
// upsert that leaves an existing row alone -- PostgreSQL's ON CONFLICT clause,
// which updates only where the stored response is the empty string and error
// IS NULL, is the clearest case -- so a collision would otherwise commit the
// status change with no audit record and no error. The whole force-resolve is
// rolled back instead. On the workflows this operation is actually used on --
// ones whose worker is gone -- there is no concurrent writer and this never
// fires.
func adminAuditCollision(action, workflowID string, step int) error {
	return fmt.Errorf("admin %s: audit event for workflow %s step %d was displaced by a concurrent writer; "+
		"the force-resolve was rolled back rather than applied without an audit record",
		action, workflowID, step)
}

// ---------------------------------------------------------------------------
// PostgreSQL
// ---------------------------------------------------------------------------

// AdminForceComplete marks a workflow as done, bypassing worker ownership.
func (s *PostgresStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	return s.adminForceResolve(ctx, workflowID, generation, adminForce{
		action: adminActionForceComplete, result: result, operator: operator,
	})
}

// AdminForceFail marks a workflow as failed, bypassing worker ownership.
func (s *PostgresStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	return s.adminForceResolve(ctx, workflowID, generation, adminForce{
		action: adminActionForceFail, errorMsg: errorMsg, errorCode: errorCode, operator: operator,
	})
}

// Every direct terminal UPDATE below clears pending_terminal_status and
// defer_phase_deadline, and that is not tidiness. A force-resolve can land on a
// workflow that is in its defer phase, and a marker left behind outlives the
// row's new status: ExpireDeferPhases sweeps on `pending_terminal_status IS NOT
// NULL AND defer_phase_deadline < now()`, so past the deadline it would apply
// the OLD recorded outcome over the operator's. Force-complete a terminating
// workflow, watch it become 'terminated' five minutes later.
//
// The same clearing is on TerminateWorkflow's one-phase arm and the parent-close
// plain TERMINATE arm, for the same reason. IMPROVEMENT-PLAN 3.112 and 3.114.
func (s *PostgresStore) adminForceResolve(ctx context.Context, workflowID string, generation int64, a adminForce) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("admin %s: begin: %w", a.action, err)
	}
	defer tx.Rollback()

	var res sql.Result
	if a.action == adminActionForceComplete {
		res, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'done', result = $3, completed_at = now(),
			    error_msg = NULL, error_code = NULL, error_op = NULL,
			    assigned_to = NULL, generation = generation + 1,
			    pending_terminal_status = NULL, defer_phase_deadline = NULL
			WHERE id = $1 AND tenant_id = $2 AND generation = $4
		`, workflowID, s.tenantID, a.result, generation)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'failed', error_msg = $3, error_code = $4, error_op = 'admin_force_fail',
			    completed_at = now(), assigned_to = NULL, generation = generation + 1,
			    pending_terminal_status = NULL, defer_phase_deadline = NULL
			WHERE id = $1 AND tenant_id = $2 AND generation = $5
		`, workflowID, s.tenantID, a.errorMsg, a.errorCode, generation)
	}
	if err != nil {
		return fmt.Errorf("admin %s: %w", a.action, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("admin %s: rows affected: %w", a.action, err)
	}
	if n == 0 {
		return s.adminResolveMiss(ctx, tx, workflowID, generation, a.action)
	}

	if err := s.adminAppendAudit(ctx, tx, workflowID, a); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("admin %s: commit: %w", a.action, err)
	}

	releaseWorkflowResources(s.log(), s, workflowID)
	s.enforceParentClosePolicy(context.Background(), workflowID)
	return nil
}

// adminResolveMiss turns a zero-row UPDATE into the specific reason for it.
// The lookup is tenant-scoped, so another tenant's workflow reads as absent
// rather than as a generation mismatch -- the same "no oracle for valid IDs"
// stance callerOwnsTarget takes at the HTTP layer.
func (s *PostgresStore) adminResolveMiss(ctx context.Context, tx *sql.Tx, workflowID string, requested int64, action string) error {
	var stored int64
	err := tx.QueryRowContext(ctx,
		`SELECT generation FROM workflow_instances WHERE id = $1 AND tenant_id = $2`,
		workflowID, s.tenantID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return adminNotFound(action, workflowID)
	}
	if err != nil {
		return fmt.Errorf("admin %s: resolve miss for workflow %s: %w", action, workflowID, err)
	}
	return adminGenerationMismatch(action, workflowID, stored, requested)
}

func (s *PostgresStore) adminAppendAudit(ctx context.Context, tx *sql.Tx, workflowID string, a adminForce) error {
	var step int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(step), -1) + 1 FROM event_history WHERE workflow_id = $1 AND tenant_id = $2`,
		workflowID, s.tenantID).Scan(&step); err != nil {
		return fmt.Errorf("admin %s: next audit step: %w", a.action, err)
	}
	if err := s.appendEventsInTx(ctx, tx, workflowID, []EventRecord{a.auditEvent(step)}); err != nil {
		return fmt.Errorf("admin %s: append audit event: %w", a.action, err)
	}

	// Looked up by (workflow_id, step) -- the primary key -- and deliberately
	// not by tenant. The question is whether the row now sitting at that step
	// is the one we just wrote, and a row belonging to some other tenant is a
	// collision like any other rather than an absence.
	var eventType, operation sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT event_type, operation FROM event_history WHERE workflow_id = $1 AND step = $2`,
		workflowID, step).Scan(&eventType, &operation); err != nil {
		return fmt.Errorf("admin %s: confirm audit event: %w", a.action, err)
	}
	if eventType.String != string(EventTypeAdminAction) || operation.String != a.action {
		return adminAuditCollision(a.action, workflowID, step)
	}
	return nil
}

// ---------------------------------------------------------------------------
// MySQL
// ---------------------------------------------------------------------------

// AdminForceComplete marks a workflow as done, bypassing worker ownership.
func (s *MySQLStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	return s.adminForceResolve(ctx, workflowID, generation, adminForce{
		action: adminActionForceComplete, result: result, operator: operator,
	})
}

// AdminForceFail marks a workflow as failed, bypassing worker ownership.
func (s *MySQLStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	return s.adminForceResolve(ctx, workflowID, generation, adminForce{
		action: adminActionForceFail, errorMsg: errorMsg, errorCode: errorCode, operator: operator,
	})
}

func (s *MySQLStore) adminForceResolve(ctx context.Context, workflowID string, generation int64, a adminForce) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("admin %s: begin: %w", a.action, err)
	}
	defer tx.Rollback()

	var res sql.Result
	if a.action == adminActionForceComplete {
		res, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'done', result = ?, completed_at = NOW(6),
			    error_msg = NULL, error_code = NULL, error_op = NULL,
			    assigned_to = NULL, generation = generation + 1,
			    pending_terminal_status = NULL, defer_phase_deadline = NULL
			WHERE id = ? AND tenant_id = ? AND generation = ?
		`, a.result, workflowID, s.tenantID, generation)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'failed', error_msg = ?, error_code = ?, error_op = 'admin_force_fail',
			    completed_at = NOW(6), assigned_to = NULL, generation = generation + 1,
			    pending_terminal_status = NULL, defer_phase_deadline = NULL
			WHERE id = ? AND tenant_id = ? AND generation = ?
		`, a.errorMsg, a.errorCode, workflowID, s.tenantID, generation)
	}
	if err != nil {
		return fmt.Errorf("admin %s: %w", a.action, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("admin %s: rows affected: %w", a.action, err)
	}
	if n == 0 {
		return s.adminResolveMiss(ctx, tx, workflowID, generation, a.action)
	}

	if err := s.adminAppendAudit(ctx, tx, workflowID, a); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("admin %s: commit: %w", a.action, err)
	}

	releaseWorkflowResources(s.log(), s, workflowID)
	s.enforceParentClosePolicy(context.Background(), workflowID)
	return nil
}

func (s *MySQLStore) adminResolveMiss(ctx context.Context, tx *sql.Tx, workflowID string, requested int64, action string) error {
	var stored int64
	err := tx.QueryRowContext(ctx,
		`SELECT generation FROM workflow_instances WHERE id = ? AND tenant_id = ?`,
		workflowID, s.tenantID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return adminNotFound(action, workflowID)
	}
	if err != nil {
		return fmt.Errorf("admin %s: resolve miss for workflow %s: %w", action, workflowID, err)
	}
	return adminGenerationMismatch(action, workflowID, stored, requested)
}

func (s *MySQLStore) adminAppendAudit(ctx context.Context, tx *sql.Tx, workflowID string, a adminForce) error {
	var step int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(step), -1) + 1 FROM event_history WHERE workflow_id = ? AND tenant_id = ?`,
		workflowID, s.tenantID).Scan(&step); err != nil {
		return fmt.Errorf("admin %s: next audit step: %w", a.action, err)
	}
	if err := s.appendEventsInTx(ctx, tx, workflowID, []EventRecord{a.auditEvent(step)}); err != nil {
		return fmt.Errorf("admin %s: append audit event: %w", a.action, err)
	}

	var eventType, operation sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT event_type, operation FROM event_history WHERE workflow_id = ? AND step = ?`,
		workflowID, step).Scan(&eventType, &operation); err != nil {
		return fmt.Errorf("admin %s: confirm audit event: %w", a.action, err)
	}
	if eventType.String != string(EventTypeAdminAction) || operation.String != a.action {
		return adminAuditCollision(a.action, workflowID, step)
	}
	return nil
}

// ---------------------------------------------------------------------------
// SQL Server
// ---------------------------------------------------------------------------

// AdminForceComplete marks a workflow as done, bypassing worker ownership.
//
// The retry wrapper is the one CompleteWorkflow uses: it retries only errors
// SQL Server guarantees it rolled back, so a deadlock cannot apply the status
// change twice or leave it applied without its audit event. The generation
// bump makes the second attempt fail with a generation mismatch if the first
// one did in fact commit.
func (s *MSSQLStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	a := adminForce{action: adminActionForceComplete, result: result, operator: operator}
	return withRollbackGuaranteedRetry(ctx, "admin force-complete", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.adminForceResolveOnce(ctx, workflowID, generation, a)
	})
}

// AdminForceFail marks a workflow as failed, bypassing worker ownership.
func (s *MSSQLStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	a := adminForce{action: adminActionForceFail, errorMsg: errorMsg, errorCode: errorCode, operator: operator}
	return withRollbackGuaranteedRetry(ctx, "admin force-fail", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.adminForceResolveOnce(ctx, workflowID, generation, a)
	})
}

func (s *MSSQLStore) adminForceResolveOnce(ctx context.Context, workflowID string, generation int64, a adminForce) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("admin %s: begin: %w", a.action, err)
	}
	defer tx.Rollback()

	var res sql.Result
	if a.action == adminActionForceComplete {
		res, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(),
			    error_msg = NULL, error_code = NULL, error_op = NULL,
			    assigned_to = NULL, generation = generation + 1,
			    pending_terminal_status = NULL, defer_phase_deadline = NULL
			WHERE id = @p1 AND tenant_id = @p2 AND generation = @p4
		`, workflowID, s.tenantID, a.result, generation)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'failed', error_msg = @p3, error_code = @p4, error_op = 'admin_force_fail',
			    completed_at = SYSUTCDATETIME(), assigned_to = NULL, generation = generation + 1,
			    pending_terminal_status = NULL, defer_phase_deadline = NULL
			WHERE id = @p1 AND tenant_id = @p2 AND generation = @p5
		`, workflowID, s.tenantID, a.errorMsg, a.errorCode, generation)
	}
	if err != nil {
		return fmt.Errorf("admin %s: %w", a.action, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("admin %s: rows affected: %w", a.action, err)
	}
	if n == 0 {
		return s.adminResolveMiss(ctx, tx, workflowID, generation, a.action)
	}

	if err := s.adminAppendAudit(ctx, tx, workflowID, a); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("admin %s: commit: %w", a.action, err)
	}

	releaseWorkflowResources(s.log(), s, workflowID)
	s.enforceParentClosePolicy(context.Background(), workflowID)
	return nil
}

func (s *MSSQLStore) adminResolveMiss(ctx context.Context, tx *sql.Tx, workflowID string, requested int64, action string) error {
	var stored int64
	err := tx.QueryRowContext(ctx,
		`SELECT generation FROM workflow_instances WHERE id = @p1 AND tenant_id = @p2`,
		workflowID, s.tenantID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return adminNotFound(action, workflowID)
	}
	if err != nil {
		return fmt.Errorf("admin %s: resolve miss for workflow %s: %w", action, workflowID, err)
	}
	return adminGenerationMismatch(action, workflowID, stored, requested)
}

func (s *MSSQLStore) adminAppendAudit(ctx context.Context, tx *sql.Tx, workflowID string, a adminForce) error {
	var step int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(step), -1) + 1 FROM event_history WHERE workflow_id = @p1 AND tenant_id = @p2`,
		workflowID, s.tenantID).Scan(&step); err != nil {
		return fmt.Errorf("admin %s: next audit step: %w", a.action, err)
	}
	if err := s.appendEventsInTx(ctx, tx, workflowID, []EventRecord{a.auditEvent(step)}); err != nil {
		return fmt.Errorf("admin %s: append audit event: %w", a.action, err)
	}

	var eventType, operation sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT event_type, operation FROM event_history WHERE workflow_id = @p1 AND step = @p2`,
		workflowID, step).Scan(&eventType, &operation); err != nil {
		return fmt.Errorf("admin %s: confirm audit event: %w", a.action, err)
	}
	if eventType.String != string(EventTypeAdminAction) || operation.String != a.action {
		return adminAuditCollision(a.action, workflowID, step)
	}
	return nil
}
