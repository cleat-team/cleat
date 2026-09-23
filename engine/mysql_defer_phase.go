package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FinalizeDeferPhase is MySQL's half of the two-phase terminal transition.
// See PostgresStore.FinalizeDeferPhase for what it is and why it is not an arm
// of FinalizeWorkflowSegment.
func (s *MySQLStore) FinalizeDeferPhase(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("finalize defer phase: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize defer phase: append events: %w", err)
	}

	// Read the outcome under the same fence the UPDATE below applies, so what
	// is read is guaranteed to be what gets written -- MySQL has no RETURNING,
	// so this is the only way to learn the applied status without a second,
	// unfenced round trip. cleat#1978: this workflow's own children, if any,
	// need to hear what happened to IT, via parentOutcomeMessage.
	var appliedStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT pending_terminal_status FROM workflow_instances
		WHERE id = ?
		  AND assigned_to = ?
		  AND generation = ?
		  AND pending_terminal_status IS NOT NULL
		  AND tenant_id = ?
		FOR UPDATE
	`, runID, workerID, generation, s.tenantID).Scan(&appliedStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFenceLost
	}
	if err != nil {
		return fmt.Errorf("finalize defer phase: read: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = pending_terminal_status,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL,
		    completed_at = NOW(6),
		    assigned_to = NULL
		WHERE id = ?
		  AND assigned_to = ?
		  AND generation = ?
		  AND pending_terminal_status IS NOT NULL
		  AND tenant_id = ?
	`, runID, workerID, generation, s.tenantID); err != nil {
		return fmt.Errorf("finalize defer phase: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finalize defer phase commit: %w", err)
	}

	releaseWorkflowResources(s.log(), s, runID)
	s.enforceParentClosePolicy(context.Background(), runID, parentOutcomeMessage(appliedStatus))
	return nil
}

// ExpireDeferPhases is MySQL's half; see PostgresStore.ExpireDeferPhases.
//
// Two statements rather than one, because MySQL has no RETURNING: the ids are
// collected first and then updated, which is the same shape releaseTerminated-
// Children uses for the same reason. A row that leaves the set between them is
// simply not expired this tick and is picked up on the next.
func (s *MySQLStore) ExpireDeferPhases(ctx context.Context) (int, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, pending_terminal_status FROM workflow_instances
		WHERE pending_terminal_status IS NOT NULL
		  AND defer_phase_deadline < NOW(6)
		  AND tenant_id = ?
		FOR UPDATE
	`, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: select: %w", err)
	}
	expired, err := scanExpiredDeferPhases(rows)
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: scan: %w", err)
	}
	if len(expired) == 0 {
		return 0, tx.Rollback()
	}

	idClause := inClausePlaceholders(len(expired))
	args := make([]any, 0, len(expired)+1)
	for _, wf := range expired {
		args = append(args, wf.id)
	}
	args = append(args, s.tenantID)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = pending_terminal_status,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL,
		    completed_at = NOW(6),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE id IN (%s) AND tenant_id = ?
	`, idClause), args...); err != nil {
		return 0, fmt.Errorf("expire defer phases: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("expire defer phases commit: %w", err)
	}

	for _, wf := range expired {
		s.log().WarnContext(ctx, "defer phase outran its deadline; applying the recorded outcome without the cleanup",
			"workflow_id", wf.id, "timeout", deferPhaseTimeout)
		releaseWorkflowResources(s.log(), s, wf.id)
		s.enforceParentClosePolicy(context.Background(), wf.id, parentOutcomeMessage(wf.status))
	}
	return len(expired), nil
}
