package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
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

	var appliedStatus string
	err = tx.QueryRowContext(ctx, `
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
		RETURNING status
	`, runID, workerID, generation).Scan(&appliedStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFenceLost
	}
	if err != nil {
		return fmt.Errorf("finalize defer phase: %w", err)
	}

	// cleat#3245 Phase 3 step 2 piece 6d: mirror the same apply-defer-phase
	// transition onto workflow_leases, same tx, same fence as the
	// workflow_instances write above. completed_at stays workflow_instances-
	// only (no piece mirrors it). No workflow_payloads write: result/
	// error_msg/error_code/error_op were already written at mark time
	// (pieces 4c/6c), and this call only flips status -- the same reason
	// adminForceMark's one-phase sibling (piece 4c) needed no payload write
	// here either.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_leases
		SET status = pending_terminal_status,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL,
		    assigned_to = NULL
		WHERE id = $1 AND assigned_to = $2 AND generation = $3 AND pending_terminal_status IS NOT NULL
	`, runID, workerID, generation); err != nil {
		return fmt.Errorf("finalize defer phase: mirror lease: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finalize defer phase commit: %w", err)
	}

	// NOW the release runs, and this ordering is the whole point of the
	// two-phase transition: after the defers that may have released these
	// same resources themselves, not before them.
	releaseWorkflowResources(s.log(), s, runID)
	// appliedStatus is the outcome THIS workflow just settled to -- what was
	// recorded at mark time (TerminateWorkflow, CancelWorkflow, or a
	// TERMINATE close-policy arm), applied verbatim by the UPDATE above. Its
	// own children, if any, need to hear that outcome, not the one that
	// closed this workflow's own parent.
	s.enforceParentClosePolicy(context.Background(), runID, parentOutcomeMessage(appliedStatus))
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
// It is safe to run against a phase that is currently claimed, and the holder's
// FinalizeDeferPhase then fails its fence and returns ErrFenceLost -- which the
// worker already treats as "another owner has this now" rather than as an error
// -- so the outcome is applied exactly once whichever of the two gets there
// first.
//
// This used to credit the generation bump for that, and the bump does suffice.
// So does each of the other two clauses this UPDATE writes, because the fence is
//
//	assigned_to = $2 AND generation = $3 AND pending_terminal_status IS NOT NULL
//
// and the sweep nulls assigned_to and pending_terminal_status as well. Measured
// on postgres, removing each and keeping the other two: all three fence the
// holder out alone, and the race only reopens when all three are gone
// (TestAnExpiredPhaseFencesOutTheWorkerStillHoldingIt). Naming one of three
// redundant mechanisms as "what makes it safe" is how §3.112 went wrong, so it
// is stated as redundancy here rather than as a single cause.
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
		RETURNING id, status
	`)
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: %w", err)
	}
	expired, err := scanExpiredDeferPhases(rows)
	if err != nil {
		return 0, fmt.Errorf("expire defer phases: scan: %w", err)
	}

	// cleat#3245 Phase 3 step 2 piece 6d: mirror the same sweep onto
	// workflow_leases, same tx. Captures the ids the instances UPDATE just
	// returned rather than re-deriving the WHERE clause against the lease
	// table -- the GAP-avoidance pattern pieces 4d/6b already use for a bulk
	// write, since nothing else can have changed these rows mid-tx but
	// re-deriving would still have to agree with what was actually touched.
	// generation bumps here, matching the instances UPDATE above: unlike
	// FinalizeDeferPhase (a single claimed row with a live holder to fence
	// out), this sweep is exactly the case that bump exists for.
	if err := mirrorExpiredDeferPhaseLeases(ctx, tx, expired); err != nil {
		return 0, err
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

// mirrorExpiredDeferPhaseLeases mirrors ExpireDeferPhases' sweep onto
// workflow_leases for exactly the rows the workflow_instances UPDATE just
// touched. No workflow_payloads write, for the same reason FinalizeDeferPhase
// needs none: result/error_msg/error_code/error_op were written at mark time.
func mirrorExpiredDeferPhaseLeases(ctx context.Context, tx *sql.Tx, expired []expiredDeferPhase) error {
	if len(expired) == 0 {
		return nil
	}
	ids := make([]string, len(expired))
	for i, wf := range expired {
		ids[i] = wf.id
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_leases
		SET status = pending_terminal_status,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL,
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE id = ANY($1)
	`, pq.Array(ids)); err != nil {
		return fmt.Errorf("expire defer phases: mirror lease: %w", err)
	}
	return nil
}
