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
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
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

func (s *MSSQLStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	var value sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT JSON_VALUE(query_state, '$.' + @p2) FROM workflow_instances WHERE id = @p1
	`, workflowID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get query state: %w", err)
	}
	return value.String, nil
}

func (s *MSSQLStore) GetEventCount(ctx context.Context, workflowID string) (int, error) {
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
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready' AND task_queue IN (SELECT value FROM STRING_SPLIT(@p1, ','))`,
		tqParam).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return count, nil
}

func (s *MSSQLStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
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

func (s *MSSQLStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
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
		    assigned_to = NULL
		WHERE id = @p1
	`, sql.Named("p1", workflowID), sql.Named("p2", reason))
	if err != nil {
		return fmt.Errorf("terminate workflow: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("terminate workflow commit: %w", err)
	}
	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	if err := s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID); err != nil {
		s.log().WarnContext(context.Background(), "release concurrency keys failed", "workflow_id", workflowID, "error", err)
	}
	return nil
}

// adminOpMSSQL performs an admin operation on MSSQL with audit event.
func (s *MSSQLStore) adminOpMSSQL(ctx context.Context, workflowID string, generation int64, operator, action, opPrefix string, doUpdate func(tx *sql.Tx) error) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", opPrefix, err)
	}
	defer tx.Rollback()

	var auditStep int
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(step), -1) + 1 FROM event_history WHERE workflow_id = @p1`,
		workflowID).Scan(&auditStep)
	if err != nil {
		return fmt.Errorf("%s: get step: %w", opPrefix, err)
	}

	if err := doUpdate(tx); err != nil {
		return fmt.Errorf("%s: %w", opPrefix, err)
	}

	tsMillis := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, op, err, timestamp_ms)
		VALUES (@p1, @p2, 'admin_action', @p3, @p4, '', @p5)
	`, sql.Named("p1", workflowID), sql.Named("p2", auditStep),
		sql.Named("p3", operator), sql.Named("p4", action), sql.Named("p5", tsMillis))
	if err != nil {
		return fmt.Errorf("%s: insert audit event: %w", opPrefix, err)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE workflow_instances SET event_count = event_count + 1 WHERE id = @p1`,
		workflowID)
	if err != nil {
		return fmt.Errorf("%s: increment event_count: %w", opPrefix, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s commit: %w", opPrefix, err)
	}

	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)
	return nil
}

// AdminForceComplete force-completes a workflow on MSSQL.
func (s *MSSQLStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	return s.adminOpMSSQL(ctx, workflowID, generation, operator, "force_complete", "admin force-complete",
		func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, `
				UPDATE workflow_instances
				SET status = 'done', result = @p3, completed_at = GETDATE(), assigned_to = NULL
				WHERE id = @p1 AND generation = @p2
			`, sql.Named("p1", workflowID), sql.Named("p2", generation), sql.Named("p3", result))
			if err != nil {
				return err
			}
			rows, _ := r.RowsAffected()
			if rows == 0 {
				var exists bool
				_ = tx.QueryRowContext(ctx, `SELECT CASE WHEN EXISTS(SELECT 1 FROM workflow_instances WHERE id = @p1) THEN 1 ELSE 0 END`, workflowID).Scan(&exists)
				if !exists {
					return fmt.Errorf("workflow %s not found", workflowID)
				}
				return fmt.Errorf("generation mismatch for workflow %s", workflowID)
			}
			return nil
		})
}

// AdminForceFail force-fails a workflow on MSSQL.
func (s *MSSQLStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	return s.adminOpMSSQL(ctx, workflowID, generation, operator, "force_fail", "admin force-fail",
		func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, `
				UPDATE workflow_instances
				SET status = 'failed', error_msg = @p3, error_code = @p4,
				    completed_at = GETDATE(), assigned_to = NULL
				WHERE id = @p1 AND generation = @p2
			`, sql.Named("p1", workflowID), sql.Named("p2", generation),
				sql.Named("p3", errorMsg), sql.Named("p4", errorCode))
			if err != nil {
				return err
			}
			rows, _ := r.RowsAffected()
			if rows == 0 {
				var exists bool
				_ = tx.QueryRowContext(ctx, `SELECT CASE WHEN EXISTS(SELECT 1 FROM workflow_instances WHERE id = @p1) THEN 1 ELSE 0 END`, workflowID).Scan(&exists)
				if !exists {
					return fmt.Errorf("workflow %s not found", workflowID)
				}
				return fmt.Errorf("generation mismatch for workflow %s", workflowID)
			}
			return nil
		})
}

// AdminReReplay resets a workflow to 'ready' on MSSQL.
func (s *MSSQLStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return s.adminOpMSSQL(ctx, workflowID, generation, operator, "re_replay", "admin re-replay",
		func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, `
				UPDATE workflow_instances
				SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
				    error_msg = NULL, error_code = NULL, error_op = NULL,
				    next_wake_at = GETDATE()
				WHERE id = @p1 AND generation = @p2
			`, sql.Named("p1", workflowID), sql.Named("p2", generation))
			if err != nil {
				return err
			}
			rows, _ := r.RowsAffected()
			if rows == 0 {
				var exists bool
				_ = tx.QueryRowContext(ctx, `SELECT CASE WHEN EXISTS(SELECT 1 FROM workflow_instances WHERE id = @p1) THEN 1 ELSE 0 END`, workflowID).Scan(&exists)
				if !exists {
					return fmt.Errorf("workflow %s not found", workflowID)
				}
				return fmt.Errorf("generation mismatch for workflow %s", workflowID)
			}
			return nil
		})
}
