package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// FinalizeDeferPhase is SQL Server's half of the two-phase terminal transition.
// See PostgresStore.FinalizeDeferPhase for what it is and why it is not an arm
// of FinalizeWorkflowSegment.
func (s *MSSQLStore) FinalizeDeferPhase(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord) error {
	return withRollbackGuaranteedRetry(ctx, "finalize defer phase", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.finalizeDeferPhaseOnce(ctx, runID, workerID, generation, newEvents)
	})
}

func (s *MSSQLStore) finalizeDeferPhaseOnce(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("finalize defer phase: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize defer phase: append events: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = pending_terminal_status,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL,
		    completed_at = GETDATE(),
		    assigned_to = NULL
		WHERE id = @p1
		  AND assigned_to = @p2
		  AND generation = @p3
		  AND pending_terminal_status IS NOT NULL
		  AND tenant_id = @p4
	`, sql.Named("p1", runID), sql.Named("p2", workerID),
		sql.Named("p3", generation), sql.Named("p4", s.tenantID))
	if err != nil {
		return fmt.Errorf("finalize defer phase: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finalize defer phase: rows affected: %w", err)
	}
	if n == 0 {
		// Not wrapped: withRollbackGuaranteedRetry must see it plainly rather
		// than retry a fence that has already moved on.
		return ErrFenceLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finalize defer phase commit: %w", err)
	}

	releaseWorkflowResources(s.log(), s, runID)
	s.enforceParentClosePolicy(context.Background(), runID)
	return nil
}

// ExpireDeferPhases is SQL Server's half; see PostgresStore.ExpireDeferPhases.
func (s *MSSQLStore) ExpireDeferPhases(ctx context.Context) (int, error) {
	var out int
	err := withRollbackGuaranteedRetry(ctx, "expire defer phases", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		out, err = s.expireDeferPhasesOnce(ctx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

func (s *MSSQLStore) expireDeferPhasesOnce(ctx context.Context) (int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = pending_terminal_status,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL,
		    completed_at = GETDATE(),
		    assigned_to = NULL,
		    generation = generation + 1
		OUTPUT INSERTED.id
		WHERE pending_terminal_status IS NOT NULL
		  AND defer_phase_deadline < SYSUTCDATETIME()
		  AND tenant_id = @p1
	`, sql.Named("p1", s.tenantID))
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: %w", err)
	}
	ids, err := scanWorkflowIDs(rows)
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: scan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("expire defer phases commit: %w", err)
	}

	for _, id := range ids {
		s.log().WarnContext(ctx, "defer phase outran its deadline; applying the recorded outcome without the cleanup",
			"workflow_id", id, "timeout", deferPhaseTimeout)
		releaseWorkflowResources(s.log(), s, id)
		s.enforceParentClosePolicy(context.Background(), id)
	}
	return len(ids), nil
}
