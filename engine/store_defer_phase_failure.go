package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// DeferPhaseFailure is the outcome a worker records when a run it owns ends in failure while defers are still owed
// (cleat#4079): a component guest that was KILLED (the execution fence, an instruction limit, a trap) never ran its
// cleanup, because a trapped component instance cannot be re-entered. The run goes to 'terminating' carrying this outcome,
// is claimed again, and a defer segment replays its history on a fresh instance to run the cleanup; FinalizeDeferPhase then
// applies the outcome recorded here.
type DeferPhaseFailure struct {
	// Status is the terminal status to apply once the cleanup has run: "failed" or "dead_lettered". It is decided by the
	// same classification the worker applies to a failure that owes no defers (endedOnAnExhaustedCall), so a killed run that
	// would have been dead-lettered still is, and keeps its retention.
	Status string

	// ErrorMsg, ErrorCode and ErrorOp are what a client reads from the finished run. They are written at MARK time, so the
	// run reports the same failure it would have without the cleanup, and nothing the segment does can change it.
	ErrorMsg  string
	ErrorCode string
	ErrorOp   string

	// QueryState is what the failing execution published via SetQueryState, carried the same way FailWorkflow carries it.
	QueryState map[string]string
}

// validDeferPhaseFailureStatus reports whether s is an outcome MarkFailureForDeferPhase can record.
func validDeferPhaseFailureStatus(s string) bool {
	return s == statusFailed || s == statusDeadLettered
}

func (s *PostgresStore) MarkFailureForDeferPhase(ctx context.Context, runID, workerID string, generation int64, f DeferPhaseFailure, newEvents []EventRecord) error {
	if !validDeferPhaseFailureStatus(f.Status) {
		return fmt.Errorf("mark failure for defer phase: status %q is not an outcome this can record", f.Status)
	}
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: begin: %w", err)
	}
	defer tx.Rollback()

	qsParam := queryStateUpdateParam(f.QueryState)
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: derive tenant key: %w", err)
	}
	sealedErrMsg, err := encryptTextColumnForStorage(f.ErrorMsg, tc)
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: encrypt error_msg: %w", err)
	}
	sealedErrorCode, err := encryptTextColumnForStorage(f.ErrorCode, tc)
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: encrypt error_code: %w", err)
	}
	sealedErrorOp, err := encryptTextColumnForStorage(f.ErrorOp, tc)
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: encrypt error_op: %w", err)
	}
	if qs, ok := qsParam.(string); ok {
		sealedQS, err := encryptJSONColumnForStorage(qs, tc)
		if err != nil {
			return fmt.Errorf("mark failure for defer phase: encrypt query_state: %w", err)
		}
		qsParam = sealedQS
	}

	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("mark failure for defer phase: append events: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_leases
		SET status = $4, pending_terminal_status = $5,
		    defer_phase_deadline = now() + ($6 * interval '1 second'),
		    next_wake_at = now(), completed_by = assigned_to, assigned_to = NULL
		WHERE id = $1 AND assigned_to = $2 AND generation = $3
	`, runID, workerID, generation, statusTerminating, f.Status, int(deferPhaseTimeout.Seconds()))
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFenceLost
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_payloads
		SET error_msg = $2, error_code = $3, error_op = $4, query_state = COALESCE($5::jsonb, query_state)
		WHERE id = $1
	`, runID, sealedErrMsg, sealedErrorCode, sealedErrorOp, qsParam); err != nil {
		return fmt.Errorf("mark failure for defer phase: payload: %w", err)
	}

	pgNotify(ctx, tx, s.notifyChannel)
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark failure for defer phase: commit: %w", err)
	}
	return nil
}

func (s *MySQLStore) MarkFailureForDeferPhase(ctx context.Context, runID, workerID string, generation int64, f DeferPhaseFailure, newEvents []EventRecord) error {
	if !validDeferPhaseFailureStatus(f.Status) {
		return fmt.Errorf("mark failure for defer phase: status %q is not an outcome this can record", f.Status)
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: begin: %w", err)
	}
	defer tx.Rollback()

	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("mark failure for defer phase: append events: %w", err)
	}

	qsParam := queryStateUpdateParam(f.QueryState)
	//nolint:gosec // G202: deferPhaseDeadlineMySQL is a compile-time constant built from deferPhaseTimeout, not user input.
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = ?, pending_terminal_status = ?,
		    defer_phase_deadline = `+deferPhaseDeadlineMySQL+`,
		    next_wake_at = NOW(6), completed_by = assigned_to, assigned_to = NULL,
		    error_msg = ?, error_code = ?, error_op = ?,
		    query_state = COALESCE(?, query_state)
		WHERE id = ? AND assigned_to = ? AND generation = ? AND tenant_id = ?
	`, statusTerminating, f.Status, f.ErrorMsg, f.ErrorCode, f.ErrorOp, qsParam, runID, workerID, generation, s.tenantID)
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFenceLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark failure for defer phase: commit: %w", err)
	}
	return nil
}

// mssqlMarkFailureForDeferPhase is one Sprintf so the statement, and its tenant predicate, stay statically readable: the
// tenant-predicate guard (TestMSSQLTenantScopedTablesAreQueriedWithATenantPredicate) reads the first string literal, and
// a `+` splice of the deadline constant cut the WHERE off from it. The predicate is the whole isolation mechanism here
// (dbo.fn_tenant_filter is off for any dbo.cleat_admin connection). The one interpolation is a compile-time constant
// expression, not reachable from a caller.
var mssqlMarkFailureForDeferPhase = fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = @p4, pending_terminal_status = @p5,
		    defer_phase_deadline = %s,
		    next_wake_at = SYSUTCDATETIME(), completed_by = assigned_to, assigned_to = NULL,
		    error_msg = @p6, error_code = @p7, error_op = @p8,
		    query_state = COALESCE(@p9, query_state)
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p3 AND tenant_id = @p10
	`, deferPhaseDeadlineMSSQL)

func (s *MSSQLStore) MarkFailureForDeferPhase(ctx context.Context, runID, workerID string, generation int64, f DeferPhaseFailure, newEvents []EventRecord) error {
	if !validDeferPhaseFailureStatus(f.Status) {
		return fmt.Errorf("mark failure for defer phase: status %q is not an outcome this can record", f.Status)
	}
	return withRollbackGuaranteedRetry(ctx, "mark failure for defer phase", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.markFailureForDeferPhaseOnce(ctx, runID, workerID, generation, f, newEvents)
	})
}

func (s *MSSQLStore) markFailureForDeferPhaseOnce(ctx context.Context, runID, workerID string, generation int64, f DeferPhaseFailure, newEvents []EventRecord) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("mark failure for defer phase: append events: %w", err)
	}

	qsParam := queryStateUpdateParam(f.QueryState)
	res, err := tx.ExecContext(ctx, mssqlMarkFailureForDeferPhase,
		sql.Named("p1", runID), sql.Named("p2", workerID), sql.Named("p3", generation),
		sql.Named("p4", statusTerminating), sql.Named("p5", f.Status),
		sql.Named("p6", f.ErrorMsg), sql.Named("p7", f.ErrorCode), sql.Named("p8", f.ErrorOp),
		sql.Named("p9", qsParam), sql.Named("p10", s.tenantID))
	if err != nil {
		return fmt.Errorf("mark failure for defer phase: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFenceLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark failure for defer phase: commit: %w", err)
	}
	return nil
}

// MarkFailureForDeferPhase routes by workflow id, like FinalizeDeferPhase: the fence lives on the shard's own row.
func (s *ShardedStore) MarkFailureForDeferPhase(ctx context.Context, runID, workerID string, generation int64, f DeferPhaseFailure, newEvents []EventRecord) error {
	shard := s.getShard(runID)
	if shard == nil {
		return fmt.Errorf("mark_failure_for_defer_phase: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	m, ok := shard.Store.(DeferPhaseFailureMarker)
	if !ok {
		return fmt.Errorf("mark_failure_for_defer_phase: shard %q's store cannot mark a failure for a defer phase", shard.Config.Name)
	}
	return m.MarkFailureForDeferPhase(ctx, runID, workerID, generation, f, newEvents)
}
