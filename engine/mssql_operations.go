package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

func (s *MSSQLStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	var out int
	err := withRollbackGuaranteedRetry(ctx, "reap stale instances", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		out, err = s.reapStaleInstancesOnce(ctx, timeout)
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

func (s *MSSQLStore) reapStaleInstancesOnce(ctx context.Context, timeout time.Duration) (int, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1
		WHERE status = 'running'
		  AND heartbeat_at < DATEADD(SECOND, @p1, SYSUTCDATETIME())
		  AND tenant_id = @p2
	`, -int(timeout.Seconds()), s.tenantID)
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
func (s *MSSQLStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	return withRollbackGuaranteedRetry(ctx, "terminate workflow", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.terminateWorkflowOnce(ctx, workflowID, reason)
	})
}

func (s *MSSQLStore) terminateWorkflowOnce(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("terminate workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'terminated',
		    error_msg = @p2,
		    completed_at = GETDATE(),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE id = @p1 AND tenant_id = @p3
	`, sql.Named("p1", workflowID), sql.Named("p2", reason), sql.Named("p3", s.tenantID))
	if err != nil {
		return fmt.Errorf("terminate workflow: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("terminate workflow commit: %w", err)
	}
	releaseWorkflowResources(s.log(), s, workflowID)
	// IMPROVEMENT-PLAN 3.79. Terminate is a terminal transition, and the close
	// policy is what stops a closed parent leaving orphans behind. Every other
	// terminal path enforces it -- FinalizeWorkflowSegment for done/failed, and
	// adminForceResolve, which is an operator verb on an unclaimed workflow
	// exactly like this one. This path did not, so terminating a parent left
	// its TERMINATE children running while force-completing the same parent
	// failed them, with nothing recording why the two differed.
	s.enforceParentClosePolicy(context.Background(), workflowID)
	return nil
}
