package host

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// MySQL error code helpers
// ---------------------------------------------------------------------------

func isDuplicateKeyError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}

func isDeadlockError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1213
	}
	return false
}

func isLockWaitTimeout(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1205
	}
	return false
}

// ---------------------------------------------------------------------------
// MySQLStore
// ---------------------------------------------------------------------------

// MySQLStore implements WorkflowStore using a MySQL 8.0+ or MariaDB 10.5+
// database. MySQL has no row-level security, so tenant isolation is
// enforced at the database level — each tenant gets its own database
// (cleat_<tenant_id>). The store's connection pool is scoped to the
// tenant's database, making cross-tenant data access impossible at the
// connection level. WHERE tenant_id = ? clauses are retained as
// defense-in-depth.
type MySQLStore struct {
	db         *sql.DB
	taskQueues []string
	tenantID   string
	dialect    Dialect
}

// NewMySQLStore creates a MySQLStore scoped to the given task queues.
// The taskQueues slice specifies which task queues this worker pool should
// poll (e.g., "default", "gpu", "high-memory"). Defaults to ["default"].
// The tenantID defaults to the default tenant UUID.
func NewMySQLStore(db *sql.DB, taskQueues ...string) *MySQLStore {
	tqs := taskQueues
	if len(tqs) == 0 {
		tqs = []string{"default"}
	}
	return &MySQLStore{
		db:         db,
		taskQueues: tqs,
		tenantID:   "00000000-0000-0000-0000-000000000000",
		dialect:    DialectMySQL,
	}
}

// WithTenant returns a copy of the store scoped to the given tenant ID.
// This is used in the dispatch loop to set the correct tenant context
// before executing a workflow. The returned store's methods will add
// WHERE tenant_id = ? to every tenant-scoped query.
func (s *MySQLStore) WithTenant(tenantID string) *MySQLStore {
	cp := *s
	cp.tenantID = tenantID
	return &cp
}

// beginTx starts a new transaction. MySQL has no RLS equivalent, so no
// additional setup is needed -- tenant isolation is handled by explicit
// WHERE tenant_id = ? clauses on every query.
func (s *MySQLStore) beginTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	return tx, nil
}

// inClausePlaceholders returns a comma-separated list of n "?" placeholders
// for use in MySQL IN (...)-clauses.
func inClausePlaceholders(n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = "?"
	}
	return strings.Join(parts, ", ")
}

// taskQueueClause returns the IN-clause SQL fragment and argument slice
// for filtering by the store's configured task queues.
func (s *MySQLStore) taskQueueClause() (string, []interface{}) {
	phs := make([]string, len(s.taskQueues))
	args := make([]interface{}, len(s.taskQueues))
	for i, tq := range s.taskQueues {
		phs[i] = "?"
		args[i] = tq
	}
	return strings.Join(phs, ", "), args
}

// ---------------------------------------------------------------------------
// ClaimWorkflow
// ---------------------------------------------------------------------------

// ClaimWorkflow atomically dequeues a runnable workflow instance.
// Uses SELECT ... FOR UPDATE SKIP LOCKED. Delegates to ClaimWorkflows.
func (s *MySQLStore) ClaimWorkflow(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error) {
	wfs, err := s.ClaimWorkflows(ctx, workerID, namespace, 1)
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
func (s *MySQLStore) ClaimWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqClause, tqArgs := s.taskQueueClause()

	// Step 1: Select IDs with SKIP LOCKED.
	// Arg order: namespace, task_queue values..., tenant_id, worker_id, limit
	selArgs := make([]interface{}, 0)
	selArgs = append(selArgs, namespace)
	selArgs = append(selArgs, tqArgs...)
	selArgs = append(selArgs, s.tenantID, workerID, limit)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM workflow_instances
		WHERE status = 'ready'
		  AND next_wake_at <= NOW(6)
		  AND namespace = ?
		  AND task_queue IN (%s)
		  AND tenant_id = ?
		ORDER BY CASE WHEN sticky_worker_id = ? THEN 0 ELSE 1 END, created_at
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
	idArgs := make([]interface{}, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}

	updateArgs := append([]interface{}{workerID}, idArgs...)
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = ?,
		    heartbeat_at = NOW(6)
		WHERE id IN (%s)
	`, idClause), updateArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: update: %w", err)
	}

	// Step 3: Fetch the full rows.
	rows2, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, def_name, def_version, status, input, COALESCE(assigned_to, ''), next_wake_at, tenant_id, created_at, error_code, error_op
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

	return wfs, tx.Commit()
}

// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
// that are sticky to this worker. Uses SELECT ... FOR UPDATE SKIP LOCKED with
// sticky_worker_id filtering for low-contention claiming.
func (s *MySQLStore) ClaimStickyWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqClause, tqArgs := s.taskQueueClause()

	// Step 1: Select IDs with SKIP LOCKED (sticky filter).
	// Arg order: sticky_worker_id, namespace, task_queue values..., tenant_id, limit
	selArgs := make([]interface{}, 0)
	selArgs = append(selArgs, workerID, namespace)
	selArgs = append(selArgs, tqArgs...)
	selArgs = append(selArgs, s.tenantID, limit)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM workflow_instances
		WHERE status = 'ready'
		  AND next_wake_at <= NOW(6)
		  AND sticky_worker_id = ?
		  AND namespace = ?
		  AND task_queue IN (%s)
		  AND tenant_id = ?
		ORDER BY created_at
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
	idArgs := make([]interface{}, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}

	updateArgs := append([]interface{}{workerID}, idArgs...)
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = ?,
		    heartbeat_at = NOW(6)
		WHERE id IN (%s)
	`, idClause), updateArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: update: %w", err)
	}

	// Step 3: Fetch the full rows.
	rows2, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, def_name, def_version, status, input, COALESCE(assigned_to, ''), next_wake_at, tenant_id, created_at, error_code, error_op
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

	return wfs, tx.Commit()
}

// ---------------------------------------------------------------------------
// CompleteWorkflow, FailWorkflow, ReleaseWorkflow
// ---------------------------------------------------------------------------

// CompleteWorkflow marks a workflow as completed with a result.
func (s *MySQLStore) CompleteWorkflow(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error {
	tx, err := s.beginTx(ctx)
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
		SET status = 'done', result = ?, completed_at = NOW(6), assigned_to = NULL, query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ?
	`, result, qsJSON, workflowID, workerID, s.tenantID)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: record result in idempotency_keys if this workflow was started with a key.
	s.db.ExecContext(ctx,
		`UPDATE idempotency_keys SET result = ? WHERE workflow_id = ?`,
		result, workflowID)

	// Best-effort: clear sticky worker assignment.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.
func (s *MySQLStore) FailWorkflow(ctx context.Context, workflowID, workerID, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTx(ctx)
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
		    error_msg = ?,
		    error_code = ?,
		    error_op = ?,
		    completed_at = NOW(6),
		    assigned_to = NULL,
		    query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ?
	`, errorMsg, errorCode, errorOp, qsJSON, workflowID, workerID, s.tenantID)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: record error in idempotency_keys if this workflow was started with a key.
	s.db.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = ? WHERE workflow_id = ?`,
		errorMsg, workflowID)

	// Best-effort: clear sticky worker assignment.
	s.ClearStickyWorker(context.Background(), workflowID)
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// ReleaseWorkflow returns a workflow to the ready queue with a next wake time.
// Used when a workflow suspends (sleep/await signals).
func (s *MySQLStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("release workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ?
	`, nextWakeAt, workflowID, workerID, s.tenantID)
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
func (s *MySQLStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
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
		if err != sql.ErrNoRows {
			return "", false, err
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return "", false, err
		}
		defer tx.Rollback()

		// Insert idempotency key record. INSERT IGNORE handles the race where
		// two requests arrive with the same key simultaneously.
		res, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO idempotency_keys (key_hash, workflow_id, expires_at)
			 VALUES (?, ?, DATE_ADD(NOW(6), INTERVAL 7 DAY))`,
			keyHash[:], runID)
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
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, namespace, task_queue, tenant_id)
			VALUES (?, ?, ?, 'ready', ?,
			        COALESCE((SELECT namespace FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
			        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
			        ?)
		`, runID, defName, defVersion, input, defName, defVersion, defName, defVersion, s.tenantID)
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
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, namespace, task_queue, tenant_id)
		VALUES (?, ?, ?, 'ready', ?,
		        COALESCE((SELECT namespace FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
		        ?)
	`, runID, defName, defVersion, input, defName, defVersion, defName, defVersion, s.tenantID)
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
func (s *MySQLStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string) (string, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return "", fmt.Errorf("continue as new: begin: %w", err)
	}
	defer tx.Rollback()

	// Append events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, currentRunID, newEvents); err != nil {
		return "", fmt.Errorf("continue as new: append events: %w", err)
	}

	// Create the new workflow run.
	newRunID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, namespace, task_queue, tenant_id)
		VALUES (?, ?, ?, 'ready', ?,
		        COALESCE((SELECT namespace FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ?), 'default'),
		        ?)
	`, newRunID, defName, defVersion, newInput, defName, defVersion, defName, defVersion, s.tenantID)
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
		SET status = 'done', result = ?, completed_at = NOW(6), assigned_to = NULL, query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ?
	`, result, qsJSON, currentRunID, workerID, s.tenantID)
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

// ---------------------------------------------------------------------------
// FinalizeWorkflowSegment
// ---------------------------------------------------------------------------

// FinalizeWorkflowSegment atomically appends new events and updates the
// workflow status in a single database transaction. finalStatus must be one
// of "done", "failed" or "ready" (suspend). Fields not relevant to the chosen
// status are ignored.
func (s *MySQLStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize workflow: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Append new events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize workflow: append events: %w", err)
	}

	// Update workflow status based on finalStatus.
	switch finalStatus {
	case "done":
		qsJSON, _ := json.Marshal(queryState)
		if qsJSON == nil {
			qsJSON = []byte("{}")
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'done', result = ?, completed_at = NOW(6), assigned_to = NULL, query_state = ?
			WHERE id = ? AND assigned_to = ? AND tenant_id = ?
		`, result, qsJSON, runID, workerID, s.tenantID)
	case "failed":
		qsJSON, _ := json.Marshal(queryState)
		if qsJSON == nil {
			qsJSON = []byte("{}")
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'failed',
			    error_msg = ?,
			    error_code = ?,
			    error_op = ?,
			    completed_at = NOW(6),
			    assigned_to = NULL,
			    query_state = ?
			WHERE id = ? AND assigned_to = ? AND tenant_id = ?
		`, result, errorCode, errorOp, qsJSON, runID, workerID, s.tenantID)
	case "ready":
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'ready', assigned_to = NULL, next_wake_at = ?
			WHERE id = ? AND assigned_to = ? AND tenant_id = ?
		`, nextWakeAt, runID, workerID, s.tenantID)
	default:
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}
	if err != nil {
		return fmt.Errorf("finalize workflow: update status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup for terminal statuses.
	if finalStatus == "done" || finalStatus == "failed" {
		switch finalStatus {
		case "done":
			s.db.ExecContext(ctx,
				`UPDATE idempotency_keys SET result = ? WHERE workflow_id = ?`,
				runID, result)
		case "failed":
			s.db.ExecContext(ctx,
				`UPDATE idempotency_keys SET error_msg = ? WHERE workflow_id = ?`,
				runID, result)
		}
		s.ClearStickyWorker(context.Background(), runID)
		s.ReleaseWorkflowConcurrencyKeys(context.Background(), runID)
		s.enforceParentClosePolicy(context.Background(), runID)
	}

	return nil
}

// ---------------------------------------------------------------------------
// AppendEventHistory / AppendEventHistoryBatch
// ---------------------------------------------------------------------------

// AppendEventHistory appends a single event to the history.
func (s *MySQLStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	return s.AppendEventHistoryBatch(ctx, workflowID, []EventRecord{rec})
}

// AppendEventHistoryBatch appends multiple events atomically.
func (s *MySQLStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append history batch: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.appendEventsInTx(ctx, tx, workflowID, recs); err != nil {
		return err
	}
	return tx.Commit()
}

// appendEventsInTx inserts event records inside an already-open transaction.
// This is shared by AppendEventHistoryBatch and FinalizeWorkflowSegment so
// that both can insert events atomically alongside other operations.
func (s *MySQLStore) appendEventsInTx(ctx context.Context, tx *sql.Tx, workflowID string, recs []EventRecord) error {
	if len(recs) == 0 {
		return nil
	}

	// MySQL uses INSERT IGNORE instead of ON CONFLICT DO NOTHING.
	// The unique key on (workflow_id, step) prevents duplicates.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT IGNORE INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
			duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
			defer_description, defer_id, child_name, child_input, run_id, new_input,
			plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
			promise_name, promise_id, promise_result, promise_error, payload,
			created_at, checksum, tenant_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("append events in tx: prepare: %w", err)
	}
	defer stmt.Close()

	for _, rec := range recs {
		payload, err := eventRecordToPayload(rec)
		payloadArg := nullStr("")
		if err == nil && len(payload) > 0 {
			payloadArg = sql.NullString{String: string(payload), Valid: true}
		}
		checksum := computeEventChecksum(rec)
		_, err = stmt.ExecContext(ctx, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op),
			nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Request))),
			nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Response))),
			nullStr(rec.Err),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(rec.SignalPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(rec.PluginInput), nullStr(rec.PluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(rec.PromiseResult), nullStr(rec.PromiseError),
			payloadArg,
			time.UnixMilli(rec.TimestampMs),
			checksum,
			s.tenantID)
		if err != nil {
			return fmt.Errorf("append events in tx: exec step %d: %w", rec.Step, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Heartbeat / BatchHeartbeat
// ---------------------------------------------------------------------------

// Heartbeat updates the heartbeat timestamp to prevent timeout.
// Returns false if the workflow is no longer assigned to this worker.
func (s *MySQLStore) Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = NOW(6)
		WHERE id = ? AND assigned_to = ? AND tenant_id = ?
	`, workflowID, workerID, s.tenantID)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// BatchHeartbeat updates heartbeat_at for all workflows assigned to this
// worker with status 'running'. Uses a single UPDATE instead of N calls.
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
func (s *MySQLStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID, errMsg, errorCode, errorOp string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = ?, error_code = ?, error_op = ?,
		    completed_at = NOW(6), assigned_to = NULL
		WHERE id = ? AND assigned_to = ? AND tenant_id = ?
	`, errMsg, errorCode, errorOp, workflowID, workerID, s.tenantID)
	if err != nil {
		return err
	}

	// Best-effort: record error in idempotency_keys if this workflow was started with a key.
	s.db.ExecContext(context.Background(),
		`UPDATE idempotency_keys SET error_msg = ? WHERE workflow_id = ?`,
		errMsg, workflowID)

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
// RequestCancellation / CheckCancellation
// ---------------------------------------------------------------------------

// RequestCancellation sets the cancellation flag on a workflow.
func (s *MySQLStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("request cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true, cancellation_reason = ?
		WHERE id = ? AND tenant_id = ?
	`, reason, workflowID, s.tenantID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CheckCancellation checks if a workflow has been cancelled.
func (s *MySQLStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	var cancelled bool
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, workflowID, s.tenantID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, nil
}

// ---------------------------------------------------------------------------
// StartChildWorkflow / GetChildResult
// ---------------------------------------------------------------------------

// StartChildWorkflow creates a child workflow instance linked to a parent.
// defVersion is the explicit workflow definition version to use, or 0 to use
// default resolution (SELECT MAX(version)).
func (s *MySQLStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string) (string, error) {
	runID := uuid.New().String()

	var err error
	if defVersion > 0 {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, namespace, task_queue, tenant_id)
			VALUES (?, ?, ?, 'ready', ?, ?,
			        COALESCE(NULLIF(?, ''), 'ABANDON'),
			        COALESCE((SELECT namespace FROM (SELECT namespace FROM workflow_instances WHERE id = ?) AS subq), 'default'),
			        COALESCE((SELECT task_queue FROM (SELECT task_queue FROM workflow_instances WHERE id = ?) AS subq), 'default'),
			        ?)
		`, runID, defName, defVersion, inputJSON, parentID, parentClosePolicy, parentID, parentID, s.tenantID)
	} else {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, namespace, task_queue, tenant_id)
			VALUES (?, ?, (SELECT COALESCE(MAX(version), 0) FROM workflow_defs WHERE name = ? AND NOT deprecated), 'ready', ?, ?,
			        COALESCE(NULLIF(?, ''), 'ABANDON'),
			        COALESCE((SELECT namespace FROM (SELECT namespace FROM workflow_instances WHERE id = ?) AS subq), 'default'),
			        COALESCE((SELECT task_queue FROM (SELECT task_queue FROM workflow_instances WHERE id = ?) AS subq), 'default'),
			        ?)
		`, runID, defName, defName, inputJSON, parentID, parentClosePolicy, parentID, parentID, s.tenantID)
	}
	if err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}
	return runID, nil
}

// StartChildWorkflowAtomic creates a child workflow and records the parent's
// child_workflow event in a single transaction, guaranteeing exactly-once creation.
func (s *MySQLStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord) (string, error) {
	if childID == "" {
		childID = uuid.New().String()
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. INSERT child workflow instance.
	if defVersion > 0 {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, namespace, task_queue, tenant_id)
			VALUES (?, ?, ?, 'ready', ?, ?,
			        COALESCE(NULLIF(?, ''), 'ABANDON'),
			        COALESCE((SELECT namespace FROM (SELECT namespace FROM workflow_instances WHERE id = ?) AS subq), 'default'),
			        COALESCE((SELECT task_queue FROM (SELECT task_queue FROM workflow_instances WHERE id = ?) AS subq), 'default'),
			        ?)
		`, childID, defName, defVersion, inputJSON, parentID, parentClosePolicy, parentID, parentID, s.tenantID)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, namespace, task_queue, tenant_id)
			VALUES (?, ?, (SELECT COALESCE(MAX(version), 0) FROM workflow_defs WHERE name = ? AND NOT deprecated), 'ready', ?, ?,
			        COALESCE(NULLIF(?, ''), 'ABANDON'),
			        COALESCE((SELECT namespace FROM (SELECT namespace FROM workflow_instances WHERE id = ?) AS subq), 'default'),
			        COALESCE((SELECT task_queue FROM (SELECT task_queue FROM workflow_instances WHERE id = ?) AS subq), 'default'),
			        ?)
		`, childID, defName, defName, inputJSON, parentID, parentClosePolicy, parentID, parentID, s.tenantID)
	}
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert child: %w", err)
	}

	// 2. INSERT IGNORE child_workflow event into parent's event_history.
	event.RunID = childID
	checksum := computeEventChecksum(event)
	_, err = tx.ExecContext(ctx, `
		INSERT IGNORE INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum, tenant_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum, s.tenantID)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow atomic: commit: %w", err)
	}
	return childID, nil
}

// GetChildResult checks whether a child workflow has completed and returns its result.
func (s *MySQLStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	var result string
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(result, '{}'), status FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, runID, s.tenantID).Scan(&result, &status)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result: %w", err)
	}
	if status == "done" || status == "failed" {
		return compactJSONString(result), true, nil
	}
	return "", false, nil
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
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
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

// ---------------------------------------------------------------------------
// GetQueryState
// ---------------------------------------------------------------------------

// GetQueryState returns the query state for a workflow instance key.
func (s *MySQLStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	var value sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT JSON_UNQUOTE(JSON_EXTRACT(query_state, ?)) FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, "$."+key, workflowID, s.tenantID).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get query state: %w", err)
	}
	return value.String, nil
}

// ---------------------------------------------------------------------------
// LoadEventHistory
// ---------------------------------------------------------------------------

// LoadEventHistory returns the full event history for a workflow, ordered by step.
func (s *MySQLStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error,
		       CAST(UNIX_TIMESTAMP(created_at) * 1000 AS UNSIGNED) AS timestamp_ms
		FROM event_history
		WHERE workflow_id = ? AND tenant_id = ?
		ORDER BY step
	`, workflowID, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	defer rows.Close()

	var history []EventRecord
	for rows.Next() {
		var rec EventRecord
		var service, op, request, response, errMsg sql.NullString
		var durationMs, timeoutMs sql.NullInt64
		var signalNames, signalName, signalPayload sql.NullString
		var deferDesc, deferID sql.NullString
		var childName, childInput, runID, newInput sql.NullString
		var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
		var payload sql.NullString
		var promiseName, promiseID, promiseResult, promiseError sql.NullString

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError,
			&rec.TimestampMs); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = tryDecodeBase64(request.String)
		rec.Response = tryDecodeBase64(response.String)
		rec.Err = errMsg.String
		rec.DurationMs = durationMs.Int64
		rec.SignalNames = signalNames.String
		rec.TimeoutMs = timeoutMs.Int64
		rec.SignalName = signalName.String
		rec.SignalPayload = signalPayload.String
		rec.DeferDescription = deferDesc.String
		rec.DeferID = deferID.String
		rec.ChildName = childName.String
		rec.ChildInput = childInput.String
		rec.RunID = runID.String
		rec.NewInput = newInput.String
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginInput = pluginInput.String
		rec.PluginOutput = pluginOutput.String
		rec.PluginError = pluginErr.String
		rec.PromiseName = promiseName.String
		rec.PromiseID = promiseID.String
		rec.PromiseResult = promiseResult.String
		rec.PromiseError = promiseError.String

		if payload.Valid {
			populateFromPayload(&rec, []byte(payload.String))
		}

		history = append(history, rec)
	}
	return history, rows.Err()
}

// LoadEventHistoryPaginated returns a page of event history for a workflow,
// with offset and limit support. Defaults limit to 1000 if limit <= 0, capped at 1000.
func (s *MySQLStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error
		FROM event_history
		WHERE workflow_id = ? AND tenant_id = ?
		ORDER BY step
		LIMIT ? OFFSET ?
	`, workflowID, s.tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load history paginated: %w", err)
	}
	defer rows.Close()

	var history []EventRecord
	for rows.Next() {
		var rec EventRecord
		var service, op, request, response, errMsg sql.NullString
		var durationMs, timeoutMs sql.NullInt64
		var signalNames, signalName, signalPayload sql.NullString
		var deferDesc, deferID sql.NullString
		var childName, childInput, runID, newInput sql.NullString
		var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
		var payload sql.NullString
		var promiseName, promiseID, promiseResult, promiseError sql.NullString

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError); err != nil {
			return nil, fmt.Errorf("scan history paginated: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = tryDecodeBase64(request.String)
		rec.Response = tryDecodeBase64(response.String)
		rec.Err = errMsg.String
		rec.DurationMs = durationMs.Int64
		rec.SignalNames = signalNames.String
		rec.TimeoutMs = timeoutMs.Int64
		rec.SignalName = signalName.String
		rec.SignalPayload = signalPayload.String
		rec.DeferDescription = deferDesc.String
		rec.DeferID = deferID.String
		rec.ChildName = childName.String
		rec.ChildInput = childInput.String
		rec.RunID = runID.String
		rec.NewInput = newInput.String
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginInput = pluginInput.String
		rec.PluginOutput = pluginOutput.String
		rec.PluginError = pluginErr.String
		rec.PromiseName = promiseName.String
		rec.PromiseID = promiseID.String
		rec.PromiseResult = promiseResult.String
		rec.PromiseError = promiseError.String

		if payload.Valid {
			populateFromPayload(&rec, []byte(payload.String))
		}

		history = append(history, rec)
	}
	return history, rows.Err()
}

// CountEventHistory returns the total number of events for a workflow.
func (s *MySQLStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_history WHERE workflow_id = ? AND tenant_id = ?`, workflowID, s.tenantID).Scan(&count)
	return count, err
}

// ---------------------------------------------------------------------------
// VerifyWorkflowEvents
// ---------------------------------------------------------------------------

// VerifyWorkflowEvents loads all events for a workflow and verifies their
// integrity by recomputing SHA-256 checksums and comparing them against the
// stored checksums. Before the checksum column migration, it loads and
// computes checksums silently and returns nil.
func (s *MySQLStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error {
	// Load the full event history for the workflow.
	events, err := s.LoadEventHistory(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("verify events: load: %w", err)
	}

	// Try to load stored checksums from the DB. If the column doesn't exist
	// (pre-migration), this query will fail, and we skip verification.
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, checksum FROM event_history
		WHERE workflow_id = ? AND tenant_id = ?
		ORDER BY step
	`, workflowID, s.tenantID)
	if err != nil {
		// Column does not exist yet -- skip verification (pre-migration).
		return nil
	}
	defer rows.Close()

	storedChecksums := make(map[int]string)
	for rows.Next() {
		var step int
		var checksum sql.NullString
		if err := rows.Scan(&step, &checksum); err != nil {
			return fmt.Errorf("verify events: scan checksum: %w", err)
		}
		if checksum.Valid && checksum.String != "" {
			storedChecksums[step] = checksum.String
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify events: rows: %w", err)
	}

	// If no checksums are stored, verification is not possible yet.
	if len(storedChecksums) == 0 {
		return nil
	}

	// Recompute and compare checksums.
	for _, ev := range events {
		expected, ok := storedChecksums[ev.Step]
		if !ok || expected == "" {
			continue // No stored checksum for this step (pre-migration partial data).
		}
		actual := computeEventChecksum(ev)
		if actual != expected {
			return fmt.Errorf("verify events: workflow %s step %d: checksum mismatch (expected %s, got %s)",
				workflowID, ev.Step, expected, actual)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// DeliverSignal / PollSignal / PollCancellation / PollAndClaimSignal
// ---------------------------------------------------------------------------

// DeliverSignal stores a signal for a workflow. Uses ON DUPLICATE KEY UPDATE
// so that re-delivering the same signal name replaces the payload.
func (s *MySQLStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("deliver signal: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE payload = VALUES(payload), delivered_at = NOW(6)
	`, workflowID, signalName, payload, s.tenantID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// PollSignal checks for a delivered signal without consuming it.
// This is non-destructive — the signal remains available after polling.
func (s *MySQLStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `
		SELECT payload FROM workflow_signals
		WHERE workflow_id = ? AND signal_name = ? AND tenant_id = ?
	`, workflowID, signalName, s.tenantID).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("poll signal: %w", err)
	}
	return payload, true, nil
}

// PollCancellation checks whether the workflow has been cancelled.
func (s *MySQLStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return s.CheckCancellation(ctx, workflowID)
}

// PollAndClaimSignal atomically checks for and claims a pending signal.
// Uses SELECT ... FOR UPDATE followed by DELETE in a transaction to emulate
// PostgreSQL's DELETE ... RETURNING.
func (s *MySQLStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	// Step 1: SELECT ... FOR UPDATE to lock the row.
	var payload string
	err = tx.QueryRowContext(ctx, `
		SELECT payload FROM workflow_signals
		WHERE workflow_id = ? AND signal_name = ? AND tenant_id = ?
		FOR UPDATE
	`, workflowID, signalName, s.tenantID).Scan(&payload)
	if err == sql.ErrNoRows {
		tx.Rollback()
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("poll and claim signal: select: %w", err)
	}

	// Step 2: DELETE the claimed row.
	_, err = tx.ExecContext(ctx, `
		DELETE FROM workflow_signals
		WHERE workflow_id = ? AND signal_name = ? AND tenant_id = ?
	`, workflowID, signalName, s.tenantID)
	if err != nil {
		return "", false, fmt.Errorf("poll and claim signal: delete: %w", err)
	}

	return payload, true, tx.Commit()
}

// ---------------------------------------------------------------------------
// UpdateStickyWorker / ClearStickyWorker
// ---------------------------------------------------------------------------

// UpdateStickyWorker sets the sticky worker for a workflow.
func (s *MySQLStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = ? WHERE id = ?
	`, workerID, workflowID)
	if err != nil {
		return fmt.Errorf("update sticky worker: %w", err)
	}
	return nil
}

// ClearStickyWorker removes the sticky worker assignment.
func (s *MySQLStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = NULL WHERE id = ?
	`, workflowID)
	if err != nil {
		return fmt.Errorf("clear sticky worker: %w", err)
	}
	return nil
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

// ---- ReleaseWorkflowConcurrencyKeys ----

// ReleaseWorkflowConcurrencyKeys releases all concurrency keys held by a workflow.
// Runs as a best-effort operation.
func (s *MySQLStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE workflow_id = ?`, workflowID)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: %w", err)
	}
	return nil
}

// ResolveTenantFromAPIKey looks up a tenant UUID by API key hash.
func (s *MySQLStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id FROM tenant_api_keys
		 WHERE key_hash = ? AND revoked_at IS NULL`, keyHash).Scan(&tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// ---------------------------------------------------------------------------
// MySQLStoreFactory
// ---------------------------------------------------------------------------

// MySQLStoreFactory implements StoreFactory for MySQL/MariaDB with
// per-tenant database isolation. Each tenant gets its own MySQL database
// (cleat_<tenant_id>), and the connection pool is scoped to that database.
type MySQLStoreFactory struct {
	mu sync.RWMutex

	// masterDB is connected without a default database — used for
	// CREATE DATABASE and other administrative operations.
	masterDB *sql.DB

	// baseDSN is a DSN template used to open per-tenant connections.
	// It should include the base connection parameters (user, password,
	// host, port, params) but NOT the database name. The database name
	// is appended per tenant.
	// Example: "user:pass@tcp(localhost:3306)/?parseTime=true&multiStatements=true"
	baseDSN string

	// tenantDBs maps tenantID -> per-tenant connection pool.
	tenantDBs map[string]*sql.DB
}

// NewMySQLStoreFactory creates a MySQLStoreFactory.
// masterDB is a *sql.DB connected without a default database (used for
// administrative operations like CREATE DATABASE). baseDSN is a DSN template
// with connection parameters but without a database name — the per-tenant
// database name is appended for each tenant's connection.
func NewMySQLStoreFactory(masterDB *sql.DB, baseDSN string) *MySQLStoreFactory {
	return &MySQLStoreFactory{
		masterDB:  masterDB,
		baseDSN:   baseDSN,
		tenantDBs: make(map[string]*sql.DB),
	}
}

// buildTenantDSN inserts the database name into the base DSN.
// The baseDSN has connection parameters without a database name, e.g.
// "user:pass@tcp(host:port)/?parseTime=true". The database name is
// inserted after the last '/' and before any '?' query parameters.
func (f *MySQLStoreFactory) buildTenantDSN(dbName string) string {
	base := f.baseDSN
	slash := strings.LastIndex(base, "/")
	if slash < 0 {
		return base + dbName
	}
	return base[:slash+1] + dbName + base[slash+1:]
}

// CreateTenantDatabase creates a new database for the given tenant and
// returns a connection pool scoped to that database. It is idempotent —
// if the database already exists, it just opens a new pool to it.
func (f *MySQLStoreFactory) CreateTenantDatabase(ctx context.Context, tenantID string) (*sql.DB, error) {
	// tenantID must be a valid UUID to prevent SQL injection through
	// backtick-quoted identifiers.
	if _, err := uuid.Parse(tenantID); err != nil {
		return nil, fmt.Errorf("invalid tenant ID %q: %w", tenantID, err)
	}
	// Replace hyphens with underscores for use as a database name suffix.
	dbName := "cleat_" + strings.ReplaceAll(tenantID, "-", "_")

	f.mu.Lock()
	defer f.mu.Unlock()

	// Check if we already have a pool for this tenant.
	if existing, ok := f.tenantDBs[tenantID]; ok {
		return existing, nil
	}

	// Create the database via the master connection.
	_, err := f.masterDB.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+dbName+"`")
	if err != nil {
		return nil, fmt.Errorf("create tenant database %s: %w", dbName, err)
	}

	// Open a new connection pool scoped to this tenant's database.
	tenantDSN := f.buildTenantDSN(dbName)
	tenantDB, err := sql.Open("mysql", tenantDSN)
	if err != nil {
		return nil, fmt.Errorf("open tenant pool %s: %w", dbName, err)
	}

	// Configure the pool.
	tenantDB.SetMaxOpenConns(15)
	tenantDB.SetMaxIdleConns(5)
	tenantDB.SetConnMaxLifetime(5 * time.Minute)

	// Verify connectivity.
	if err := tenantDB.PingContext(ctx); err != nil {
		tenantDB.Close()
		return nil, fmt.Errorf("ping tenant db %s: %w", dbName, err)
	}

	f.tenantDBs[tenantID] = tenantDB
	return tenantDB, nil
}

// DropTenantDatabase removes a tenant database and closes its connection pool.
func (f *MySQLStoreFactory) DropTenantDatabase(tenantID string) error {
	dbName := "cleat_" + strings.ReplaceAll(tenantID, "-", "_")

	f.mu.Lock()
	defer f.mu.Unlock()

	if db, ok := f.tenantDBs[tenantID]; ok {
		db.Close()
		delete(f.tenantDBs, tenantID)
	}

	_, err := f.masterDB.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
	return err
}

// getOrCreateTenantDB returns the connection pool for a tenant,
// creating the tenant database if needed.
func (f *MySQLStoreFactory) getOrCreateTenantDB(ctx context.Context, tenantID string) (*sql.DB, error) {
	f.mu.RLock()
	db, ok := f.tenantDBs[tenantID]
	f.mu.RUnlock()
	if ok {
		return db, nil
	}
	return f.CreateTenantDatabase(ctx, tenantID)
}

// OpenStore creates a MySQLStore scoped to the given tenant and task queues.
// The store's connection pool is scoped to the tenant's database.
func (f *MySQLStoreFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error) {
	tenantDB, err := f.getOrCreateTenantDB(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("open store for tenant %s: %w", tenantID, err)
	}

	store := NewMySQLStore(tenantDB, taskQueues...)
	store.tenantID = tenantID
	return store, nopCloser{}, nil
}

// Close closes all tenant connection pools.
func (f *MySQLStoreFactory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for tenantID, db := range f.tenantDBs {
		db.Close()
		delete(f.tenantDBs, tenantID)
	}
	return nil
}

// DriverName returns "mysql".
func (f *MySQLStoreFactory) DriverName() string { return "mysql" }

// Dialect returns DialectMySQL.
func (f *MySQLStoreFactory) Dialect() Dialect { return DialectMySQL }
