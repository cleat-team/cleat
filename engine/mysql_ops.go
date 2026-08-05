package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"
)

// compactJSONString removes any extra whitespace (spaces after colons and commas)
// that MySQL adds when casting JSON columns to CHAR/VARCHAR. This ensures
// consistent JSON string comparison in tests and callers.
func compactJSONString(s string) string {
	if s == "" {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return s // fallback to original if invalid JSON
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// Promises
// ---------------------------------------------------------------------------

// CreatePromise creates a new promise for a workflow.
func (s *MySQLStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id, status)
		VALUES (?, ?, ?, ?, 'pending')
	`, workflowID, promiseID, promiseName, s.tenantID)
	return err
}

// ResolvePromise marks a promise as resolved with the given result.
// Also wakes the workflow instance so it can pick up the resolved promise
// on the next poll cycle instead of waiting for the original timeout.
func (s *MySQLStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_promises SET status = ?, result = ?, resolved_at = NOW(6)
		WHERE workflow_id = ? AND promise_id = ? AND tenant_id = ?
	`, "resolved", result, workflowID, promiseID, s.tenantID)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = NOW(6)
		WHERE id = ? AND status = 'ready' AND tenant_id = ?
	`, workflowID, s.tenantID)
	return nil
}

// RejectPromise marks a promise as rejected with the given error message.
// Also wakes the workflow instance so it can pick up the rejected promise
// on the next poll cycle instead of waiting for the original timeout.
func (s *MySQLStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_promises SET status = ?, error_msg = ?, resolved_at = NOW(6)
		WHERE workflow_id = ? AND promise_id = ? AND tenant_id = ?
	`, "rejected", errMsg, workflowID, promiseID, s.tenantID)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = NOW(6)
		WHERE id = ? AND status = 'ready' AND tenant_id = ?
	`, workflowID, s.tenantID)
	return nil
}

// GetPromise returns the current status and result of a promise.
func (s *MySQLStore) GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error) {
	var resultStr, errStr sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT status, CAST(result AS CHAR), error_msg FROM workflow_promises
		WHERE workflow_id = ? AND promise_id = ? AND tenant_id = ?
	`, workflowID, promiseID, s.tenantID).Scan(&status, &resultStr, &errStr)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", "", "", nil
	}
	if err != nil {
		return "", "", "", err
	}
	return status, compactJSONString(resultStr.String), errStr.String, nil
}

// ListPromises returns all promises for a workflow ordered by creation time.
func (s *MySQLStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT promise_id, promise_name, status,
		       COALESCE(CAST(result AS CHAR), ''),
		       COALESCE(error_msg, ''),
		       created_at, resolved_at
		FROM workflow_promises
		WHERE workflow_id = ? AND tenant_id = ?
		ORDER BY created_at
	`, workflowID, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var promises []PromiseInfo
	for rows.Next() {
		var pi PromiseInfo
		var resolvedAt sql.NullTime
		if err := rows.Scan(&pi.PromiseID, &pi.PromiseName, &pi.Status,
			&pi.Result, &pi.ErrorMsg, &pi.CreatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		pi.Result = compactJSONString(pi.Result)
		if resolvedAt.Valid {
			pi.ResolvedAt = &resolvedAt.Time
		}
		promises = append(promises, pi)
	}
	return promises, rows.Err()
}

// ---------------------------------------------------------------------------
// Update Requests
// ---------------------------------------------------------------------------

// CreateUpdateRequest registers an incoming update request for a workflow.
func (s *MySQLStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO workflow_update_requests (workflow_id, update_name, payload, promise_id, status)
		VALUES (?, ?, ?, ?, 'pending')
	`, workflowID, updateName, payload, promiseID)
	return err
}

// GetPendingUpdateRequests returns all pending (not yet dispatched) update requests.
func (s *MySQLStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workflow_id, update_name, CAST(payload AS CHAR),
		       COALESCE(promise_id, ''),
		       status,
		       COALESCE(CAST(result AS CHAR), ''),
		       COALESCE(error_msg, ''),
		       created_at
		FROM workflow_update_requests
		WHERE workflow_id = ? AND status = 'pending'
		ORDER BY created_at
	`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []UpdateRequestInfo
	for rows.Next() {
		var r UpdateRequestInfo
		if err := rows.Scan(&r.WorkflowID, &r.UpdateName, &r.Payload,
			&r.PromiseID, &r.Status, &r.Result, &r.ErrorMsg, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Payload = compactJSONString(r.Payload)
		r.Result = compactJSONString(r.Result)
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

// CompleteUpdateRequest marks an update request as completed with a result or error.
func (s *MySQLStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_update_requests
		SET status = 'completed', result = ?, error_msg = ?, completed_at = NOW(6)
		WHERE workflow_id = ? AND update_name = ? AND status = 'pending'
	`, result, errMsg, workflowID, updateName)
	return err
}

// ---------------------------------------------------------------------------
// Concurrency Keys
// ---------------------------------------------------------------------------

// AcquireConcurrencyKey tries to acquire a concurrency key for a workflow.
// Returns true if acquired, false if already held by another workflow.
func (s *MySQLStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	hash := sha256.Sum256([]byte(key))
	keyHash := hash[:]
	expiration := time.Now().Add(ttl)

	// Step 1: delete any expired key for this hash (tenant-scoped).
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM concurrency_keys WHERE key_hash = ? AND expires_at <= NOW(6) AND tenant_id = ?
	`, keyHash, s.tenantID)
	if err != nil {
		return false, fmt.Errorf("AcquireConcurrencyKey: cleanup expired: %w", err)
	}

	// Step 2: try to insert. If the key_hash already exists (held by another
	// workflow with a still-valid expiry), INSERT IGNORE is a silent no-op.
	_, err = s.db.ExecContext(ctx, `
		INSERT IGNORE INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		VALUES (?, ?, ?, ?, ?)
	`, keyHash, key, workflowID, expiration, s.tenantID)
	if err != nil {
		return false, fmt.Errorf("AcquireConcurrencyKey: %w", err)
	}

	// Step 3: check who owns the key now (tenant-scoped).
	var ownerID string
	err = s.db.QueryRowContext(ctx, `
		SELECT workflow_id FROM concurrency_keys WHERE key_hash = ? AND tenant_id = ?
	`, keyHash, s.tenantID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("AcquireConcurrencyKey: verify: %w", err)
	}

	return ownerID == workflowID, nil
}

// ReleaseConcurrencyKey releases a specific concurrency key.
func (s *MySQLStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	hash := sha256.Sum256([]byte(key))
	keyHash := hash[:]
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM concurrency_keys WHERE key_hash = ? AND tenant_id = ?
	`, keyHash, s.tenantID)
	if err != nil {
		return fmt.Errorf("ReleaseConcurrencyKey: %w", err)
	}
	return nil
}

// ReapExpiredConcurrencyKeys deletes all expired concurrency keys
// for the current tenant. Returns the number of keys deleted.
func (s *MySQLStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM concurrency_keys WHERE expires_at < NOW(6) AND tenant_id = ?
	`, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("ReapExpiredConcurrencyKeys: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// ---------------------------------------------------------------------------
// Schedules
// ---------------------------------------------------------------------------

// CreateSchedule inserts a new cron schedule.
func (s *MySQLStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, enabled, next_run_at, tenant_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, sch.Name, sch.DefName, sch.EntryPoint, sch.CronExpression, sch.Input, sch.Enabled, sch.NextRunAt, s.tenantID)
	if err != nil {
		return fmt.Errorf("CreateSchedule: %w", err)
	}
	return nil
}

// ListSchedules returns all registered schedules for the current tenant.
func (s *MySQLStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at
		FROM workflow_schedules
		WHERE tenant_id = ?
		ORDER BY name
	`, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("ListSchedules: %w", err)
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&sch.Input, &sch.Enabled, &sch.NextRunAt, &lastRunAt); err != nil {
			return nil, fmt.Errorf("ListSchedules: scan: %w", err)
		}
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

// DeleteSchedule removes a schedule by name.
func (s *MySQLStore) DeleteSchedule(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_schedules WHERE name = ? AND tenant_id = ?
	`, name, s.tenantID)
	if err != nil {
		return fmt.Errorf("DeleteSchedule: %w", err)
	}
	return nil
}

// SetScheduleEnabled enables or disables a schedule.
func (s *MySQLStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules SET enabled = ? WHERE name = ? AND tenant_id = ?
	`, enabled, name, s.tenantID)
	if err != nil {
		return fmt.Errorf("SetScheduleEnabled: %w", err)
	}
	return nil
}

// GetDueSchedules returns enabled schedules whose next_run_at <= NOW(6).
func (s *MySQLStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at
		FROM workflow_schedules
		WHERE enabled = 1 AND next_run_at <= NOW(6) AND tenant_id = ?
		FOR UPDATE SKIP LOCKED
	`, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("GetDueSchedules: %w", err)
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&sch.Input, &sch.Enabled, &sch.NextRunAt, &lastRunAt); err != nil {
			return nil, fmt.Errorf("GetDueSchedules: scan: %w", err)
		}
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

// UpdateScheduleNextRun updates a schedule's next_run_at after firing.
func (s *MySQLStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules SET next_run_at = ?, last_run_at = NOW(6) WHERE name = ? AND tenant_id = ?
	`, nextRun, name, s.tenantID)
	if err != nil {
		return fmt.Errorf("UpdateScheduleNextRun: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

// GetCompactionCandidates returns up to limit workflow IDs whose event
// history exceeds the threshold and could benefit from compaction.
func (s *MySQLStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id
		FROM workflow_instances w
		JOIN (
			SELECT workflow_id, COUNT(*) AS cnt
			FROM event_history
			GROUP BY workflow_id
		) e ON w.id = e.workflow_id
		WHERE e.cnt > ?
		  AND (w.compaction_step IS NULL OR w.compaction_step < e.cnt - ?)
		  AND w.tenant_id = ?
		ORDER BY e.cnt DESC
		LIMIT ?
	`, threshold, threshold, s.tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("GetCompactionCandidates: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("GetCompactionCandidates: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LoadCompactionState returns the compaction state for a workflow, or nil
// if the workflow has not been compacted.
func (s *MySQLStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) {
	var rawJSON []byte
	var compactedStep sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT compaction_state, compaction_step FROM workflow_instances
		WHERE id = ? AND tenant_id = ?
	`, workflowID, s.tenantID).Scan(&rawJSON, &compactedStep)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("LoadCompactionState: %w", err)
	}
	if rawJSON == nil {
		return nil, nil
	}
	var cs CompactionState
	if err := json.Unmarshal(rawJSON, &cs); err != nil {
		return nil, fmt.Errorf("LoadCompactionState: unmarshal: %w", err)
	}
	if compactedStep.Valid {
		cs.CompactedStep = int(compactedStep.Int64)
	}
	return &cs, nil
}

// CompactHistory deletes old events and persists the compaction checkpoint
// for a workflow. compactionStep records the step up to which events were
// compacted; keepStep controls which events are deleted (step < keepStep).
func (s *MySQLStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("CompactHistory: begin: %w", err)
	}
	defer tx.Rollback()

	// Read current generation for optimistic locking.
	var gen int64
	err = tx.QueryRowContext(ctx, `SELECT generation FROM workflow_instances WHERE id = ? AND tenant_id = ?`, workflowID, s.tenantID).Scan(&gen)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit() // Workflow no longer exists.
		}
		return fmt.Errorf("CompactHistory: get generation: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM event_history WHERE workflow_id = ? AND step < ? AND tenant_id = ?
	`, workflowID, keepStep, s.tenantID)
	if err != nil {
		return fmt.Errorf("CompactHistory: delete: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET compaction_state = ?, compacted_at = NOW(6), compaction_step = ?
		WHERE id = ? AND tenant_id = ? AND generation = ?
	`, compactionState, compactionStep, workflowID, s.tenantID, gen)
	if err != nil {
		return fmt.Errorf("CompactHistory: update: %w", err)
	}

	return tx.Commit()
}

// ---------------------------------------------------------------------------
// List / Search
// ---------------------------------------------------------------------------

// ListWorkflows returns workflow instances filtered by the given filter parameters.
func (s *MySQLStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) {
	d := s.dialect
	qb := NewQueryBuilder(d,
		"SELECT "+d.workflowInstanceColumns()+" FROM workflow_instances WHERE tenant_id = ?",
	)
	qb.AddArgs(s.tenantID)

	if filter.Status != "" {
		qb.AddCondition("status = %s", filter.Status)
	}
	if filter.InputContains != "" {
		qb.AddLikeCondition(d.castExpr("input"), "%"+filter.InputContains+"%", true)
	}
	if filter.ErrorContains != "" {
		qb.AddLikeCondition("error_msg", "%"+filter.ErrorContains+"%", true)
	}
	if filter.Search != "" {
		pattern := "%" + filter.Search + "%"
		icol := d.castExpr("input")
		rcol := d.castExpr("result")
		n := qb.NextPos()
		qb.AddRaw(fmt.Sprintf("AND (%s OR %s OR %s OR %s)",
			d.likeExpr(icol, n, true),
			d.likeExpr(rcol, n+1, true),
			d.likeExpr("error_msg", n+2, true),
			d.likeExpr("def_name", n+3, true)))
		qb.AddArgs(pattern, pattern, pattern, pattern)
	}

	qb.AddRaw("ORDER BY created_at DESC")

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}

	if filter.Offset > 0 {
		qb.AddRaw(d.limitOffset(qb.NextPos(), qb.NextPos()+1, true))
		qb.AddArgs(limit, filter.Offset)
	} else {
		qb.AddRaw(d.limitOffset(qb.NextPos(), 0, false))
		qb.AddArgs(limit)
	}

	query, args := qb.SQL()
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListWorkflows: %w", err)
	}
	defer rows.Close()

	var workflows []WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		if err := d.scanWorkflowInstance(rows, &wf); err != nil {
			return nil, fmt.Errorf("ListWorkflows: scan: %w", err)
		}
		workflows = append(workflows, wf)
	}
	return workflows, rows.Err()
}

// GetWorkflowByID returns a single workflow instance by ID.
func (s *MySQLStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) {
	var wf WorkflowInstance
	var nextWakeAt, heartbeatAt, completedAt sql.NullTime
	var assignedTo, errorMsg sql.NullString
	var result sql.NullString
	var tenantID sql.NullString

	var errorCode, errorOp sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, def_name, def_version, status, input,
		       assigned_to, heartbeat_at, next_wake_at, completed_at,
		       CAST(result AS CHAR), error_msg, error_code, error_op,
		       generation, COALESCE(priority, 0) AS priority,
		       COALESCE(trace_id, ''), tenant_id
		FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, id, s.tenantID).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
		&assignedTo, &heartbeatAt, &nextWakeAt, &completedAt, &result, &errorMsg,
		&errorCode, &errorOp, &wf.Generation, &wf.Priority, &wf.TraceID, &tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetWorkflowByID: %w", err)
	}
	wf.AssignedTo = assignedTo.String
	wf.Input = json.RawMessage(compactJSONString(string(wf.Input)))
	wf.Result = compactJSONString(result.String)
	wf.Error = errorMsg.String
	wf.ErrorCode = errorCode.String
	wf.ErrorOp = errorOp.String
	wf.TenantID = tenantID.String
	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	return &wf, nil
}

// ---------------------------------------------------------------------------
// Workflow Definitions
// ---------------------------------------------------------------------------

// LoadWASM returns the compiled WASM bytes for a workflow definition.
func (s *MySQLStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	var wasmBytes []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT wasm_bytes FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?
	`, defName, defVersion, s.tenantID).Scan(&wasmBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("wasm not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("LoadWASM: %w", err)
	}
	return wasmBytes, nil
}

// GetWASMLength returns the byte length of the stored WASM binary.
func (s *MySQLStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) {
	var length int64
	// Scoped by tenant: definition names are chosen by whoever deploys, so
	// two customers both calling something "order-processor" is ordinary, and
	// the size of one's compiled WASM is not the other's to read. MySQL has no
	// row-level security, so this predicate is the whole of the isolation.
	// IMPROVEMENT-PLAN 3.11. (ListVersions, immediately below, has always
	// carried it -- this statement was the odd one out.)
	err := s.db.QueryRowContext(ctx,
		`SELECT LENGTH(wasm_bytes) FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?`,
		defName, defVersion, s.tenantID).Scan(&length)
	return length, err
}

// ListVersions returns all deployed versions of a workflow.
func (s *MySQLStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version FROM workflow_defs WHERE name = ? AND tenant_id = ? ORDER BY version DESC
	`, defName, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("ListVersions: %w", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// LoadWorkflowConfig returns the max_history_length for a workflow definition.
func (s *MySQLStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	var maxHistoryLength int
	err := s.db.QueryRowContext(ctx, `
		SELECT max_history_length FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?
	`, defName, defVersion, s.tenantID).Scan(&maxHistoryLength)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return 0, fmt.Errorf("LoadWorkflowConfig: %w", err)
	}
	return maxHistoryLength, nil
}

// LoadDAGSpec returns the dag_spec JSON for a workflow definition, or nil if none.
func (s *MySQLStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	var raw *[]byte
	err := s.db.QueryRowContext(ctx, `
		SELECT dag_spec FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?
	`, defName, defVersion, s.tenantID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("LoadDAGSpec: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	return json.RawMessage(*raw), nil
}

// TraceWorkflow sets the W3C trace_id on a workflow instance.
func (s *MySQLStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET trace_id = ? WHERE id = ? AND tenant_id = ?
	`, traceID, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("TraceWorkflow: %w", err)
	}
	return nil
}

// DeployWorkflowDef inserts or updates a workflow definition.
func (s *MySQLStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	pluginDepsJSON, _ := json.Marshal(def.PluginDeps)
	if pluginDepsJSON == nil {
		pluginDepsJSON = []byte("{}")
	}
	// Refuse to deploy over a definition owned by another tenant.
	//
	// MySQL has no row-level security, so this check is the only thing between
	// a deploy and another tenant's WASM bytes: the primary key is (name,
	// version) with no tenant in it, and ON DUPLICATE KEY UPDATE turns the
	// collision into an overwrite. IMPROVEMENT-PLAN 3.12.
	//
	// Read and write in one transaction, with the row locked. SELECT ... FOR
	// UPDATE also takes a gap lock on the unique index when the row does not
	// exist, so a concurrent deploy of the same new name blocks here rather
	// than slipping between the read and the insert.
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("DeployWorkflowDef: begin: %w", err)
	}
	defer tx.Rollback()

	var owner sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT tenant_id FROM workflow_defs WHERE name = ? AND version = ? FOR UPDATE`,
		def.Name, def.Version).Scan(&owner)
	switch {
	case err == nil:
		if !canAdoptDef(owner.String, s.tenantID) {
			return defOwnershipError(def.Name, def.Version)
		}
	case errors.Is(err, sql.ErrNoRows):
		// Does not exist yet; the insert below creates it.
	default:
		return fmt.Errorf("DeployWorkflowDef: read owner: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, plugin_deps, deprecated, tenant_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			wasm_bytes = VALUES(wasm_bytes),
			abi_version = VALUES(abi_version),
			min_version = VALUES(min_version),
			plugin_deps = VALUES(plugin_deps),
			deprecated = VALUES(deprecated),
			tenant_id = VALUES(tenant_id)
	`, def.Name, def.Version, def.WASMBytes, def.ABIVersion, def.MinVersion, pluginDepsJSON, def.Deprecated, s.tenantID)
	if err != nil {
		return fmt.Errorf("DeployWorkflowDef: %w", err)
	}
	return tx.Commit()
}

// ListWorkflowDefs returns all versions of a workflow, ordered by version DESC.
// If name is empty, returns all workflow definitions across all workflows.
func (s *MySQLStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	var rows *sql.Rows
	var err error
	if name == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs WHERE tenant_id = ?
			ORDER BY name, version DESC
		`, s.tenantID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs WHERE name = ? AND tenant_id = ?
			ORDER BY version DESC
		`, name, s.tenantID)
	}
	if err != nil {
		return nil, fmt.Errorf("ListWorkflowDefs: %w", err)
	}
	defer rows.Close()

	var defs []WorkflowDef
	for rows.Next() {
		var def WorkflowDef
		var pluginDepsRaw []byte
		var createdAt time.Time
		if err := rows.Scan(&def.Name, &def.Version, &def.ABIVersion, &def.MinVersion,
			&pluginDepsRaw, &createdAt, &def.Deprecated); err != nil {
			return nil, fmt.Errorf("ListWorkflowDefs: scan: %w", err)
		}
		def.CreatedAt = createdAt
		if len(pluginDepsRaw) > 0 {
			json.Unmarshal(pluginDepsRaw, &def.PluginDeps)
		}
		if def.PluginDeps == nil {
			def.PluginDeps = make(map[string]string)
		}
		defs = append(defs, def)
	}
	return defs, rows.Err()
}

// GetWorkflowDef returns a single workflow definition by name and version.
func (s *MySQLStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) {
	var def WorkflowDef
	var pluginDepsRaw []byte
	var wasmBytes []byte
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT name, version, wasm_bytes, abi_version, min_version, plugin_deps, created_at, deprecated
		FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?
	`, name, version, s.tenantID).Scan(&def.Name, &def.Version, &wasmBytes, &def.ABIVersion,
		&def.MinVersion, &pluginDepsRaw, &createdAt, &def.Deprecated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetWorkflowDef: %w", err)
	}
	def.WASMBytes = wasmBytes
	def.CreatedAt = createdAt
	if len(pluginDepsRaw) > 0 {
		json.Unmarshal(pluginDepsRaw, &def.PluginDeps)
	}
	if def.PluginDeps == nil {
		def.PluginDeps = make(map[string]string)
	}
	return &def, nil
}

// MarkVersionDeprecated sets the deprecated flag on a workflow version.
func (s *MySQLStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_defs SET deprecated = ? WHERE name = ? AND version = ? AND tenant_id = ?
	`, deprecated, name, version, s.tenantID)
	if err != nil {
		return fmt.Errorf("MarkVersionDeprecated: %w", err)
	}
	return nil
}

// PurgeWorkflowDef permanently deletes a workflow definition (WASM bytes and all).
func (s *MySQLStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?
	`, name, version, s.tenantID)
	if err != nil {
		return fmt.Errorf("PurgeWorkflowDef: %w", err)
	}
	return nil
}

// CountActiveInstances returns the number of running/ready instances for a version.
func (s *MySQLStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE def_name = ? AND def_version = ?
		  AND status IN ('ready', 'running')
		  AND tenant_id = ?
	`, name, version, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountActiveInstances: %w", err)
	}
	return count, nil
}

// ResolveLatestVersion resolves the latest (highest) version for a named definition.
func (s *MySQLStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM workflow_defs
		WHERE name = ? AND NOT deprecated AND tenant_id = ?
	`, defName, s.tenantID).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("ResolveLatestVersion: %w", err)
	}
	return version, nil
}

// ValidateVersion checks whether the given version is valid (exists and not deprecated).
func (s *MySQLStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_defs
		WHERE name = ? AND version = ? AND NOT deprecated AND tenant_id = ?
	`, defName, defVersion, s.tenantID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("ValidateVersion: %w", err)
	}
	return count > 0, nil
}

// GetActiveInstanceCountsByVersion returns a map of "name:version" -> count for
// all workflow definitions that have active instances.
func (s *MySQLStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) {
	if err := s.requireTenant("GetActiveInstanceCountsByVersion"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name, def_version, COUNT(*) as cnt
		FROM workflow_instances
		WHERE status IN ('ready', 'running')
		  AND tenant_id = ?
		GROUP BY def_name, def_version
	`, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("GetActiveInstanceCountsByVersion: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var name string
		var version, count int
		if err := rows.Scan(&name, &version, &count); err != nil {
			return nil, fmt.Errorf("GetActiveInstanceCountsByVersion: scan: %w", err)
		}
		key := name + ":" + fmt.Sprintf("%d", version)
		counts[key] = count
	}
	return counts, rows.Err()
}

// ---------------------------------------------------------------------------
// Memory Stats
// ---------------------------------------------------------------------------

// RecordWorkflowMemorySample inserts a new memory sample and updates the EWMA summary.
func (s *MySQLStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("RecordWorkflowMemorySample: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO workflow_memory_samples (def_name, sample_bytes) VALUES (?, ?)`,
		defName, sampleBytes)
	if err != nil {
		return fmt.Errorf("RecordWorkflowMemorySample: insert: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_memory_stats (def_name, mean_bytes, sample_count, updated_at)
		VALUES (?, ?, 1, NOW(6))
		ON DUPLICATE KEY UPDATE
			mean_bytes   = (alpha * VALUES(mean_bytes) + (1 - alpha) * mean_bytes),
			sample_count = sample_count + 1,
			updated_at   = NOW(6)
	`, defName, float64(sampleBytes))
	if err != nil {
		return fmt.Errorf("RecordWorkflowMemorySample: upsert: %w", err)
	}

	return tx.Commit()
}

// LoadMemoryEstimates returns EWMA mean bytes for all def_names.
func (s *MySQLStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_name, mean_bytes FROM workflow_memory_stats`)
	if err != nil {
		return nil, fmt.Errorf("LoadMemoryEstimates: %w", err)
	}
	defer rows.Close()

	estimates := make(map[string]float64)
	for rows.Next() {
		var name string
		var mean float64
		if err := rows.Scan(&name, &mean); err != nil {
			return nil, fmt.Errorf("LoadMemoryEstimates: scan: %w", err)
		}
		estimates[name] = mean
	}
	return estimates, rows.Err()
}

// LoadMemoryStats returns full distribution statistics for all def_names.
// Percentiles are computed in Go since MySQL lacks PERCENTILE_CONT.
func (s *MySQLStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name, sample_bytes
		FROM workflow_memory_samples
		ORDER BY def_name, sample_bytes
	`)
	if err != nil {
		return nil, fmt.Errorf("LoadMemoryStats: %w", err)
	}
	defer rows.Close()

	// Group samples by def_name.
	type samples struct {
		vals []int64
	}
	grouped := make(map[string]*samples)
	var orderedKeys []string
	for rows.Next() {
		var name string
		var val int64
		if err := rows.Scan(&name, &val); err != nil {
			return nil, fmt.Errorf("LoadMemoryStats: scan: %w", err)
		}
		if _, ok := grouped[name]; !ok {
			grouped[name] = &samples{}
			orderedKeys = append(orderedKeys, name)
		}
		grouped[name].vals = append(grouped[name].vals, val)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var stats []WorkflowMemoryStats
	for _, name := range orderedKeys {
		s := grouped[name]
		sorted := s.vals
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

		var sum int64
		for _, v := range sorted {
			sum += v
		}
		avg := float64(sum) / float64(len(sorted))

		st := WorkflowMemoryStats{
			DefName:     name,
			MinBytes:    sorted[0],
			AvgBytes:    avg,
			MaxBytes:    sorted[len(sorted)-1],
			P10:         percentile(sorted, 0.10),
			P25:         percentile(sorted, 0.25),
			P50:         percentile(sorted, 0.50),
			P75:         percentile(sorted, 0.75),
			P90:         percentile(sorted, 0.90),
			P99:         percentile(sorted, 0.99),
			SampleCount: len(sorted),
		}
		stats = append(stats, st)
	}
	return stats, nil
}

// percentile returns the p-th percentile (0.0–1.0) of sorted data using
// the nearest-rank method.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	k := int(math.Ceil(p * float64(len(sorted))))
	if k < 1 {
		k = 1
	}
	if k > len(sorted) {
		k = len(sorted)
	}
	return sorted[k-1]
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

// QueueDepth returns the count of ready workflows in the store's task queues.
func (s *MySQLStore) QueueDepth(ctx context.Context) (int64, error) {
	clause, args := s.taskQueueClause()
	// QueueDepth drives autoscaling and the dashboard's backlog figure.
	// Unscoped it counted every tenant's ready workflows, so one tenant's
	// burst read as everyone's. IMPROVEMENT-PLAN 3.11.
	query := `SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready' AND task_queue IN (` +
		clause + `) AND tenant_id = ?`
	args = append(args, s.tenantID)

	var count int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("QueueDepth: %w", err)
	}
	return count, nil
}

// CleanupMemorySamples deletes samples beyond maxSamplesPerDef per def_name.
func (s *MySQLStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
	defRows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT def_name FROM workflow_memory_samples`)
	if err != nil {
		return 0, fmt.Errorf("CleanupMemorySamples: list: %w", err)
	}
	defer defRows.Close()

	var defNames []string
	for defRows.Next() {
		var name string
		if err := defRows.Scan(&name); err != nil {
			return 0, fmt.Errorf("CleanupMemorySamples: scan: %w", err)
		}
		defNames = append(defNames, name)
	}
	if err := defRows.Err(); err != nil {
		return 0, err
	}

	var totalDeleted int64
	for _, defName := range defNames {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_memory_samples
			WHERE def_name = ?
			  AND id NOT IN (
			      SELECT id FROM (
			          SELECT id FROM workflow_memory_samples
			          WHERE def_name = ?
			          ORDER BY recorded_at DESC
			          LIMIT ?
			      ) AS keep
			  )
		`, defName, defName, maxSamplesPerDef)
		if err != nil {
			return totalDeleted, fmt.Errorf("CleanupMemorySamples: delete %s: %w", defName, err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
	}
	return totalDeleted, nil
}

// DeleteExpiredEvents deletes event history rows for workflows that are in a
// terminal state (completed/failed) and whose last update is older than the
// cutoff time. It also cleans up associated compaction states.
// Returns the number of event rows deleted.
func (s *MySQLStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		result, err := s.db.ExecContext(ctx, `
			DELETE e FROM event_history e
			INNER JOIN (
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < ?
				  AND tenant_id = ?
				ORDER BY completed_at
				LIMIT 10000
			) AS w ON e.workflow_id = w.id
		`, olderThan, s.tenantID)
		if err != nil {
			return totalDeleted, fmt.Errorf("DeleteExpiredEvents: %w", err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Also batch cleanup compaction states for those workflows.
	for {
		result, err := s.db.ExecContext(ctx, `
			UPDATE workflow_instances w
			INNER JOIN (
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < ?
				  AND compaction_state IS NOT NULL
				ORDER BY completed_at
				LIMIT 10000
			) AS subq ON w.id = subq.id
			SET w.compaction_state = NULL, w.compaction_step = NULL, w.compacted_at = NULL
		`, olderThan)
		if err != nil {
			break
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return totalDeleted, nil
}

// TerminateWorkflow force-terminates a workflow, setting status to 'terminated'.
func (s *MySQLStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'terminated',
		    error_msg = ?,
		    completed_at = NOW(),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE id = ? AND tenant_id = ?
	`, reason, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("terminate workflow: %w", err)
	}
	return nil
}

// DeleteDeadLetteredWorkflows permanently deletes dead-lettered workflow instances
// whose completed_at is older than the cutoff. Child rows (event_history, signals,
// promises, concurrency_keys, update_requests) are automatically deleted via
// ON DELETE CASCADE.
func (s *MySQLStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		result, err := s.db.ExecContext(ctx, `
			DELETE w FROM workflow_instances w
			INNER JOIN (
				SELECT id FROM workflow_instances
				WHERE status = 'dead_lettered'
				  AND completed_at IS NOT NULL
				  AND completed_at < ?
				  AND tenant_id = ?
				ORDER BY id
				LIMIT 10000
			) d ON w.id = d.id
		`, olderThan, s.tenantID)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete dead-lettered workflows: %w", err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return totalDeleted, nil
}

// GetChildCount returns the number of active (non-terminal) child workflows
// for the given parent workflow. Terminal statuses are excluded.
func (s *MySQLStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE parent_workflow_id = ? AND status NOT IN ('done', 'failed', 'dead_lettered') AND tenant_id = ?
	`, parentWorkflowID, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: %w", parentWorkflowID, err)
	}
	return count, nil
}

// GetConcurrencyKeyCount returns the number of non-expired concurrency keys
// held by the given workflow.
func (s *MySQLStore) GetConcurrencyKeyCount(ctx context.Context, workflowID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM concurrency_keys
		WHERE workflow_id = ? AND expires_at > NOW(6) AND tenant_id = ?
	`, workflowID, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get concurrency key count for %s: %w", workflowID, err)
	}
	return count, nil
}

// GetEventCount returns the event_count for a workflow instance.
func (s *MySQLStore) GetEventCount(ctx context.Context, workflowID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = ? AND tenant_id = ?`, workflowID, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get event count for %s: %w", workflowID, err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// Tag methods (deployment channels)
// ---------------------------------------------------------------------------

// SetWorkflowTag assigns a tag to a specific version.
// Uses INSERT ... ON DUPLICATE KEY UPDATE so reassigning a tag updates in place.
func (s *MySQLStore) SetWorkflowTag(ctx context.Context, workflowName string, version int, tag string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_tags (workflow_name, version, tag, tenant_id)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE version = VALUES(version), created_at = NOW(6)
	`, workflowName, version, tag, s.tenantID)
	if err != nil {
		return fmt.Errorf("SetWorkflowTag: %w", err)
	}
	return nil
}

// RemoveWorkflowTag deletes a tag assignment.
func (s *MySQLStore) RemoveWorkflowTag(ctx context.Context, workflowName string, tag string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_tags WHERE workflow_name = ? AND tag = ? AND tenant_id = ?
	`, workflowName, tag, s.tenantID)
	if err != nil {
		return fmt.Errorf("RemoveWorkflowTag: %w", err)
	}
	return nil
}

// GetWorkflowTag returns the version for a given tag.
func (s *MySQLStore) GetWorkflowTag(ctx context.Context, workflowName string, tag string) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `
		SELECT version FROM workflow_tags WHERE workflow_name = ? AND tag = ? AND tenant_id = ?
	`, workflowName, tag, s.tenantID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("GetWorkflowTag: tag %q not found for workflow %s", tag, workflowName)
	}
	if err != nil {
		return 0, fmt.Errorf("GetWorkflowTag: %w", err)
	}
	return version, nil
}

// GetWorkflowTags returns all tag -> version mappings for a workflow.
func (s *MySQLStore) GetWorkflowTags(ctx context.Context, workflowName string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tag, version FROM workflow_tags WHERE workflow_name = ? AND tenant_id = ?
	`, workflowName, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("GetWorkflowTags: %w", err)
	}
	defer rows.Close()

	tags := make(map[string]int)
	for rows.Next() {
		var tag string
		var version int
		if err := rows.Scan(&tag, &version); err != nil {
			return nil, fmt.Errorf("GetWorkflowTags: scan: %w", err)
		}
		tags[tag] = version
	}
	return tags, rows.Err()
}

// ---------------------------------------------------------------------------
// Routing methods (A/B traffic splitting)
// ---------------------------------------------------------------------------

// SetRoutingRule creates a routing rule for a workflow version.
func (s *MySQLStore) SetRoutingRule(ctx context.Context, workflowName string, targetVersion int, weight float64) error {
	id := uuid.New().String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_routing (id, workflow_name, target_version, weight, tenant_id)
		VALUES (?, ?, ?, ?, ?)
	`, id, workflowName, targetVersion, weight, s.tenantID)
	if err != nil {
		return fmt.Errorf("SetRoutingRule: %w", err)
	}
	return nil
}

// RemoveRoutingRule deletes a routing rule by ID.
func (s *MySQLStore) RemoveRoutingRule(ctx context.Context, ruleID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_routing WHERE id = ? AND tenant_id = ?
	`, ruleID, s.tenantID)
	if err != nil {
		return fmt.Errorf("RemoveRoutingRule: %w", err)
	}
	return nil
}

// GetRoutingRules returns all routing rules for a workflow.
func (s *MySQLStore) GetRoutingRules(ctx context.Context, workflowName string) ([]RoutingRule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, workflow_name, target_version, weight
		FROM workflow_routing WHERE workflow_name = ? AND tenant_id = ?
	`, workflowName, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("GetRoutingRules: %w", err)
	}
	defer rows.Close()

	var rules []RoutingRule
	for rows.Next() {
		var r RoutingRule
		if err := rows.Scan(&r.ID, &r.WorkflowName, &r.TargetVersion, &r.Weight); err != nil {
			return nil, fmt.Errorf("GetRoutingRules: scan: %w", err)
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// PickVersionByRouting performs weighted random version selection.
// Returns 0 if no routing rules exist.
func (s *MySQLStore) PickVersionByRouting(ctx context.Context, workflowName string) (int, error) {
	rules, err := s.GetRoutingRules(ctx, workflowName)
	if err != nil {
		return 0, err
	}
	if len(rules) == 0 {
		return 0, nil
	}

	total := 0.0
	for _, r := range rules {
		total += r.Weight
	}
	if total <= 0 {
		return 0, nil
	}

	// Use crypto/rand for weighted selection.
	scale := int64(1_000_000_000)
	scaledTotal := int64(total * float64(scale))
	if scaledTotal <= 0 {
		return 0, nil
	}

	n, err := rand.Int(rand.Reader, big.NewInt(scaledTotal))
	if err != nil {
		return 0, fmt.Errorf("PickVersionByRouting: random: %w", err)
	}
	pick := n.Int64()

	cumulative := int64(0)
	for _, r := range rules {
		cumulative += int64(r.Weight * float64(scale))
		if pick < cumulative {
			return r.TargetVersion, nil
		}
	}
	return rules[len(rules)-1].TargetVersion, nil
}

// ---------------------------------------------------------------------------
// Version Resolution
// ---------------------------------------------------------------------------

// ResolveVersionByTag resolves a tag to a version number.
// If tag is "latest", returns the highest non-deprecated version.
func (s *MySQLStore) ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error) {
	if tag == "latest" {
		return s.ResolveLatestVersion(ctx, workflowName)
	}
	var version int
	err := s.db.QueryRowContext(ctx, `
		SELECT version FROM workflow_tags WHERE workflow_name = ? AND tag = ? AND tenant_id = ?
	`, workflowName, tag, s.tenantID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("ResolveVersionByTag: tag %q not found for workflow %s", tag, workflowName)
	}
	if err != nil {
		return 0, fmt.Errorf("ResolveVersionByTag: %w", err)
	}
	return version, nil
}
