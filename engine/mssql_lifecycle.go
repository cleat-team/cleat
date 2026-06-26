package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Claim Methods (C.3)
// ---------------------------------------------------------------------------

// ClaimWorkflow atomically claims a single runnable workflow instance.
func (s *MSSQLStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	wfs, err := s.ClaimWorkflows(ctx, workerID, 1)
	if err != nil {
		return nil, err
	}
	if len(wfs) == 0 {
		return nil, nil
	}
	return wfs[0], nil
}

// ClaimWorkflows atomically claims up to limit runnable workflow instances.
// Uses UPDATE...OUTPUT with READPAST/UPDLOCK hints (SQL Server's equivalent
// of FOR UPDATE SKIP LOCKED) wrapped in a transaction with RLS context.
func (s *MSSQLStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqParam := s.buildTaskQueueParam()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
		    generation = generation + 1
		OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
		       INSERTED.status, INSERTED.input, INSERTED.assigned_to,
		       INSERTED.next_wake_at, INSERTED.tenant_id, INSERTED.created_at,
		       INSERTED.error_code, INSERTED.error_op, INSERTED.generation,
		       COALESCE(INSERTED.priority, 0) AS priority,
		       INSERTED.trace_id
		WHERE id IN (
			SELECT id
			FROM workflow_instances WITH (READPAST, UPDLOCK, ROWLOCK)
			WHERE status = 'ready'
			  AND next_wake_at <= SYSUTCDATETIME()
			  AND task_queue IN (SELECT value FROM STRING_SPLIT(@p2, ','))
			ORDER BY priority ASC, created_at
			OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY
		)
	`, workerID, tqParam, limit)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: %w", err)
	}
	defer rows.Close()

	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString
		var createdAt sql.NullTime
		var inputStr string
		var errorCode, errorOp sql.NullString
		var traceID sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID); err != nil {
			return nil, fmt.Errorf("claim workflows scan: %w", err)
		}
		wf.TraceID = traceID.String

		wf.Input = json.RawMessage(inputStr)
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		tx.Rollback()
		return nil, nil
	}
	return wfs, tx.Commit()
}

// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
// that are sticky to this worker. Uses the sticky_worker_id filter for
// low-contention claiming. Returns fewer than limit if not enough sticky
// workflows are ready. Callers should fall back to ClaimWorkflows for remaining capacity.
func (s *MSSQLStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqParam := s.buildTaskQueueParam()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
		    generation = generation + 1
		OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
		       INSERTED.status, INSERTED.input, INSERTED.assigned_to,
		       INSERTED.next_wake_at, INSERTED.tenant_id, INSERTED.created_at,
		       INSERTED.error_code, INSERTED.error_op, INSERTED.generation,
		       COALESCE(INSERTED.priority, 0) AS priority,
		       INSERTED.trace_id
		WHERE id IN (
			SELECT id
			FROM workflow_instances WITH (READPAST, UPDLOCK, ROWLOCK)
			WHERE status = 'ready'
			  AND next_wake_at <= SYSUTCDATETIME()
			  AND sticky_worker_id = @p1
			  AND task_queue IN (SELECT value FROM STRING_SPLIT(@p2, ','))
			ORDER BY priority ASC, created_at
			OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY
		)
	`, workerID, tqParam, limit)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: %w", err)
	}
	defer rows.Close()

	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString
		var createdAt sql.NullTime
		var inputStr string
		var errorCode, errorOp sql.NullString
		var traceID sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID); err != nil {
			return nil, fmt.Errorf("claim sticky workflows scan: %w", err)
		}
		wf.TraceID = traceID.String

		wf.Input = json.RawMessage(inputStr)
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		tx.Rollback()
		return nil, nil
	}
	return wfs, tx.Commit()
}

// ---------------------------------------------------------------------------
// Workflow Lifecycle Methods (C.5)
// ---------------------------------------------------------------------------

// Heartbeat updates the heartbeat timestamp. Returns false if the workflow
// is no longer assigned to this worker.
func (s *MSSQLStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = SYSUTCDATETIME()
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p3
	`, workflowID, workerID, generation)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, tx.Commit()
}

// BatchHeartbeat updates heartbeat_at for all workflows assigned to this worker
// with status 'running'. Uses a single UPDATE instead of N calls.
// NOTE: This intentionally does NOT check per-workflow generation because it
// operates on ALL running workflows for a worker, and generations differ per
// workflow. Individual generation-guarded operations (Heartbeat,
// CompleteWorkflow, FailWorkflow, etc.) prevent double-execution even if the
// batch heartbeat refreshes a stale workflow's heartbeat_at.
func (s *MSSQLStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = SYSUTCDATETIME()
		WHERE assigned_to = @p1 AND status = 'running'
	`, workerID)
	if err != nil {
		return 0, fmt.Errorf("batch heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// CompleteWorkflow marks a workflow as completed with a result.
func (s *MSSQLStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("complete workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), assigned_to = NULL, query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
	`, workflowID, workerID, result, string(qsJSON), generation)
	if err != nil {
		return err
	}

	// Record idempotency result within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET result = @p2 WHERE workflow_id = @p1`,
		workflowID, result); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.
func (s *MSSQLStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("fail workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed',
		    error_msg = @p3,
		    error_code = @p4,
		    error_op = @p5,
		    completed_at = SYSUTCDATETIME(),
		    assigned_to = NULL,
		    query_state = @p6
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p7
	`, workflowID, workerID, errorMsg, errorCode, errorOp, string(qsJSON), generation)
	if err != nil {
		return err
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1`,
		workflowID, errorMsg); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts.
func (s *MSSQLStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = @p3, error_code = @p4, error_op = @p5,
		    completed_at = SYSUTCDATETIME(), assigned_to = NULL
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p6
	`, workflowID, workerID, errMsg, errorCode, errorOp, generation)
	if err != nil {
		return err
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1`,
		workflowID, errMsg); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// RetryWorkflow moves a dead_lettered workflow back to a runnable state.
// Resets status to 'ready', clears the worker assignment and error fields,
// and sets next_wake_at to now so the workflow is re-queued immediately.
func (s *MSSQLStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = SYSUTCDATETIME()
		WHERE id = @p1 AND status = 'dead_lettered'
	`, workflowID)
	return err
}

// ReleaseWorkflow returns a workflow to the ready queue with a next wake time.
func (s *MSSQLStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("release workflow: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = @p3
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p4
	`, workflowID, workerID, nextWakeAt, generation)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("release workflow: rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("release workflow: no rows affected for %s", workflowID)
	}

	return tx.Commit()
}

// ContinueAsNew atomically creates a new workflow run and completes the current
// one in a single database transaction. Returns the new run ID on success.
func (s *MSSQLStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("continue as new: begin: %w", err)
	}
	defer tx.Rollback()

	// Append events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, currentRunID, newEvents); err != nil {
		return "", fmt.Errorf("continue as new: append events: %w", err)
	}

	// Use the store's tenant scope to preserve tenant isolation.
	// Create the new workflow run with a Go-generated UUID.
	newRunID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS VARCHAR(MAX)),
		        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3), 'default'),
		        @p5, @p6)
	`, newRunID, defName, defVersion, newInput, s.tenantID, priority)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// Complete the current run.
	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), assigned_to = NULL, query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
	`, currentRunID, workerID, result, string(qsJSON), generation)
	if err != nil {
		return "", fmt.Errorf("continue as new: complete old run: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}

	// Best-effort cleanup after commit.
	s.ClearStickyWorker(context.Background(), currentRunID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), currentRunID)
	s.enforceParentClosePolicy(context.Background(), currentRunID)

	return newRunID, nil
}

// FinalizeWorkflowSegment atomically appends new events and updates the
// workflow status in a single database transaction. This eliminates the
// race between AppendEventHistoryBatch and the subsequent CompleteWorkflow /
// FailWorkflow / ReleaseWorkflow call.
//
// finalStatus must be one of:
//   - "done"   — marks the workflow as completed with the given result
//   - "failed" — marks the workflow as failed with the given error info
//   - "ready"  — returns the workflow to the ready queue (suspend)
//
// Fields not relevant to the chosen status are ignored.
func (s *MSSQLStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	if !validFinalStatus(finalStatus) {
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize workflow: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setSessionContext(tx); err != nil {
		return fmt.Errorf("finalize workflow: set session: %w", err)
	}

	// Append new events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize workflow: append events: %w", err)
	}

	// Delegate the terminal UPDATEs (status, idempotency, parent wake,
	// await_child population) to a server-side stored procedure.
	// This replaces 5 individual round-trips with 1 procedure call.
	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	resultJSON := result
	if resultJSON == "" || !json.Valid([]byte(resultJSON)) {
		resultJSON = "{}"
	}

	if _, err := tx.ExecContext(ctx, `
		EXEC finalize_workflow_status @p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10
	`, runID, workerID, generation, finalStatus, resultJSON, errorCode, errorOp, string(qsJSON), nextWakeAt, s.notifyChannel); err != nil {
		return fmt.Errorf("finalize workflow: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup for terminal statuses (post-commit).
	if finalStatus == "done" || finalStatus == "failed" {
		s.ClearStickyWorker(context.Background(), runID)
		s.ReleaseWorkflowConcurrencyKeys(context.Background(), runID)
		s.enforceParentClosePolicy(context.Background(), runID)
	}

	return nil
}

// RequestCancellation sets the cancellation flag on a workflow.
func (s *MSSQLStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("request cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = 1, cancellation_reason = @p2
		WHERE id = @p1
	`, workflowID, reason)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CheckCancellation checks if a workflow has been cancelled.
func (s *MSSQLStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	var cancelled bool
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = @p1
	`, workflowID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, nil
}

// StartNewRun creates a new workflow instance.
// If idempotencyKey is non-empty, provides exactly-once semantics.
func (s *MSSQLStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	if runID == "" {
		runID = uuid.New().String()
	}
	if idempotencyKey != "" {
		keyHash := sha256.Sum256([]byte(idempotencyKey))

		// Check for existing idempotency key.
		var existingWfID string
		err := s.db.QueryRowContext(ctx,
			`SELECT workflow_id FROM idempotency_keys
			 WHERE key_hash = @p1 AND expires_at > SYSUTCDATETIME()`,
			keyHash[:]).Scan(&existingWfID)
		if err == nil {
			return existingWfID, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}

		// Use the provided runID (already generated above).

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return "", false, err
		}
		defer tx.Rollback()

		// Insert idempotency key record. INSERT...WHERE NOT EXISTS handles the
		// race where two requests arrive with the same key simultaneously.
		ttlSeconds := int(s.idempotencyKeyTTL.Seconds())
		result, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at)
			 SELECT @p1, @p2, DATEADD(SECOND, @p3, SYSUTCDATETIME())
			 WHERE NOT EXISTS (
			     SELECT 1 FROM idempotency_keys WHERE key_hash = @p1
			 )`,
			keyHash[:], runID, ttlSeconds)
		if err != nil {
			return "", false, err
		}

		n, _ := result.RowsAffected()
		if n == 0 {
			// Key was inserted concurrently — rollback and return the existing one.
			tx.Rollback()
			err := s.db.QueryRowContext(ctx,
				`SELECT workflow_id FROM idempotency_keys
				 WHERE key_hash = @p1 AND expires_at > SYSUTCDATETIME()`,
				keyHash[:]).Scan(&existingWfID)
			if err != nil {
				return "", false, err
			}
			return existingWfID, true, nil
		}

		if err := s.setSessionContext(tx); err != nil {
			return "", false, fmt.Errorf("start new run: set session: %w", err)
		}

		// Insert the workflow instance.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
			VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
			        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3), 'default'),
			        @p5, @p6)
		`, runID, defName, defVersion, string(input), tenantID, priority)
		if err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		return runID, false, tx.Commit()
	}

	// No idempotency key — normal flow.
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
		        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3), 'default'),
		        @p5, @p6)
	`, runID, defName, defVersion, string(input), tenantID, priority)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	return runID, false, tx.Commit()
}

// ---------------------------------------------------------------------------
// Best-effort cleanup helpers
// ---------------------------------------------------------------------------

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Best-effort post-commit cleanup.
func (s *MSSQLStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		log.Printf("[store] enforceParentClosePolicy: begin TERMINATE tx: %v", err)
		return
	}
	defer tx.Rollback()
	tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated'
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
	tx.Commit()

	tx2, err := s.beginTxWithContext(ctx)
	if err != nil {
		log.Printf("[store] enforceParentClosePolicy: begin REQUEST_CANCEL tx: %v", err)
		return
	}
	defer tx2.Rollback()
	tx2.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = 1
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
	tx2.Commit()
}
