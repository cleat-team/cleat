package engine

import (
	"context"
	"fmt"
)

// FinalizeDeferPhase applies the outcome recorded at mark time, after the defer
// segment that ran the workflow's cleanup. IMPROVEMENT-PLAN 3.75 step 2.
//
// It is deliberately NOT an arm of FinalizeWorkflowSegment. That method routes
// through the finalize_workflow_status PL/pgSQL function, whose accepted status
// set is fixed by a migration and shared with two other dialects, and the
// status this applies is not a value the caller chose -- it is the one the
// database recorded when the transition was marked. Reading it out of the row
// is the point: nothing between the two phases can substitute a different
// outcome, because the caller never names one.
//
// The fence is the ordinary claim fence, assigned_to + generation. Losing it is
// normal rather than exceptional here: ExpireDeferPhases bumps the generation
// precisely so that a phase past its deadline is taken away from a worker that
// is still grinding on it.
//
// `AND pending_terminal_status IS NOT NULL` is the third predicate and it is
// not redundant with the fence. Two workers cannot both hold the claim, but a
// retry of this same call after a successful commit would otherwise write
// status = NULL over a terminated workflow. Requiring the marker makes the
// second attempt a no-op that reports a lost fence.
func (s *PostgresStore) FinalizeDeferPhase(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize defer phase: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return fmt.Errorf("finalize defer phase: set rls: %w", err)
	}

	// The defer bodies' own host calls are durable calls, and their events are
	// this segment's output. Appending them in the same transaction as the
	// terminal write is what makes a crash here leave either a complete
	// segment or none of it.
	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize defer phase: append events: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = pending_terminal_status,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL,
		    completed_at = now(),
		    assigned_to = NULL
		WHERE id = $1
		  AND assigned_to = $2
		  AND generation = $3
		  AND pending_terminal_status IS NOT NULL
	`, runID, workerID, generation)
	if err != nil {
		return fmt.Errorf("finalize defer phase: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finalize defer phase: rows affected: %w", err)
	}
	if n == 0 {
		return ErrFenceLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finalize defer phase commit: %w", err)
	}

	// NOW the release runs, and this ordering is the whole point of the
	// two-phase transition: after the defers that may have released these
	// same resources themselves, not before them.
	releaseWorkflowResources(s.log(), s, runID)
	s.enforceParentClosePolicy(context.Background(), runID)
	return nil
}

// ExpireDeferPhases applies the recorded outcome to every workflow whose defer
// phase has outrun defer_phase_deadline.
//
// This is not the crash sweep. A worker that dies mid-phase is caught by
// ReapStaleInstances on the heartbeat, which returns the workflow to
// 'terminating' for another attempt. This is the bound on ATTEMPTS: a guest
// that traps every time it replays would otherwise be re-queued forever, and a
// terminate that cannot run its cleanup must still terminate.
//
// The generation bump is what makes it safe to run against a phase that is
// currently claimed. The holder's FinalizeDeferPhase then fails its fence and
// returns ErrFenceLost, which the worker already treats as "another owner has
// this now" rather than as an error -- so the outcome is applied exactly once
// whichever of the two gets there first.
func (s *PostgresStore) ExpireDeferPhases(ctx context.Context) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = pending_terminal_status,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL,
		    completed_at = now(),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE pending_terminal_status IS NOT NULL
		  AND defer_phase_deadline < now()
		RETURNING id
	`)
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
