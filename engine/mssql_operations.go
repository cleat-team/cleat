package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

func (s *MSSQLStore) ReapStaleInstances(ctx context.Context, timeout time.Duration, limit int) (int, error) {
	var out int
	err := withRollbackGuaranteedRetry(ctx, "reap stale instances", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		out, err = s.reapStaleInstancesOnce(ctx, timeout, limit)
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

func (s *MSSQLStore) reapStaleInstancesOnce(ctx context.Context, timeout time.Duration, limit int) (int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: begin: %w", err)
	}
	defer tx.Rollback()

	// See PostgresStore.ReapStaleInstances: a workflow reaped mid-defer-phase
	// goes back to 'terminating', because its terminal outcome is already
	// decided and calling it 'ready' would undo the distinction D6 created the
	// status to make.
	// TOP (@p3) inside the subquery, because SQL Server's UPDATE TOP takes no
	// ORDER BY and the order is the point -- see the interface doc.
	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1,
		    reclaim_count = reclaim_count + 1
		WHERE id IN (
		    SELECT TOP (@p3) id FROM workflow_instances
		    WHERE status = 'running'
		      AND heartbeat_at < DATEADD(SECOND, @p1, SYSUTCDATETIME())
		      AND tenant_id = @p2
		    ORDER BY heartbeat_at
		)
	`, -int(timeout.Seconds()), s.tenantID, reapLimitArg(limit))
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), tx.Commit()
}

// GetQueryState reads one key of a workflow's query state.
//
// Tenant-predicated for the reason on TerminateWorkflow: the id comes from the
// URL path of two separate handlers (cmd/cleat-worker/server.go's
// handleGetWorkflow and handleGetQueryState).
// This one is a read, so the consequence is disclosure rather than damage --
// query state is whatever the workflow chose to publish about itself.
func (s *MSSQLStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	var value sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT JSON_VALUE(query_state, '$.' + @p2)
		FROM workflow_instances WHERE id = @p1 AND tenant_id = @p3
	`, workflowID, key, s.tenantID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get query state: %w", err)
	}
	return value.String, nil
}

// ListQueryState returns every key a run published. See the PostgreSQL
// implementation for why this reads the whole column rather than using SQL
// Server's JSON functions.
func (s *MSSQLStore) ListQueryState(ctx context.Context, workflowID string) (map[string]string, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT query_state FROM workflow_instances WHERE id = @p1 AND tenant_id = @p2`,
		workflowID, s.tenantID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list query state: %w", err)
	}
	return decodeQueryState(raw)
}

func (s *MSSQLStore) GetEventCount(ctx context.Context, workflowID string) (int, error) {
	var out int
	err := withRollbackGuaranteedRetry(ctx, "get event count", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		out, err = s.getEventCountOnce(ctx, workflowID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

func (s *MSSQLStore) getEventCountOnce(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("get event count for %s: begin: %w", workflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = @p1`, workflowID).Scan(&count)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("get event count for %s: %w", workflowID, err)
	}
	return count, tx.Commit()
}

func (s *MSSQLStore) QueueDepth(ctx context.Context) (int64, error) {
	var count int64
	tqParam := s.buildTaskQueueParam()
	// Scoped by tenant. SQL Server's security policies do this in production,
	// but only when the session context is set on the connection the query
	// lands on -- and the test schema defines no policies at all (2.71
	// residual), so nothing was checking it either way. The predicate makes
	// the three dialects agree. IMPROVEMENT-PLAN 3.11.
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready' AND task_queue IN (SELECT value FROM STRING_SPLIT(@p1, ',')) AND tenant_id = @p2`,
		tqParam, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return count, nil
}

func (s *MSSQLStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	return withRollbackGuaranteedRetry(ctx, "update sticky worker", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.updateStickyWorkerOnce(ctx, workflowID, workerID)
	})
}

func (s *MSSQLStore) updateStickyWorkerOnce(ctx context.Context, workflowID, workerID string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("update sticky worker: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		-- No AND tenant_id, and not an omission: SQL Server bounds this with a
		-- session-context security policy, as Postgres does with FOR ALL RLS.
		-- MySQL has neither and states the predicate in SQL. See the note on
		-- PostgresStore.UpdateStickyWorker before filing the asymmetry.
		UPDATE workflow_instances SET sticky_worker_id = @p2 WHERE id = @p1
	`, workflowID, workerID)
	if err != nil {
		return fmt.Errorf("update sticky worker: %w", err)
	}
	return tx.Commit()
}

func (s *MSSQLStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	return withRollbackGuaranteedRetry(ctx, "clear sticky worker", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.clearStickyWorkerOnce(ctx, workflowID)
	})
}

func (s *MSSQLStore) clearStickyWorkerOnce(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("clear sticky worker: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		-- No AND tenant_id, for the same reason as updateStickyWorkerOnce above.
		UPDATE workflow_instances SET sticky_worker_id = NULL WHERE id = @p1
	`, workflowID)
	if err != nil {
		return fmt.Errorf("clear sticky worker: %w", err)
	}
	return tx.Commit()
}

func (s *MSSQLStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	return withRollbackGuaranteedRetry(ctx, "release workflow concurrency keys", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.releaseWorkflowConcurrencyKeysOnce(ctx, workflowID)
	})
}

func (s *MSSQLStore) releaseWorkflowConcurrencyKeysOnce(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE workflow_id = @p1 AND tenant_id = @p2`, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: %w", err)
	}
	// A registered queue's slot lives in queue_holders; release it with the bare
	// keys so a finished run frees its queue slot the same way it frees a mutex.
	_, err = tx.ExecContext(ctx, `DELETE FROM queue_holders WHERE workflow_id = @p1 AND tenant_id = @p2`, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: queue holders: %w", err)
	}
	return tx.Commit()
}

// TerminateWorkflow marks a workflow terminated.
//
// `AND tenant_id` here, and on the five other statements this commit touched,
// is load-bearing rather than defensive; the reasoning is the same as
// ClaimDueSchedule's and is written out there. What is different about this
// group is WHERE THE ID COMES FROM. The schedule and definition statements key
// on a name the tenant chose; these key on a generated workflow id that
// arrives from outside -- every one of them is reachable from an HTTP handler
// that takes the id straight out of the URL path
// (cmd/cleat-worker/app.go:handleDeadLetterTerminate, handleWorkflowRetry;
// server.go's query, signal and cancel routes).
//
// 3.77 argued that a generated id needs no predicate because a UUID cannot be
// guessed. That argument covers the plumbing statements, whose ids the engine
// read back from a row it had already scoped, and it does not cover these:
// unguessability is a claim about what an attacker knows, and a workflow id
// travels -- through logs, support tickets, a URL, a user who has since left
// the tenant. Knowing one is enough to terminate somebody else's workflow on a
// cleat_admin connection, which is every multi-tenant SQL Server deployment.
//
// Each handler resolves its store through apiServer.scopedStore, so the
// authenticated tenant was already on the store at every one of these sites
// and simply was not reaching the SQL.
//
// Since 3.92 a terminate that matches no row returns ErrWorkflowNotFound and
// does not run the parent-close cascade. See preemptivelySettleOnce.
//
// TERMINATE IS ASYNCHRONOUS WHEN THE WORKFLOW OWES CLEANUP (D6, and
// IMPROVEMENT-PLAN 3.75 step 2) -- see PostgresStore.TerminateWorkflow for the
// whole story. A workflow with registered defers goes to 'terminating' here,
// carrying its outcome in pending_terminal_status, and is finalized by
// FinalizeDeferPhase once its cleanup has run.
// TerminateWorkflow force-terminates a workflow, recording 'terminated'.
func (s *MSSQLStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	return withRollbackGuaranteedRetry(ctx, "terminate workflow", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.preemptivelySettleOnce(ctx, workflowID, reason, statusTerminated)
	})
}

// CancelWorkflow stops a workflow pre-emptively and records 'cancelled'.
// cleat#1153. See the PostgresStore method for why this shares a body with
// TerminateWorkflow rather than repeating the two-phase transition.
func (s *MSSQLStore) CancelWorkflow(ctx context.Context, workflowID, reason string) error {
	return withRollbackGuaranteedRetry(ctx, "cancel workflow", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.preemptivelySettleOnce(ctx, workflowID, reason, statusCancelled)
	})
}

func (s *MSSQLStore) preemptivelySettleOnce(ctx context.Context, workflowID, reason, finalStatus string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("%s workflow: begin: %w", finalStatus, err)
	}
	defer tx.Rollback()

	// Does this workflow owe a defer phase? See deferPhaseOwed. UPDLOCK holds
	// the row for the UPDATE that follows, so the status this reads is the
	// status that gets marked.
	var curStatus string
	var hasDefers, compacted int
	err = tx.QueryRowContext(ctx, `
		SELECT w.status,
		       CASE WHEN EXISTS(SELECT 1 FROM event_history e
		                        WHERE e.workflow_id = w.id AND e.event_type = 'defer')
		            THEN 1 ELSE 0 END,
		       CASE WHEN w.compaction_state IS NOT NULL THEN 1 ELSE 0 END
		FROM workflow_instances w WITH (UPDLOCK, ROWLOCK)
		WHERE w.id = @p1 AND w.tenant_id = @p2
	`, sql.Named("p1", workflowID), sql.Named("p2", s.tenantID)).Scan(&curStatus, &hasDefers, &compacted)
	if errors.Is(err, sql.ErrNoRows) {
		// Not wrapped, for the same reason the RowsAffected == 0 arm below is
		// not: withRollbackGuaranteedRetry must see it plainly rather than
		// retry a lookup that will keep answering the same thing.
		return ErrWorkflowNotFound
	}
	if err != nil {
		return fmt.Errorf("%s workflow: read: %w", finalStatus, err)
	}

	// cleat#1975 (D3): settled is final. See PostgresStore.TerminateWorkflow's
	// doc comment for the dead-letter exception. Not a rollback-guaranteed
	// class (isMSSQLRollbackGuaranteed only checks deadlock/snapshot errors),
	// so withRollbackGuaranteedRetry returns it on the first attempt.
	if isSettledStatus(curStatus) && !(finalStatus == statusTerminated && curStatus == statusDeadLettered) {
		return adminErrorf(ErrAdminStateConflict,
			"workflow %s: already settled (status=%s); refusing to write %s over it",
			workflowID, curStatus, finalStatus)
	}

	if deferPhaseOwed(curStatus, hasDefers == 1, compacted == 1) {
		// Phase 1 of the two-phase transition: mark, do not finalize. See
		// PostgresStore.TerminateWorkflow for why next_wake_at moves and why
		// nothing is released here.
		if _, err := tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = @p4,
			    pending_terminal_status = @p6,
			    defer_phase_deadline = DATEADD(SECOND, @p5, SYSUTCDATETIME()),
			    error_msg = @p2,
			    next_wake_at = SYSUTCDATETIME(),
			    assigned_to = NULL,
			    generation = generation + 1
			WHERE id = @p1 AND tenant_id = @p3
		`, sql.Named("p1", workflowID), sql.Named("p2", reason), sql.Named("p3", s.tenantID),
			sql.Named("p4", statusTerminating), sql.Named("p5", int(deferPhaseTimeout.Seconds())), sql.Named("p6", finalStatus)); err != nil {
			return fmt.Errorf("%s workflow: mark defer phase: %w", finalStatus, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("%s workflow commit: %w", finalStatus, err)
		}
		return nil
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = @p4,
		    error_msg = @p2,
		    completed_at = SYSUTCDATETIME(),
		    completed_by = assigned_to, assigned_to = NULL,
		    generation = generation + 1,
		    pending_terminal_status = NULL,
		    defer_phase_deadline = NULL
		WHERE id = @p1 AND tenant_id = @p3
	`, sql.Named("p1", workflowID), sql.Named("p2", reason), sql.Named("p3", s.tenantID), sql.Named("p4", finalStatus))
	if err != nil {
		return fmt.Errorf("%s workflow: %w", finalStatus, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s workflow: rows affected: %w", finalStatus, err)
	}
	if n == 0 {
		// Not wrapped, so withRollbackGuaranteedRetry's
		// isMSSQLRollbackGuaranteed check sees it plainly and returns rather
		// than retrying a lookup that will keep answering the same thing.
		return ErrWorkflowNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s workflow commit: %w", finalStatus, err)
	}
	releaseWorkflowResources(s.log(), s, workflowID)
	// IMPROVEMENT-PLAN 3.79. Terminate is a terminal transition, and the close
	// policy is what stops a closed parent leaving orphans behind. Every other
	// terminal path enforces it -- FinalizeWorkflowSegment for done/failed, and
	// adminForceResolve, which is an operator verb on an unclaimed workflow
	// exactly like this one. This path did not, so terminating a parent left
	// its TERMINATE children running while force-completing the same parent
	// failed them, with nothing recording why the two differed.
	s.enforceParentClosePolicy(context.Background(), workflowID, parentOutcomeMessage(finalStatus))
	return nil
}
