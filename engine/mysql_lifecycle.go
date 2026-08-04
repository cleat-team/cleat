package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// ClaimWorkflow
// ---------------------------------------------------------------------------

// ClaimWorkflow atomically dequeues a runnable workflow instance.
// Uses SELECT ... FOR UPDATE SKIP LOCKED. Delegates to ClaimWorkflows.
func (s *MySQLStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
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
// Uses SELECT ... FOR UPDATE SKIP LOCKED to avoid contention.
// MySQL does not support UPDATE ... RETURNING, so we use a three-step
// process inside a transaction: SELECT FOR UPDATE, UPDATE, SELECT.
func (s *MySQLStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqClause, tqArgs := s.taskQueueClause()

	// Step 1: Select IDs with SKIP LOCKED.
	// Arg order: task_queue values..., tenant_id, limit
	selArgs := make([]any, 0)
	selArgs = append(selArgs, tqArgs...)
	selArgs = append(selArgs, s.tenantID, limit)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM workflow_instances
		WHERE status = 'ready'
		  AND next_wake_at <= NOW(6)
		  AND task_queue IN (%s)
		  AND tenant_id = ?
		ORDER BY priority ASC, created_at
		LIMIT ?
		FOR UPDATE SKIP LOCKED
	`, tqClause), selArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: select: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("claim workflows: scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows: rows: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		tx.Rollback()
		return nil, nil
	}

	// Step 2: Update the claimed rows.
	idClause := inClausePlaceholders(len(ids))
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}

	updateArgs := append([]any{workerID}, idArgs...)
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = ?,
		    heartbeat_at = NOW(6),
		    generation = generation + 1
		WHERE id IN (%s)
	`, idClause), updateArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: update: %w", err)
	}

	// Step 3: Fetch the full rows.
	rows2, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, def_name, def_version, status, input, COALESCE(assigned_to, ''), next_wake_at, tenant_id, created_at, error_code, error_op, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id
		FROM workflow_instances
		WHERE id IN (%s)
	`, idClause), idArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: fetch: %w", err)
	}
	defer rows2.Close()

	var wfs []*WorkflowInstance
	for rows2.Next() {
		var wf WorkflowInstance
		if err := s.dialect.scanWorkflowInstanceExtra(rows2, &wf); err != nil {
			return nil, fmt.Errorf("claim workflows scan: %w", err)
		}
		wfs = append(wfs, &wf)
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	return s.finishClaim(ctx, tx, workerID, limit, wfs)
}

// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
// that are sticky to this worker. Uses SELECT ... FOR UPDATE SKIP LOCKED with
// sticky_worker_id filtering for low-contention claiming.
func (s *MySQLStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqClause, tqArgs := s.taskQueueClause()

	// Step 1: Select IDs with SKIP LOCKED (sticky filter).
	// Arg order: sticky_worker_id, task_queue values..., tenant_id, limit
	selArgs := make([]any, 0)
	selArgs = append(selArgs, workerID)
	selArgs = append(selArgs, tqArgs...)
	selArgs = append(selArgs, s.tenantID, limit)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM workflow_instances
		WHERE status = 'ready'
		  AND next_wake_at <= NOW(6)
		  AND sticky_worker_id = ?
		  AND task_queue IN (%s)
		  AND tenant_id = ?
		ORDER BY priority ASC, created_at
		LIMIT ?
		FOR UPDATE SKIP LOCKED
	`, tqClause), selArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: select: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("claim sticky workflows: scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows: rows: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		tx.Rollback()
		return nil, nil
	}

	// Step 2: Update the claimed rows.
	idClause := inClausePlaceholders(len(ids))
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}

	updateArgs := append([]any{workerID}, idArgs...)
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = ?,
		    heartbeat_at = NOW(6),
		    generation = generation + 1
		WHERE id IN (%s)
	`, idClause), updateArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: update: %w", err)
	}

	// Step 3: Fetch the full rows.
	rows2, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, def_name, def_version, status, input, COALESCE(assigned_to, ''), next_wake_at, tenant_id, created_at, error_code, error_op, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id
		FROM workflow_instances
		WHERE id IN (%s)
	`, idClause), idArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: fetch: %w", err)
	}
	defer rows2.Close()

	var wfs []*WorkflowInstance
	for rows2.Next() {
		var wf WorkflowInstance
		if err := s.dialect.scanWorkflowInstanceExtra(rows2, &wf); err != nil {
			return nil, fmt.Errorf("claim sticky workflows scan: %w", err)
		}
		wfs = append(wfs, &wf)
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows rows: %w", err)
	}

	return s.finishClaim(ctx, tx, workerID, limit, wfs)
}

// ---------------------------------------------------------------------------
// CompleteWorkflow, FailWorkflow, ReleaseWorkflow
// ---------------------------------------------------------------------------

// CompleteWorkflow marks a workflow as completed with a result.
func (s *MySQLStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("complete workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = ?, completed_at = NOW(6), assigned_to = NULL, query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, result, qsJSON, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete workflow: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the idempotency-key write and post-commit cleanup below
		// are not safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	// Record idempotency result within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET result = ? WHERE workflow_id = ?`,
		result, workflowID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: clear sticky worker assignment.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.
func (s *MySQLStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("fail workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed',
		    error_msg = ?,
		    error_code = ?,
		    error_op = ?,
		    completed_at = NOW(6),
		    assigned_to = NULL,
		    query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, errorMsg, errorCode, errorOp, qsJSON, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail workflow: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the idempotency-key write and post-commit cleanup below
		// are not safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = ? WHERE workflow_id = ?`,
		errorMsg, workflowID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: clear sticky worker assignment.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// ReleaseWorkflow returns a workflow to the ready queue with a next wake time.
// Used when a workflow suspends (sleep/await signals).
func (s *MySQLStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("release workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, nextWakeAt, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ---------------------------------------------------------------------------
// StartNewRun
// ---------------------------------------------------------------------------

// StartNewRun creates a new workflow instance.
// If idempotencyKey is non-empty, provides exactly-once semantics: a subsequent
// call with the same key returns the existing workflow ID without creating a
// duplicate. Returns the workflow ID, whether it already existed, and any error.
func (s *MySQLStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	if runID == "" {
		runID = uuid.New().String()
	}
	if idempotencyKey != "" {
		keyHash := sha256.Sum256([]byte(idempotencyKey))

		// Check for existing idempotency key.
		var existingWfID string
		err := s.db.QueryRowContext(ctx,
			`SELECT workflow_id FROM idempotency_keys
			 WHERE key_hash = ? AND expires_at > NOW(6)`,
			keyHash[:]).Scan(&existingWfID)
		if err == nil {
			return existingWfID, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return "", false, err
		}
		defer tx.Rollback()

		// Insert idempotency key record. INSERT IGNORE handles the race where
		// two requests arrive with the same key simultaneously.
		ttlSeconds := int(s.idempotencyKeyTTL.Seconds())
		res, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO idempotency_keys (key_hash, workflow_id, expires_at)
			 VALUES (?, ?, DATE_ADD(NOW(6), INTERVAL ? SECOND))`,
			keyHash[:], runID, ttlSeconds)
		if err != nil {
			return "", false, err
		}

		n, _ := res.RowsAffected()
		if n == 0 {
			// Key was inserted concurrently -- rollback and return the existing one.
			tx.Rollback()
			err := s.db.QueryRowContext(ctx,
				`SELECT workflow_id FROM idempotency_keys
				 WHERE key_hash = ? AND expires_at > NOW(6)`,
				keyHash[:]).Scan(&existingWfID)
			if err != nil {
				return "", false, err
			}
			return existingWfID, true, nil
		}

		// Insert the workflow instance.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
			VALUES (?, ?, ?, 'ready', ?,
			        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
			        ?, ?)
		`, runID, defName, defVersion, input, defName, defVersion, tenantID, priority)
		if err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		return runID, false, tx.Commit()
	}

	// No idempotency key -- normal flow.
	tx, err := s.beginTx(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES (?, ?, ?, 'ready', ?,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
		        ?, ?)
	`, runID, defName, defVersion, input, defName, defVersion, tenantID, priority)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	return runID, false, tx.Commit()
}

// ---------------------------------------------------------------------------
// ContinueAsNew
// ---------------------------------------------------------------------------

// ContinueAsNew atomically creates a new workflow run AND completes the
// current one in a single database transaction. If the transaction fails
// neither operation takes effect. Returns the new run ID on success.
func (s *MySQLStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return "", fmt.Errorf("continue as new: begin: %w", err)
	}
	defer tx.Rollback()

	// Append events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, currentRunID, newEvents); err != nil {
		return "", fmt.Errorf("continue as new: append events: %w", err)
	}

	// Use the store's tenant scope to preserve tenant isolation.
	// Create the new workflow run.
	// Use the store's tenant scope to preserve tenant isolation.
	newRunID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES (?, ?, ?, 'ready', ?,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
		        ?, ?)
	`, newRunID, defName, defVersion, newInput, defName, defVersion, s.tenantID, priority)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// Complete the current run.
	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = ?, completed_at = NOW(6), assigned_to = NULL, query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, result, qsJSON, currentRunID, workerID, s.tenantID, generation)
	if err != nil {
		return "", fmt.Errorf("continue as new: complete old run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("continue as new: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: this also discards the new run row we just inserted, so
		// a lost fence leaves no orphaned, unreachable continuation run
		// behind.
		return "", ErrFenceLost
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

// ---------------------------------------------------------------------------
// FinalizeWorkflowSegment
// ---------------------------------------------------------------------------

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
func (s *MySQLStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	if !validFinalStatus(finalStatus) {
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize workflow: begin tx: %w", err)
	}
	defer tx.Rollback()

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

	// p_next_wake_at is only meaningful for the "ready" status; callers
	// finalizing as "done"/"failed" routinely pass the zero time.Time{}.
	// The go-sql-driver/mysql driver encodes a Go zero time as MySQL's
	// legacy zero-date sentinel "0000-00-00 00:00:00", which MySQL's
	// default strict sql_mode (NO_ZERO_DATE, on by default since 5.7)
	// rejects with Error 1292 "Incorrect datetime value". Postgres and
	// MSSQL both accept a year-1 timestamp fine, so this is MySQL-only.
	// Pass NULL instead when the caller didn't supply a real time.
	var nextWakeParam interface{}
	if !nextWakeAt.IsZero() {
		nextWakeParam = nextWakeAt
	}

	var fenceHeld bool
	if err := tx.QueryRowContext(ctx, `
		CALL finalize_workflow_status(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, runID, workerID, generation, finalStatus, resultJSON, errorCode, errorOp, string(qsJSON), nextWakeParam, s.notifyChannel).Scan(&fenceHeld); err != nil {
		return fmt.Errorf("finalize workflow: %w", err)
	}

	if !fenceHeld {
		// Another worker now owns this workflow (e.g. this worker stalled,
		// was reaped, and the workflow was reclaimed). Roll back rather
		// than commit: the events we just appended belong to a segment
		// that is no longer valid, and none of the post-commit cleanup
		// below is safe to run on the new owner's behalf.
		return ErrFenceLost
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

// ---------------------------------------------------------------------------
// Heartbeat / BatchHeartbeat
// ---------------------------------------------------------------------------

// Heartbeat updates the heartbeat timestamp to prevent timeout.
// Returns false if the workflow is no longer assigned to this worker.
func (s *MySQLStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = NOW(6)
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// BatchHeartbeat updates heartbeat_at for all workflows assigned to this
// worker with status 'running'. Uses a single UPDATE instead of N calls.
// NOTE: This intentionally does NOT check per-workflow generation because it
// operates on ALL running workflows for a worker, and generations differ per
// workflow. Individual generation-guarded operations (Heartbeat,
// CompleteWorkflow, FailWorkflow, etc.) prevent double-execution even if the
// batch heartbeat refreshes a stale workflow's heartbeat_at.
func (s *MySQLStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = NOW(6)
		WHERE assigned_to = ? AND status = 'running' AND tenant_id = ?
	`, workerID, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("batch heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// ---------------------------------------------------------------------------
// MoveToDeadLetterQueue
// ---------------------------------------------------------------------------

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts. This is a terminal status similar to
// 'failed' but indicates the workflow was retried without success.
func (s *MySQLStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = ?, error_code = ?, error_op = ?,
		    completed_at = NOW(6), assigned_to = NULL
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, errMsg, errorCode, errorOp, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("move to dead letter queue: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the idempotency-key write and post-commit cleanup below
		// are not safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = ? WHERE workflow_id = ?`,
		errMsg, workflowID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: clear sticky worker assignment.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// RetryWorkflow moves a dead_lettered workflow back to a runnable state.
func (s *MySQLStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = NOW(6)
		WHERE id = ? AND status = 'dead_lettered'
	`, workflowID)
	return err
}

// ---------------------------------------------------------------------------
// ReapStaleInstances
// ---------------------------------------------------------------------------

// ReapStaleInstances reclaims workflow instances that have been running
// but whose heartbeat has not been updated within the given timeout.
// Returns the number of instances reclaimed.
func (s *MySQLStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1
		WHERE status = 'running'
		  AND heartbeat_at < NOW(6) - INTERVAL ? SECOND
		  AND tenant_id = ?
	`, int(timeout.Seconds()), s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// ---- ParentClosePolicy ----

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Runs as a best-effort operation.
func (s *MySQLStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	// Terminate children with TERMINATE policy.
	s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated'
		WHERE parent_workflow_id = ?
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)

	// Request cancellation for children with REQUEST_CANCEL policy.
	s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true
		WHERE parent_workflow_id = ?
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
}

// finishClaim commits a claim transaction and enforces the claim-limit
// invariant, releasing any excess rather than truncating it away. See
// enforceClaimLimit in claim_limit.go for why.
func (s *MySQLStore) finishClaim(ctx context.Context, tx *sql.Tx, workerID string, limit int, wfs []*WorkflowInstance) ([]*WorkflowInstance, error) {
	keep, excess := enforceClaimLimit(ctx, s.log(), "mysql", workerID, limit, wfs)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, wf := range excess {
		if err := s.ReleaseWorkflow(context.Background(), wf.ID, workerID, wf.Generation, wf.NextWakeAt); err != nil {
			s.log().ErrorContext(ctx, "releasing an over-claimed workflow failed; it stays claimed until its lease expires",
				"worker_id", workerID, "workflow_id", wf.ID, "error", err)
		}
	}
	return keep, nil
}
