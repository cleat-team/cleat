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
	"github.com/lib/pq"
)

func (s *PostgresStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	return s.claimWorkflowImpl(ctx, workerID)
}

func (s *PostgresStore) claimWorkflowImpl(ctx context.Context, workerID string) (*WorkflowInstance, error) {
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
// Like ClaimWorkflow but batches multiple claims into one query.

func (s *PostgresStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = $1,
		    heartbeat_at = now(),
		    generation = generation + 1
		WHERE id IN (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			  AND task_queue = ANY($2)
			ORDER BY priority ASC, created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, def_name, def_version, status, input, assigned_to, next_wake_at, tenant_id, created_at, error_code, error_op, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id
	`, workerID, pq.Array(s.taskQueues), limit)
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
		var assignedTo, errorCode, errorOp sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &wf.TraceID); err != nil {
			return nil, fmt.Errorf("claim workflows scan: %w", err)
		}

		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.AssignedTo = assignedTo.String
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}
	return wfs, tx.Commit()
}

// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
// that are sticky to this worker. Filters on sticky_worker_id to use the
// idx_instances_sticky partial index for low-contention claiming.

func (s *PostgresStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = $1,
		    heartbeat_at = now(),
		    generation = generation + 1
		WHERE id IN (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			  AND sticky_worker_id = $1
			  AND task_queue = ANY($2)
			ORDER BY priority ASC, created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, def_name, def_version, status, input, assigned_to, next_wake_at, tenant_id, created_at, error_code, error_op, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id
	`, workerID, pq.Array(s.taskQueues), limit)
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
		var assignedTo, errorCode, errorOp sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &wf.TraceID); err != nil {
			return nil, fmt.Errorf("claim sticky workflows scan: %w", err)
		}

		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.AssignedTo = assignedTo.String
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}
	return wfs, tx.Commit()
}

// LoadEventHistory returns all event records for a workflow, ordered by step.

func (s *PostgresStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("continue as new: begin: %w", err)
	}
	defer tx.Rollback()

	// Append events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, currentRunID, newEvents); err != nil {
		return "", fmt.Errorf("continue as new: append events: %w", err)
	}

	// Create the new workflow run.
	// Use the store's tenant scope to preserve tenant isolation.
	var newRunID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, next_wake_at)
		VALUES (gen_random_uuid(), $1, $2, 'ready', $3,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $1 AND version = $2), 'default'),
			$4, $5)
		RETURNING id
		`, defName, defVersion, newInput, s.tenantID, priority).Scan(&newRunID)
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
		SET status = 'done', result = $3, completed_at = now(), assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2 AND generation = $5
	`, currentRunID, workerID, result, qsJSON, generation)
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
	_ = s.ClearStickyWorker(context.Background(), currentRunID)
	_ = s.ReleaseWorkflowConcurrencyKeys(context.Background(), currentRunID)
	s.enforceParentClosePolicy(context.Background(), currentRunID)

	return newRunID, nil
}

// FinalizeWorkflowSegment atomically appends new events and updates the
// workflow status in a single database transaction.  This eliminates the
// race between AppendEventHistoryBatch and the subsequent CompleteWorkflow /
// FailWorkflow / ReleaseWorkflow call.
//
// finalStatus must be one of:
//   - "done"   — marks the workflow as completed with the given result
//   - "failed" — marks the workflow as failed with the given error info
//   - "ready"  — returns the workflow to the ready queue (suspend)
//
// Fields not relevant to the chosen status are ignored.

func (s *PostgresStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	if !validFinalStatus(finalStatus) {
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize workflow: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return fmt.Errorf("finalize workflow: set rls: %w", err)
	}

	// Append new events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize workflow: append events: %w", err)
	}

	// Delegate the terminal UPDATEs (status, idempotency, parent wake,
	// await_child population, pg_notify) to a server-side PL/pgSQL function.
	// This replaces 5 individual round-trips with 1 function call.
	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	resultJSON := result
	if resultJSON == "" || !json.Valid([]byte(resultJSON)) {
		resultJSON = "{}"
	}

	var fenceHeld bool
	if err := tx.QueryRowContext(ctx, `
		SELECT finalize_workflow_status($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, runID, workerID, generation, finalStatus, resultJSON, errorCode, errorOp, string(qsJSON), nextWakeAt, s.notifyChannel).Scan(&fenceHeld); err != nil {
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
		_ = s.ClearStickyWorker(context.Background(), runID)
		_ = s.ReleaseWorkflowConcurrencyKeys(context.Background(), runID)
		s.enforceParentClosePolicy(context.Background(), runID)
	}

	return nil
}

// validFinalStatus returns true for status values accepted by finalize_workflow_status.
func validFinalStatus(status string) bool {
	switch status {
	case "done", "failed", "ready", "suspended":
		return true
	}
	return false
}

// AppendEventHistory appends a single event to the history.

func (s *PostgresStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	tx, err := s.beginTxWithRLS(ctx)
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
		SET status = 'done', result = $3, completed_at = now(), assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2 AND generation = $5
	`, workflowID, workerID, result, qsJSON, generation)
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
		`UPDATE idempotency_keys SET result = $2 WHERE workflow_id = $1`,
		workflowID, result); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: clear sticky worker assignment (Feature 10).
	_ = s.ClearStickyWorker(context.Background(), workflowID)
	// Best-effort: release all concurrency keys (Feature 5).
	_ = s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.

func (s *PostgresStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTxWithRLS(ctx)
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
		    error_msg = $3,
		    error_code = $4,
		    error_op = $5,
		    completed_at = now(),
		    assigned_to = NULL,
		    query_state = $6
		WHERE id = $1 AND assigned_to = $2 AND generation = $7
	`, workflowID, workerID, errorMsg, errorCode, errorOp, string(qsJSON), generation)
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
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1`,
		workflowID, errorMsg); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: clear sticky worker assignment (Feature 10).
	s.ClearStickyWorker(context.Background(), workflowID)
	// Best-effort: release all concurrency keys (Feature 5).
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Best-effort post-commit cleanup.

func (s *PostgresStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: begin TERMINATE tx failed", "error", err)
		return
	}
	defer tx.Rollback()
	tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated'
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
	tx.Commit()

	tx2, err := s.beginTxWithRLS(ctx)
	if err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: begin REQUEST_CANCEL tx failed", "error", err)
		return
	}
	defer tx2.Rollback()
	tx2.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
	tx2.Commit()
}

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts.

func (s *PostgresStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = $3, error_code = $4, error_op = $5,
		    completed_at = now(), assigned_to = NULL
		WHERE id = $1 AND assigned_to = $2 AND generation = $6
	`, workflowID, workerID, errMsg, errorCode, errorOp, generation)
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
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1`,
		workflowID, errMsg); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: clear sticky worker assignment (Feature 10).
	s.ClearStickyWorker(context.Background(), workflowID)
	// Best-effort: release all concurrency keys (Feature 5).
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// RetryWorkflow moves a dead_lettered workflow back to a runnable state.

func (s *PostgresStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("retry workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = now()
		WHERE id = $1 AND status = 'dead_lettered'
	`, workflowID)
	if err != nil {
		return err
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// ReleaseWorkflow returns a workflow to the queue with a next wake time.

func (s *PostgresStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("release workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = $3
		WHERE id = $1 AND assigned_to = $2 AND generation = $4
	`, workflowID, workerID, nextWakeAt, generation)
	if err != nil {
		return err
	}

	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// RequestCancellation sets the cancellation flag.

func (s *PostgresStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	if runID == "" {
		runID = uuid.New().String()
	}
	if idempotencyKey != "" {
		keyHash := sha256.Sum256([]byte(idempotencyKey))

		// Check for existing idempotency key.
		var existingWfID string
		err := s.db.QueryRowContext(ctx,
			`SELECT workflow_id FROM idempotency_keys
			 WHERE key_hash = $1 AND expires_at > now()`,
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
		defer tx.Rollback() // Note: explicit tx.Rollback() below when wfs is empty is intentional — the defer would also catch it, but early rollback releases the lock immediately rather than waiting for function return.

		// Insert idempotency key record. ON CONFLICT DO NOTHING handles the
		// race where two requests arrive with the same key simultaneously.
		ttlSeconds := int(s.idempotencyKeyTTL.Seconds())
		res, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at)
			 VALUES ($1, $2, now() + ($3 * INTERVAL '1 second'))
			 ON CONFLICT (key_hash) DO NOTHING`,
			keyHash[:], runID, ttlSeconds)
		if err != nil {
			return "", false, err
		}

		n, _ := res.RowsAffected()
		if n == 0 {
			// Key was inserted concurrently — rollback and return the existing one.
			_ = tx.Rollback()
			err := s.db.QueryRowContext(ctx,
				`SELECT workflow_id FROM idempotency_keys
				 WHERE key_hash = $1 AND expires_at > now()`,
				keyHash[:]).Scan(&existingWfID)
			if err != nil {
				return "", false, err
			}
			return existingWfID, true, nil
		}

		if err := s.setRLSOnTx(tx); err != nil {
			return "", false, fmt.Errorf("start new run: set rls: %w", err)
		}

		// Insert the workflow instance.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, next_wake_at)
			VALUES ($1, $2, $3, 'ready', $4,
			        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3), 'default'),
			$5, $6, now() - INTERVAL '1 millisecond')
		`, runID, defName, defVersion, input, tenantID, priority)
		if err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		pgNotify(ctx, tx, s.notifyChannel)
		return runID, false, tx.Commit()
	}

	// No idempotency key — normal flow.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, next_wake_at)
		VALUES ($1, $2, $3, 'ready', $4,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3), 'default'),
			$5, $6, now() - INTERVAL '1 millisecond')
	`, runID, defName, defVersion, input, tenantID, priority)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return runID, false, tx.Commit()
}

// StartChildWorkflow creates a child workflow instance linked to a parent.
// The child is created with its own independent workflow instance.
// If defVersion > 0, that version is used explicitly; otherwise the latest
// non-deprecated version is used (SELECT MAX(version)).

func (s *PostgresStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1
		WHERE status = 'running'
		  AND heartbeat_at < now() - $1::interval
	`, fmt.Sprintf("%d seconds", int(timeout.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), tx.Commit()
}

// ---- SignalStore interface implementation ----

// DeliverSignal satisfies the SignalStore interface.
