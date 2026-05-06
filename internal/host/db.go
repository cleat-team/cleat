package host

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// WorkflowInstance is a row from workflow_instances.
type WorkflowInstance struct {
	ID         string
	DefName    string
	DefVersion int
	MinVersion int
	Status     string
	Input      json.RawMessage
	AssignedTo string
	NextWakeAt time.Time
}

// Schedule is a row from workflow_schedules.
type Schedule struct {
	Name          string
	DefName       string
	EntryPoint    string
	CronExpression string
	Input         json.RawMessage
	Enabled       bool
	NextRunAt     time.Time
	LastRunAt     *time.Time
}

// WorkflowStore is the database interface for the worker.
type WorkflowStore interface {
	// ClaimWorkflow atomically dequeues a runnable workflow instance.
	// Uses SELECT ... FOR UPDATE SKIP LOCKED. Filters by namespace.
	ClaimWorkflow(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error)

	// LoadEventHistory returns the full event history for a workflow.
	LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error)

	// AppendEventHistory appends a single event to the history.
	// Uses ON CONFLICT (workflow_id, step) DO NOTHING for idempotency.
	AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error

	// AppendEventHistoryBatch appends multiple events atomically.
	AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error

	// LoadWASM returns the compiled WASM bytes for a workflow definition.
	LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error)

	// ListVersions returns all deployed versions of a workflow.
	ListVersions(ctx context.Context, defName string) ([]int, error)

	// Heartbeat updates the heartbeat timestamp to prevent timeout.
	// Returns false if the workflow is no longer assigned to this worker.
	Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error)

	// CompleteWorkflow marks a workflow as completed with a result.
	CompleteWorkflow(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error

	// FailWorkflow marks a workflow as failed.
	FailWorkflow(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error

	// ReleaseWorkflow returns a workflow to the ready queue.
	// Used when a workflow suspends (sleep/await signals).
	ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error

	// RequestCancellation sets the cancellation flag on a workflow.
	RequestCancellation(ctx context.Context, workflowID, reason string) error

	// CheckCancellation checks if a workflow has been cancelled.
	CheckCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error)

	// DeliverSignal stores a signal for a workflow.
	DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error

	// PollAndClaimSignal atomically checks for and claims a pending signal.
	PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error)

	// StartNewRun creates a new workflow instance (for ContinueAsNew and child workflows).
	StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage) (runID string, err error)

	// StartChildWorkflow creates a child workflow instance linked to a parent.
	StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string) (runID string, err error)

	// GetChildResult checks whether a child workflow has completed and returns its result.
	GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error)

	// ReapStaleInstances reclaims workflow instances that have been running
	// but whose heartbeat has not been updated within the given timeout.
	// Returns the number of instances reclaimed.
	ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error)

	// GetQueryState returns the query state for a workflow instance key.
	GetQueryState(ctx context.Context, workflowID, key string) (string, error)

	// ListWorkflows returns workflow instances filtered by status.
	ListWorkflows(ctx context.Context, status string, limit int) ([]WorkflowInstance, error)

	// GetWorkflowByID returns a single workflow instance by ID.
	GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error)

	// CreateSchedule inserts a new cron schedule.
	CreateSchedule(ctx context.Context, s Schedule) error

	// ListSchedules returns all registered schedules.
	ListSchedules(ctx context.Context) ([]Schedule, error)

	// DeleteSchedule removes a schedule by name.
	DeleteSchedule(ctx context.Context, name string) error

	// SetScheduleEnabled enables or disables a schedule.
	SetScheduleEnabled(ctx context.Context, name string, enabled bool) error

	// GetDueSchedules returns enabled schedules whose next_run_at <= now().
	GetDueSchedules(ctx context.Context) ([]Schedule, error)

	// UpdateScheduleNextRun updates a schedule's next_run_at after firing.
	UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error

	// LoadWorkflowConfig returns the max_history_length for a workflow definition.
	LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (maxHistoryLength int, err error)

	// TraceWorkflow sets the W3C trace_id on a workflow instance.
	TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error)
}

// PostgresStore implements WorkflowStore using a PostgreSQL database.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore creates a PostgresStore.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// ClaimWorkflow atomically claims a runnable workflow using SKIP LOCKED.
func (s *PostgresStore) ClaimWorkflow(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error) {
	return s.claimWorkflowImpl(ctx, workerID, namespace)
}

func (s *PostgresStore) claimWorkflowImpl(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error) {
	var wf WorkflowInstance
	var nextWakeAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = $1,
		    heartbeat_at = now()
		WHERE id = (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			  AND namespace = $2
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, def_name, def_version, status, input, assigned_to, next_wake_at
	`, workerID, namespace).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
		&wf.AssignedTo, &nextWakeAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim workflow: %w", err)
	}

	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	return &wf, nil
}

// LoadEventHistory returns all event records for a workflow, ordered by step.
func (s *PostgresStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input
		FROM event_history
		WHERE workflow_id = $1
		ORDER BY step
	`, workflowID)
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

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}

		rec.Service = service.String
		rec.Op = op.String
		rec.Request = request.String
		rec.Response = response.String
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

		history = append(history, rec)
	}
	return history, rows.Err()
}

// AppendEventHistoryBatch appends multiple events to the history.
func (s *PostgresStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append history batch: begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
			duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
			defer_description, defer_id, child_name, child_input, run_id, new_input)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("append history batch: prepare: %w", err)
	}
	defer stmt.Close()

	for _, rec := range recs {
		_, err := stmt.ExecContext(ctx, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(rec.Request), nullStr(rec.Response), nullStr(rec.Err),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(rec.SignalPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput))
		if err != nil {
			return fmt.Errorf("append history batch: exec step %d: %w", rec.Step, err)
		}
	}
	return tx.Commit()
}

// AppendEventHistory appends a single event to the history.
func (s *PostgresStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	return s.AppendEventHistoryBatch(ctx, workflowID, []EventRecord{rec})
}

// LoadWASM returns the WASM bytes for a workflow definition.
func (s *PostgresStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	var wasmBytes []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT wasm_bytes FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&wasmBytes)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("wasm not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load wasm: %w", err)
	}
	return wasmBytes, nil
}

// TraceWorkflow sets the W3C trace_id on a workflow instance.
func (s *PostgresStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error) {
	return s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET trace_id = $2 WHERE id = $1
	`, workflowID, traceID)
}

// LoadWorkflowConfig returns configuration for a workflow definition.
func (s *PostgresStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	var maxHistoryLength int
	err := s.db.QueryRowContext(ctx, `
		SELECT max_history_length FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&maxHistoryLength)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return 0, fmt.Errorf("load workflow config: %w", err)
	}
	return maxHistoryLength, nil
}

// ListVersions returns all deployed versions of a workflow.
func (s *PostgresStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version FROM workflow_defs WHERE name = $1 ORDER BY version DESC
	`, defName)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
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

// Heartbeat updates the heartbeat timestamp.
func (s *PostgresStore) Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = now()
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// CompleteWorkflow marks a workflow as done.
func (s *PostgresStore) CompleteWorkflow(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error {
	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = $3, completed_at = now(), assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, result, qsJSON)
	return err
}

// FailWorkflow marks a workflow as failed.
func (s *PostgresStore) FailWorkflow(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
	qsJSON, _ := json.Marshal(queryState)
	if qsJSON == nil {
		qsJSON = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = $3, completed_at = now(), assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, errMsg, qsJSON)
	return err
}

// ReleaseWorkflow returns a workflow to the queue with a next wake time.
func (s *PostgresStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = $3
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, nextWakeAt)
	return err
}

// RequestCancellation sets the cancellation flag.
func (s *PostgresStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true, cancellation_reason = $2
		WHERE id = $1
	`, workflowID, reason)
	return err
}

// CheckCancellation checks if a workflow has been cancelled.
func (s *PostgresStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	var cancelled bool
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = $1
	`, workflowID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, nil
}
// PollAndClaimSignal atomically checks for and claims a pending signal.
func (s *PostgresStore) PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var payload string
	err = tx.QueryRowContext(ctx, `
		DELETE FROM workflow_signals
		WHERE workflow_id = $1 AND signal_name = $2
		RETURNING payload
	`, workflowID, signalName).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", false, tx.Rollback()
	}
	if err != nil {
		return "", false, fmt.Errorf("poll signal: %w", err)
	}
	return payload, true, tx.Commit()
}

// StartNewRun creates a new workflow instance.
func (s *PostgresStore) StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage) (string, error) {
	var runID string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, namespace)
		VALUES (gen_random_uuid(), $1, $2, 'ready', $3,
		        COALESCE((SELECT namespace FROM workflow_defs WHERE name = $1 AND version = $2), 'default'))
		RETURNING id
	`, defName, defVersion, input).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("start new run: %w", err)
	}
	return runID, nil
}

// StartChildWorkflow creates a child workflow instance linked to a parent.
// The child inherits the namespace from the parent.
func (s *PostgresStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string) (string, error) {
	var runID string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, namespace)
		VALUES (gen_random_uuid(), $1, (SELECT MAX(version) FROM workflow_defs WHERE name = $1), 'ready', $2, $3,
		        COALESCE((SELECT namespace FROM workflow_instances WHERE id = $3), 'default'))
		RETURNING id
	`, defName, inputJSON, parentID).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}
	return runID, nil
}

// GetChildResult checks whether a child workflow has completed (status 'done' or 'failed').
func (s *PostgresStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	var result string
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(result, '{}'), status FROM workflow_instances WHERE id = $1
	`, runID).Scan(&result, &status)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result: %w", err)
	}
	if status == "done" || status == "failed" {
		return result, true, nil
	}
	return "", false, nil
}

// ReapStaleInstances reclaims workflow instances with stale heartbeats.
func (s *PostgresStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
		WHERE status = 'running'
		  AND heartbeat_at < now() - $1::interval
	`, fmt.Sprintf("%d seconds", int(timeout.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// ---- SignalStore interface implementation ----

// DeliverSignal satisfies the SignalStore interface.
func (s *PostgresStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload)
		VALUES ($1, $2, $3)
		ON CONFLICT (workflow_id, signal_name) DO UPDATE SET payload = $3, delivered_at = now()
	`, workflowID, signalName, payload)
	return err
}

// PollSignal satisfies the SignalStore interface by checking for a delivered signal.
func (s *PostgresStore) PollSignal(ctx context.Context, workflowID, signalName string) (string, bool, error) {
	return s.PollAndClaimSignal(ctx, workflowID, signalName)
}

// PollCancellation satisfies the SignalStore interface.
func (s *PostgresStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return s.CheckCancellation(ctx, workflowID)
}

// GetQueryState returns the value for a key in the workflow's query_state JSONB.
func (s *PostgresStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	var value sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT query_state ->> $2 FROM workflow_instances WHERE id = $1
	`, workflowID, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get query state: %w", err)
	}
	return value.String, nil
}

// ListWorkflows returns workflow instances filtered by status, ordered by creation time.
func (s *PostgresStore) ListWorkflows(ctx context.Context, status string, limit int) ([]WorkflowInstance, error) {
	query := `
		SELECT id, def_name, def_version, status, input, assigned_to, next_wake_at
		FROM workflow_instances
		WHERE 1=1
	`
	var args []interface{}
	argN := 0

	if status != "" {
		argN++
		query += fmt.Sprintf(" AND status = $%d", argN)
		args = append(args, status)
	}

	query += " ORDER BY created_at DESC"

	if limit > 0 {
		argN++
		query += fmt.Sprintf(" LIMIT $%d", argN)
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()

	var workflows []WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&wf.AssignedTo, &nextWakeAt); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		workflows = append(workflows, wf)
	}
	return workflows, rows.Err()
}

// GetWorkflowByID returns a single workflow instance by ID.
func (s *PostgresStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) {
	var wf WorkflowInstance
	var nextWakeAt, heartbeatAt, completedAt sql.NullTime
	var assignedTo, errorMsg sql.NullString
	var result json.RawMessage

	err := s.db.QueryRowContext(ctx, `
		SELECT id, def_name, def_version, status, input,
		       assigned_to, heartbeat_at, next_wake_at, completed_at, result, error_msg
		FROM workflow_instances WHERE id = $1
	`, id).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
		&assignedTo, &heartbeatAt, &nextWakeAt, &completedAt, &result, &errorMsg)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	wf.AssignedTo = assignedTo.String
	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	return &wf, nil
}

// ---- Schedule methods ----

func (s *PostgresStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, enabled, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, sch.Name, sch.DefName, sch.EntryPoint, sch.CronExpression, sch.Input, sch.Enabled, sch.NextRunAt)
	return err
}

func (s *PostgresStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at
		FROM workflow_schedules ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&sch.Input, &sch.Enabled, &sch.NextRunAt, &lastRunAt); err != nil {
			return nil, err
		}
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

func (s *PostgresStore) DeleteSchedule(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workflow_schedules WHERE name = $1`, name)
	return err
}

func (s *PostgresStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules SET enabled = $2 WHERE name = $1
	`, name, enabled)
	return err
}

func (s *PostgresStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at
		FROM workflow_schedules
		WHERE enabled = true AND next_run_at <= now()
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&sch.Input, &sch.Enabled, &sch.NextRunAt, &lastRunAt); err != nil {
			return nil, err
		}
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

func (s *PostgresStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules SET next_run_at = $2, last_run_at = now() WHERE name = $1
	`, name, nextRun)
	return err
}

// NextCronTime computes the next firing time for a 5-field cron expression
// (minute hour day-of-month month day-of-week) from the given time.
func NextCronTime(cronExpr string, from time.Time) time.Time {
	fields := strings.Fields(cronExpr)
	if len(fields) != 5 {
		return from.Add(24 * time.Hour) // fallback: daily
	}

	// Start at the next minute.
	t := from.Truncate(time.Minute).Add(time.Minute)

	// Search up to 4 years ahead.
	end := from.AddDate(4, 0, 0)
	for t.Before(end) {
		if matchField(fields[0], t.Minute(), 0, 59) &&
			matchField(fields[1], t.Hour(), 0, 23) &&
			matchField(fields[2], t.Day(), 1, 31) &&
			matchField(fields[3], int(t.Month()), 1, 12) &&
			matchField(fields[4], int(t.Weekday()), 0, 6) {
			// Also verify day-of-month is valid for this month.
			if t.Day() <= daysInMonth(t.Year(), t.Month()) {
				return t
			}
		}
		t = t.Add(time.Minute)
	}
	return from.Add(24 * time.Hour)
}

func matchField(pattern string, value int, min, max int) bool {
	if pattern == "*" {
		return true
	}
	// Handle step values: */N
	if strings.HasPrefix(pattern, "*/") {
		step := atoi(strings.TrimPrefix(pattern, "*/"))
		if step > 0 {
			return (value-min)%step == 0
		}
		return false
	}
	// Handle comma-separated lists.
	for _, part := range strings.Split(pattern, ",") {
		part = strings.TrimSpace(part)
		// Handle ranges: N-M
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			lo, hi := atoi(rangeParts[0]), atoi(rangeParts[1])
			if value >= lo && value <= hi {
				return true
			}
		} else if atoi(part) == value {
			return true
		}
	}
	return false
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func atoi(s string) int {
	var n int
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}

// nullStr returns a sql.NullString that is valid if s is non-empty.
func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// nullInt64 returns a sql.NullInt64 that is valid if v is non-zero.
func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v != 0}
}
