package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

	// The candidate set is selected in a CTE rather than an
	// `id IN (SELECT ... LIMIT n FOR UPDATE SKIP LOCKED)` sublink. Both forms
	// respect the limit; this one is kept because it is evaluated once by
	// construction rather than by argument.
	//
	// This comment used to claim the sublink form was unsafe -- that an
	// EvalPlanQual recheck re-executes it and a claim for n could update far
	// more than n. That explanation is wrong, and is corrected here rather
	// than left in place, because a plausible-sounding false mechanism in a
	// comment is worse than no comment. Two independent reasons it cannot
	// happen, on PostgreSQL 16:
	//
	//   - The sublink is uncorrelated, so the planner pulls it up into a
	//     semi-join. EXPLAIN (ANALYZE, VERBOSE) of the old form shows the
	//     candidate subquery as the *outer* side of a nested loop, executed
	//     once (loops=1) and unique-ified through a HashAggregate, with a
	//     primary-key index scan on the inner side. The UPDATE therefore
	//     visits exactly the candidate rows and no others. EvalPlanQual can
	//     only keep or drop a row the UPDATE already visits; it cannot add
	//     rows to the update set.
	//   - The sublink's LockRows node takes FOR UPDATE on the candidates
	//     before the outer UPDATE reaches them, so no concurrent transaction
	//     can modify those rows mid-statement. EvalPlanQual has nothing to
	//     fire on.
	//
	// Also checked empirically against the old form: 24,000 claims with 12
	// concurrent claimers and 10 disrupting transactions committing mid-claim
	// -- including ones mutating `status`, which the sublink's own WHERE
	// clause reads -- over candidate sets of 40, 400 and 5010 rows. The most
	// any single claim ever returned was exactly the limit.
	//
	// So the "asked for 3, got 10" observation in IMPROVEMENT-PLAN.md 2.11 is
	// still unexplained, but it is not this. Do not treat the CTE as the fix
	// for it.
	rows, err := tx.QueryContext(ctx, `
		WITH candidates AS (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			  AND task_queue = ANY($2)
			ORDER BY priority ASC, created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE workflow_instances w
		SET status = 'running',
		    assigned_to = $1,
		    heartbeat_at = now(),
		    generation = generation + 1
		FROM candidates c
		WHERE w.id = c.id
		RETURNING w.id, w.def_name, w.def_version, w.status, w.input, w.assigned_to, w.next_wake_at, w.tenant_id, w.created_at, w.error_code, w.error_op, w.generation, COALESCE(w.priority, 0) AS priority, COALESCE(w.trace_id, '') AS trace_id
	`, workerID, pq.Array(s.taskQueues), limit)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: %w", err)
	}
	defer rows.Close()

	wfs, err := scanClaimedWorkflows(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}
	return s.finishClaim(ctx, tx, workerID, limit, wfs)
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

	// CTE rather than an IN (SELECT ... LIMIT n) sublink, for consistency with
	// ClaimWorkflows above -- see the note there, including why the
	// EvalPlanQual explanation this comment used to give is wrong.
	rows, err := tx.QueryContext(ctx, `
		WITH candidates AS (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			  AND sticky_worker_id = $1
			  AND task_queue = ANY($2)
			ORDER BY priority ASC, created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE workflow_instances w
		SET status = 'running',
		    assigned_to = $1,
		    heartbeat_at = now(),
		    generation = generation + 1
		FROM candidates c
		WHERE w.id = c.id
		RETURNING w.id, w.def_name, w.def_version, w.status, w.input, w.assigned_to, w.next_wake_at, w.tenant_id, w.created_at, w.error_code, w.error_op, w.generation, COALESCE(w.priority, 0) AS priority, COALESCE(w.trace_id, '') AS trace_id
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
		var errorCode, errorOp sql.NullString

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
	return s.finishClaim(ctx, tx, workerID, limit, wfs)
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
			$4, $5, now() - INTERVAL '1 millisecond')
		RETURNING id
		`, defName, defVersion, newInput, s.tenantID, priority).Scan(&newRunID)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// Complete the current run.
	qsJSON := marshalQueryState(queryState)
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

	releaseWorkflowResources(s.log(), s, currentRunID)
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
	qsJSON := marshalQueryState(queryState)
	resultJSON := coerceResultJSON(ctx, s.log(), runID, result)

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
		releaseWorkflowResources(s.log(), s, runID)
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

	qsJSON := marshalQueryState(queryState)
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
	//
	// AND tenant_id = $3, not workflow_id alone: this UPDATE ran unscoped by
	// tenant, so it matched any row across every tenant whose workflow_id
	// happened to equal this one. workflow_id is generated per call
	// (uuid.New() when the caller supplies none) so a same-tenant collision
	// is astronomically unlikely, but a caller-supplied runID is not
	// guaranteed unique *across* tenants the way it is guaranteed unique
	// *within* one (StartNewRun's idempotency_keys primary key is now
	// (key_hash, tenant_id), migrations/postgres/010). s.tenantID is set
	// on this tx already -- beginTxWithRLS calls setRLSOnTx before any
	// caller reaches here -- so this is a Go-level filter matching the RLS
	// policy's own scope, not a new source of truth for it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET result = $2 WHERE workflow_id = $1 AND tenant_id = $3`,
		workflowID, result, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

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

	qsJSON := marshalQueryState(queryState)
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
	//
	// AND tenant_id = $3: see the identical comment on the sibling UPDATE in
	// CompleteWorkflow. s.tenantID is already set on this tx by
	// beginTxWithRLS.
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1 AND tenant_id = $3`,
		workflowID, errorMsg, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Best-effort post-commit cleanup.

// enforceParentClosePolicy applies a closing parent's policy to its children.
//
// It used to discard every error it produced: neither ExecContext's nor
// Commit's return value was assigned, in either transaction. When it failed,
// the children of a closed parent were simply unaffected by its policy --
// TERMINATE children kept running, REQUEST_CANCEL children were never
// flagged -- with no log line, no metric and no error anywhere. The function
// is void and its callers treat it as best-effort post-commit cleanup, so
// nothing downstream noticed either. See IMPROVEMENT-PLAN.md 2.50.
//
// It stays void: the contract with callers has not changed, only whether a
// failure is observable.
func (s *PostgresStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	steps := []struct {
		policy string
		query  string
	}{
		{"TERMINATE", `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated'
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed')
	`},
		{"REQUEST_CANCEL", `
		UPDATE workflow_instances
		SET cancellation_requested = true
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed')
	`},
	}

	// Collected before the UPDATE: see releaseTerminatedChildren for why not
	// RETURNING. A failure here costs the release, not the close policy.
	terminated, err := s.childrenClosedByTerminate(ctx, parentWorkflowID)
	if err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: could not list TERMINATE children; their concurrency keys and sticky-worker assignments stay held until TTL",
			"parent_workflow_id", parentWorkflowID, "error", err)
	}

	for _, step := range steps {
		if err := s.runParentClosePolicyStep(ctx, step.query, parentWorkflowID); err != nil {
			s.log().WarnContext(ctx, "enforceParentClosePolicy failed; children of a closed parent are unaffected by its close policy",
				"policy", step.policy, "parent_workflow_id", parentWorkflowID, "error", err)
			if step.policy == "TERMINATE" {
				terminated = nil
			}
		}
	}

	releaseTerminatedChildren(s.log(), s, terminated)
}

// terminateChildrenQuery selects the children the TERMINATE arm is about to
// fail, so their resources can be released after it commits. Its WHERE must
// stay identical to that UPDATE's, or the two disagree about which children
// were closed.
func (s *PostgresStore) childrenClosedByTerminate(ctx context.Context, parentWorkflowID string) ([]string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM workflow_instances
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
	if err != nil {
		return nil, err
	}
	return scanWorkflowIDs(rows)
}

func (s *PostgresStore) runParentClosePolicyStep(ctx context.Context, query, parentWorkflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, query, parentWorkflowID); err != nil {
		return err
	}
	return tx.Commit()
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
	//
	// AND tenant_id = $3: see the identical comment on the sibling UPDATE in
	// CompleteWorkflow. s.tenantID is already set on this tx by
	// beginTxWithRLS.
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1 AND tenant_id = $3`,
		workflowID, errMsg, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

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

		// Check for existing idempotency key, within this tenant.
		//
		// The tenant filter is not defence in depth: idempotency_keys was
		// keyed by key_hash alone, so an Idempotency-Key was global across
		// every tenant in the deployment. Two customers both choosing
		// "order-123" collided, and the second was handed the first's
		// workflow ID with alreadyExisted = true while its own workflow was
		// never started. The key is a client-supplied request header, so that
		// is the expected outcome of ordinary naming rather than an attack.
		// migrations/*/010_idempotency_keys_tenant_id.sql, IMPROVEMENT-PLAN
		// 3.10.
		var existingWfID string
		err := s.db.QueryRowContext(ctx,
			`SELECT workflow_id FROM idempotency_keys
			 WHERE key_hash = $1 AND tenant_id = $2 AND expires_at > now()`,
			keyHash[:], tenantID).Scan(&existingWfID)
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
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
			 VALUES ($1, $2, now() + ($3 * INTERVAL '1 second'), $4)
			 ON CONFLICT (key_hash, tenant_id) DO NOTHING`,
			keyHash[:], runID, ttlSeconds, tenantID)
		if err != nil {
			return "", false, err
		}

		n, _ := res.RowsAffected()
		if n == 0 {
			// Key was inserted concurrently — rollback and return the existing one.
			_ = tx.Rollback()
			err := s.db.QueryRowContext(ctx,
				`SELECT workflow_id FROM idempotency_keys
				 WHERE key_hash = $1 AND tenant_id = $2 AND expires_at > now()`,
				keyHash[:], tenantID).Scan(&existingWfID)
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

// finishClaim commits a claim transaction and enforces the claim-limit
// invariant, releasing any excess rather than truncating it away. See
// enforceClaimLimit in claim_limit.go for why.
func (s *PostgresStore) finishClaim(ctx context.Context, tx *sql.Tx, workerID string, limit int, wfs []*WorkflowInstance) ([]*WorkflowInstance, error) {
	keep, excess := enforceClaimLimit(ctx, s.log(), "postgres", workerID, limit, wfs)
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

// coerceResultJSON returns a result safe for the JSON-typed result column, and
// says so out loud when it had to replace one.
//
// The column is JSONB on PostgreSQL, JSON on MySQL and NVARCHAR(MAX) under an
// ISJSON check on SQL Server, so a result that is not valid JSON cannot be
// stored and something has to give. Replacing it with "{}" is the right call --
// failing the terminal write would lose the whole workflow over a formatting
// defect -- but doing it silently is not.
//
// It was silent, and that is how IMPROVEMENT-PLAN 3.22 erased an ambiguous call
// rather than merely mislabelling it: a workflow whose durable call came back
// [AMBIGUOUS] returned `{"error":""durable call ...""}` -- doubled quotes, from
// a generator that wraps an already-quoted string a second time -- which is
// invalid, so the workflow was stored `done` with result `{}` and no error
// anywhere. The engine had detected the ambiguity correctly and every trace of
// it was dropped here, in a two-line conditional with no log statement.
//
// The empty case is not logged: an entry point with no return value produces it
// on every successful run, and it is what the column default means anyway.
func coerceResultJSON(ctx context.Context, log *slog.Logger, workflowID, result string) string {
	if result == "" {
		return "{}"
	}
	if json.Valid([]byte(result)) {
		// Valid JSON is not the same as conforming to the contract. An entry
		// point returns a string containing a JSON-encoded OBJECT, and
		// json.Valid happily accepts a bare string, number or array -- which is
		// precisely why a double-encoded result ("{\"ok\":true}" as a JSON
		// string) sailed through here undetected across three SDKs.
		//
		// Reported, not rejected. Replacing a valid-but-wrong-shaped result
		// with {} would destroy data that is at least storable, and workflows
		// predating the contract still return scalars. What this removes is the
		// silence: a violation now names itself, with the workflow that
		// produced it.
		if log != nil && !looksLikeJSONObject(result) {
			log.ErrorContext(ctx, "workflow result is valid JSON but not an object -- the "+
				"contract is a string containing a JSON-encoded object, so this is stored as-is "+
				"but no consumer can rely on its shape. A result that starts with a quote is "+
				"usually an SDK encoding a value that was already JSON",
				"workflow_id", workflowID, "result_len", len(result),
				"result", truncateForLog(result))
		}
		return result
	}
	if log != nil {
		log.ErrorContext(ctx, "workflow result is not valid JSON and was replaced with {} -- "+
			"whatever it carried, including any error the workflow returned, is not stored anywhere",
			"workflow_id", workflowID, "result_len", len(result), "result", truncateForLog(result))
	}
	return "{}"
}

// truncateForLog bounds a value that goes into a log line. A workflow result is
// caller-controlled and can be large.
func truncateForLog(s string) string {
	const max = 512
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}

// scanClaimedWorkflows reads the rows a claim returns.
//
// Shared by ClaimWorkflows and ClaimWorkflowsAcrossTenants deliberately: the
// second reads its columns from admin.claim_workflows, a function defined in a
// migration, and the only thing keeping that definition in step with this scan
// is that there is exactly one scan. Two copies would drift, and the symptom
// would be a scan error at claim time on whichever deployment ran the newer
// migration.
func scanClaimedWorkflows(rows *sql.Rows) ([]*WorkflowInstance, error) {
	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt, createdAt sql.NullTime
		var tenantID, errorCode, errorOp sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp,
			&wf.Generation, &wf.Priority, &wf.TraceID); err != nil {
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
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	return wfs, rows.Err()
}

// ErrCrossTenantClaimUnsupported is returned by a store that implements
// CrossTenantClaimer but cannot honour it in the topology it finds itself in.
//
// Implementing the interface and quietly returning one tenant's work would be
// worse than not implementing it: the caller would believe it was claiming for
// everyone. MySQL is the case that forced this -- MySQLStoreFactory gives each
// tenant its own physical database, so there is no predicate to drop; the other
// tenants' rows are not filtered out, they are in a different database. The
// same type is also used against a single shared database, where the claim IS
// meaningful, so the answer depends on how the store was built rather than on
// which type it is.
var ErrCrossTenantClaimUnsupported = errors.New("cross-tenant claim not supported by this store's topology")

// CrossTenantClaimer is implemented by stores that can claim runnable work for
// every tenant in a single query.
//
// It is deliberately NOT part of WorkflowStore. A store that cannot do it --
// because its dialect has no mechanism, or because the deployment has not
// granted one -- simply does not implement it, and the caller falls back to the
// ordinary tenant-scoped claim. Putting it on WorkflowStore would force every
// implementation and every test double to answer a question most of them have
// no business answering.
//
// The returned instances carry TenantID, and the caller MUST re-scope to it
// before touching anything else. That is the whole bargain: one query sees
// across tenants so the dispatch loop does not have to poll each one, and
// everything downstream of it is scoped again immediately. See
// cmd/cleat-worker's storeForTenant.
type CrossTenantClaimer interface {
	ClaimWorkflowsAcrossTenants(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error)
}

// ClaimWorkflowsAcrossTenants claims runnable workflows for every tenant.
//
// It calls admin.claim_workflows (migrations/postgres/023_cross_tenant_claim.sql)
// rather than issuing the claim directly, because the exemption that lets it
// see across tenants belongs to that function's owner and nowhere else. The
// statement inside is the same one ClaimWorkflows runs; the column list here is
// the contract with it.
//
// No beginTxWithRLS. That helper exists to set the tenant GUC the policies read,
// and this call must not be scoped to a tenant -- setting one would filter the
// very rows it exists to find. The function performs its own UPDATE, so a single
// statement is already atomic.
func (s *PostgresStore) ClaimWorkflowsAcrossTenants(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, def_name, def_version, status, input, assigned_to, next_wake_at,
		       tenant_id, created_at, error_code, error_op, generation, priority, trace_id
		FROM admin.claim_workflows($1, $2, $3)
	`, workerID, pq.Array(s.taskQueues), limit)
	if err != nil {
		if reason := crossTenantProvisioningGap(err); reason != "" {
			return nil, fmt.Errorf("claim workflows across tenants: %s: %w", reason, ErrCrossTenantClaimUnsupported)
		}
		return nil, fmt.Errorf("claim workflows across tenants: %w", err)
	}
	defer rows.Close()

	return scanClaimedWorkflows(rows)
}

// crossTenantProvisioningGap distinguishes "this deployment never provisioned
// the cross-tenant claim" from "the claim ran and something went wrong".
//
// The distinction decides whether the worker keeps running. A provisioning gap
// is answered by falling back to the per-tenant claim and warning once, which
// keeps dispatch alive on a deployment that opted into a flag it had not yet
// granted; anything else propagates, because a claim that fails for a reason
// nobody anticipated should not be quietly downgraded into a narrower one that
// works.
//
// Both codes here mean the same thing from the operator's side -- migration 023
// has not been applied and granted -- and neither is reachable once it has:
//
//	42883 undefined_function     admin.claim_workflows does not exist
//	42501 insufficient_privilege it exists, but EXECUTE was never granted
//	                             (023 revokes from PUBLIC, so this is the
//	                             default state until a role is granted)
//
// A missing BYPASSRLS on the owner is deliberately NOT in this list. It does
// not raise: the function runs and returns only the rows RLS admits, which is
// the silent-wrong-answer case this whole mechanism exists to avoid. Nothing
// here can detect it, which is why 023 sets the attribute in the same migration
// that creates the role rather than leaving it to a deployment step.
func crossTenantProvisioningGap(err error) string {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return ""
	}
	switch pqErr.Code {
	case "42883":
		return "admin.claim_workflows does not exist; apply migrations/postgres/023_cross_tenant_claim.sql"
	case "42501":
		return "this connection may not EXECUTE admin.claim_workflows; grant it as " +
			"migrations/postgres/023_cross_tenant_claim.sql documents"
	}
	return ""
}

// CrossTenantScheduleReader is implemented by stores that can read due
// schedules for every tenant in a single query.
//
// Separate from CrossTenantClaimer rather than folded into it, because the two
// are provisioned separately and a deployment can legitimately have one and not
// the other: 023 grants the claim, 024 grants this, and a database that applied
// only the first should get a working dispatch loop and a loud warning about
// schedules rather than a worker that refuses both.
//
// The returned schedules carry TenantID, and the caller MUST re-scope to it
// before doing anything else -- starting the run, and claiming the schedule.
// The widened view covers the read and nothing after it. See
// cmd/cleat-worker's scheduleLoop.
type CrossTenantScheduleReader interface {
	GetDueSchedulesAcrossTenants(ctx context.Context) ([]Schedule, error)
}

// GetDueSchedulesAcrossTenants returns every tenant's due schedules.
//
// It calls admin.get_due_schedules (migrations/postgres/024_cross_tenant_schedules.sql)
// for the same reason ClaimWorkflowsAcrossTenants calls admin.claim_workflows:
// the exemption that lets it see across tenants belongs to that function's
// owner and nowhere else.
//
// No beginTxWithRLS. That helper sets the tenant GUC the policies read, and
// this call must not be scoped to a tenant -- setting one would filter the very
// rows it exists to find.
func (s *PostgresStore) GetDueSchedulesAcrossTenants(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled,
		       next_run_at, last_run_at, timezone, tenant_id, misfire_policy,
		       catch_up_limit, overlap_policy, last_run_id
		FROM admin.get_due_schedules()
	`)
	if err != nil {
		if reason := crossTenantScheduleProvisioningGap(err); reason != "" {
			return nil, fmt.Errorf("get due schedules across tenants: %s: %w", reason, ErrCrossTenantClaimUnsupported)
		}
		return nil, fmt.Errorf("get due schedules across tenants: %w", err)
	}
	defer rows.Close()

	return scanDueSchedules(rows)
}

// crossTenantScheduleProvisioningGap is crossTenantProvisioningGap for 024.
//
// Same two SQLSTATEs and the same reasoning -- see that function -- pointing at
// the migration that provisions this call rather than the claim. Kept separate
// so the remediation an operator is handed names the file they actually need to
// apply; a worker can have 023 and not 024.
func crossTenantScheduleProvisioningGap(err error) string {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return ""
	}
	switch pqErr.Code {
	case "42883":
		return "admin.get_due_schedules does not exist; apply migrations/postgres/024_cross_tenant_schedules.sql"
	case "42501":
		return "this connection may not EXECUTE admin.get_due_schedules; grant it as " +
			"migrations/postgres/024_cross_tenant_schedules.sql documents"
	}
	return ""
}

// scanDueSchedules reads the rows a due-schedule query returns.
//
// Shared by the cross-tenant read and, on PostgreSQL, by the tenant-scoped one
// for the same reason scanClaimedWorkflows is shared: the cross-tenant column
// list lives in a migration, and the only thing keeping it in step with this
// scan is that there is exactly one scan.
func scanDueSchedules(rows *sql.Rows) ([]Schedule, error) {
	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&sch.Input, &sch.Enabled, &sch.NextRunAt, &lastRunAt, &sch.Timezone, &sch.TenantID,
			&sch.MisfirePolicy, &sch.CatchUpLimit, &sch.OverlapPolicy, &sch.LastRunID); err != nil {
			return nil, fmt.Errorf("get due schedules scan: %w", err)
		}
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

// CrossTenantCapability reports whether a store can actually honour the
// cross-tenant paths, and why not when it cannot.
//
// Both fields are answered independently because 023 and 024 are separate
// migrations with separate grants: a deployment can execute workflows for every
// tenant and still fire cron for only one.
type CrossTenantCapability struct {
	Claim           bool
	ClaimReason     string
	Schedules       bool
	SchedulesReason string
}

// CrossTenantCapabilityChecker is implemented by stores that can answer "would
// the cross-tenant paths work" WITHOUT exercising them.
//
// Exercising them is not an option at startup. The claim is a write: running it
// to see whether it works would claim real workflows into a worker whose
// dispatch loop has not started, leaving them 'running' with no executor until
// the lease expires. So each dialect answers from its own catalog instead.
type CrossTenantCapabilityChecker interface {
	CheckCrossTenantCapability(ctx context.Context) CrossTenantCapability
}

// CheckCrossTenantCapability answers from the PostgreSQL catalog.
//
// It checks three things per function, and the third is the reason this exists
// at all:
//
//	does it exist        migration not applied -- would raise 42883 at runtime
//	may this role EXECUTE  grant not made      -- would raise 42501 at runtime
//	does its OWNER have BYPASSRLS
//
// The first two are detected at runtime too, as 42883 and 42501, and answered by
// falling back to the tenant-scoped path with a warning.
//
// The third is different, and it is the reason this check earns its place. A
// function whose owner has lost BYPASSRLS is subject to the same policies as
// its caller, and these functions are called outside beginTxWithRLS so no
// tenant GUC is set on that connection. 001_schema.sql's policies are
// fail-closed -- cleat.assert_tenant_set() RAISES on an unset GUC rather than
// COALESCE-ing to a default -- so every call fails with
//
//	cleat.tenant_id is not set -- tenant context required for RLS-scoped query
//
// That is loud, which is good, and it was worth measuring rather than assuming:
// an earlier version of this comment (and of 023's header) claimed the call
// would quietly return fewer rows instead. It does not, and the difference is
// the fail-closed policy choice rather than luck.
//
// What it is NOT is useful. P0001 names neither the function nor the attribute,
// it does not map to the provisioning-gap sentinel, so it propagates as a hard
// error on every tick, and an operator reading it has no path from "tenant
// context required" to "a role lost a privilege". This check names the cause,
// before the first tick.
//
// 023 and 024 set the attribute in the same migration that creates the role so
// it cannot drift on install, but a role is a database-wide object an operator
// can ALTER afterwards.
func (s *PostgresStore) CheckCrossTenantCapability(ctx context.Context) CrossTenantCapability {
	const q = `
		WITH f AS (
			SELECT to_regprocedure('admin.claim_workflows(text, text[], integer)') AS claim,
			       to_regprocedure('admin.get_due_schedules()')                    AS sched
		)
		SELECT
			f.claim IS NOT NULL,
			CASE WHEN f.claim IS NOT NULL THEN has_function_privilege(f.claim, 'EXECUTE') ELSE false END,
			COALESCE((SELECT r.rolbypassrls FROM pg_proc p JOIN pg_roles r ON r.oid = p.proowner WHERE p.oid = f.claim), false),
			f.sched IS NOT NULL,
			CASE WHEN f.sched IS NOT NULL THEN has_function_privilege(f.sched, 'EXECUTE') ELSE false END,
			COALESCE((SELECT r.rolbypassrls FROM pg_proc p JOIN pg_roles r ON r.oid = p.proowner WHERE p.oid = f.sched), false)
		FROM f
	`
	var claimExists, claimExec, claimBypass, schedExists, schedExec, schedBypass bool
	if err := s.db.QueryRowContext(ctx, q).Scan(
		&claimExists, &claimExec, &claimBypass,
		&schedExists, &schedExec, &schedBypass); err != nil {
		// Inconclusive is not the same as unsupported. Reported as the reason
		// so the caller can say so rather than assert something it did not
		// establish.
		reason := fmt.Sprintf("could not check: %v", err)
		return CrossTenantCapability{ClaimReason: reason, SchedulesReason: reason}
	}

	cap := CrossTenantCapability{}
	cap.Claim, cap.ClaimReason = crossTenantVerdict(claimExists, claimExec, claimBypass,
		"admin.claim_workflows", "migrations/postgres/023_cross_tenant_claim.sql")
	cap.Schedules, cap.SchedulesReason = crossTenantVerdict(schedExists, schedExec, schedBypass,
		"admin.get_due_schedules", "migrations/postgres/024_cross_tenant_schedules.sql")
	return cap
}

func crossTenantVerdict(exists, exec, bypass bool, fn, migration string) (bool, string) {
	switch {
	case !exists:
		return false, fn + " does not exist; apply " + migration
	case !exec:
		return false, "this connection may not EXECUTE " + fn + "; grant it as " + migration + " documents"
	case !bypass:
		// This one does raise at runtime -- measured, see below -- but it raises
		// something that names neither the function nor the attribute, so the
		// operator gets "cleat.tenant_id is not set" and no way to connect it to
		// a role privilege. Naming it here is the whole value.
		return false, fn + " exists and is executable, but its owner does not have BYPASSRLS, so " +
			"it is subject to the same policies as the caller. Every call will fail with " +
			"\"cleat.tenant_id is not set\" (P0001), which names neither this function nor the " +
			"missing attribute. Restore it with: ALTER ROLE cleat_dispatcher BYPASSRLS"
	}
	return true, ""
}

// looksLikeJSONObject reports whether a result is a JSON object.
//
// Structural, not a full parse: coerceResultJSON has already established the
// value is valid JSON, so the first non-space byte decides it. A parse here
// would cost a second decode of every workflow result on the terminal path to
// learn one byte.
func looksLikeJSONObject(result string) bool {
	for i := 0; i < len(result); i++ {
		switch result[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}
