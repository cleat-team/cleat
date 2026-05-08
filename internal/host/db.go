package host

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// WorkflowDef is a row from the workflow_defs table.
// It represents a deployed version of a workflow definition.
type WorkflowDef struct {
	Name        string            `json:"name"`
	Version     int               `json:"version"`
	WASMBytes   []byte            `json:"wasm_bytes,omitempty"`
	ABIVersion  int               `json:"abi_version"`
	MinVersion  int               `json:"min_version"`
	PluginDeps  map[string]string `json:"plugin_deps,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	Deprecated  bool              `json:"deprecated"`
}

// WorkflowInstance is a row from workflow_instances.
type WorkflowInstance struct {
	ID         string          `json:"id"`
	DefName    string          `json:"def_name"`
	DefVersion int             `json:"def_version"`
	MinVersion int             `json:"min_version"`
	Status     string          `json:"status"`
	Input      json.RawMessage `json:"input"`
	Result     string          `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	AssignedTo string          `json:"assigned_to"`
	NextWakeAt time.Time       `json:"next_wake_at"`
	TenantID   string          `json:"tenant_id,omitempty"`
}

// Schedule is a row from workflow_schedules.
type Schedule struct {
	Name           string          `json:"name"`
	DefName        string          `json:"def_name"`
	EntryPoint     string          `json:"entry_point"`
	CronExpression string          `json:"cron_expression"`
	Input          json.RawMessage `json:"input"`
	Enabled        bool            `json:"enabled"`
	NextRunAt      time.Time       `json:"next_run_at"`
	LastRunAt      *time.Time      `json:"last_run_at,omitempty"`
}

// PromiseInfo holds the state of a cleat promise.
type PromiseInfo struct {
	PromiseID   string     `json:"promise_id"`
	PromiseName string     `json:"promise_name"`
	Status      string     `json:"status"`
	Result      string     `json:"result,omitempty"`
	ErrorMsg    string     `json:"error_msg,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// ConcurrencyKeyInfo holds the state of an acquired concurrency key.
type ConcurrencyKeyInfo struct {
	KeyHash    []byte    `json:"key_hash"`
	KeyText    string    `json:"key_text"`
	WorkflowID string    `json:"workflow_id"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}


// UpdateRequestInfo holds the state of an incoming update request.
type UpdateRequestInfo struct {
	WorkflowID string    `json:"workflow_id"`
	UpdateName string    `json:"update_name"`
	Payload    string    `json:"payload"`
	PromiseID  string    `json:"promise_id,omitempty"`
	Status     string    `json:"status"`
	Result     string    `json:"result,omitempty"`
	ErrorMsg   string    `json:"error_msg,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// WorkflowMemoryStats holds distribution statistics for per-definition memory usage.
type WorkflowMemoryStats struct {
	DefName     string  `json:"def_name"`
	MinBytes    int64   `json:"min_bytes"`
	AvgBytes    float64 `json:"avg_bytes"`
	MaxBytes    int64   `json:"max_bytes"`
	P10         int64   `json:"p10"`
	P25         int64   `json:"p25"`
	P50         int64   `json:"p50"`
	P75         int64   `json:"p75"`
	P90         int64   `json:"p90"`
	P99         int64   `json:"p99"`
	SampleCount int     `json:"sample_count"`
}

// WorkflowStore is the database interface for the worker.
type WorkflowStore interface {
	// ClaimWorkflow atomically dequeues a runnable workflow instance.
	// Uses SELECT ... FOR UPDATE SKIP LOCKED. Filters by namespace.
	ClaimWorkflow(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error)

	// ClaimWorkflows atomically claims up to limit runnable workflow instances.
	// Like ClaimWorkflow but batches multiple claims into one query.
	ClaimWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error)

	// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
	// that are sticky to this worker. Uses idx_instances_sticky for low-contention
	// claiming. Returns fewer than limit if not enough sticky workflows are ready.
	// Callers should fall back to ClaimWorkflows for remaining capacity.
	ClaimStickyWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error)

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

	// StartNewRun creates a new workflow instance.
	// If idempotencyKey is non-empty, provides exactly-once semantics: a
	// subsequent call with the same key returns the existing workflow ID
	// without creating a duplicate.
	StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (runID string, alreadyExisted bool, err error)

	// StartChildWorkflow creates a child workflow instance linked to a parent.
	// defVersion is the explicit workflow definition version to use, or 0 to use
	// default resolution (SELECT MAX(version)).
	StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string) (runID string, err error)

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

	// LoadDAGSpec returns the dag_spec JSON for a workflow definition, or nil if none.
	LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error)

	// TraceWorkflow sets the W3C trace_id on a workflow instance.
	TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error)

	// GetCompactionCandidates returns up to limit workflow IDs whose event
	// history exceeds the threshold and could benefit from compaction.
	GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error)

	// LoadCompactionState returns the compaction state for a workflow, or nil
	// if the workflow has not been compacted.
	LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error)

	// CompactHistory deletes old events and persists the compaction checkpoint
	// for a workflow. compactionStep records the step up to which events were
	// compacted; keepStep controls which events are deleted (step < keepStep).
	CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error

	// CreatePromise creates a new promise for a workflow.
	CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error

	// ResolvePromise marks a promise as resolved with the given result.
	ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error

	// RejectPromise marks a promise as rejected with the given error message.
	RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error

	// GetPromise returns the current status and result of a promise.
	GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error)

	// ListPromises returns all promises for a workflow ordered by creation time.
	ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error)


	// ---- Update Request methods (Feature 3: Update Handler) ----
	
	// CreateUpdateRequest registers an incoming update request for a workflow.
	// The update will be dispatched to the workflow's registered handler.
	CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error
	
	// GetPendingUpdateRequests returns all pending (not yet dispatched) update
	// requests for a workflow.
	GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error)
	
	// CompleteUpdateRequest marks an update request as completed with a result or error.
	CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error

	// ---- Concurrency Key methods (Feature 5) ----

	// AcquireConcurrencyKey tries to acquire a concurrency key for a workflow.
	// Returns true if acquired, false if already held by another workflow.
	// Automatically releases expired keys during acquisition.
	AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (acquired bool, err error)

	// ReleaseConcurrencyKey releases a specific concurrency key.
	ReleaseConcurrencyKey(ctx context.Context, key string) error

	// ReleaseWorkflowConcurrencyKeys releases all concurrency keys held by a workflow.
	ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error

	// ReapExpiredConcurrencyKeys deletes all expired concurrency keys.
	// Returns the number of keys deleted.
	ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error)

	// ---- Sticky Session methods (Feature 10) ----

	// UpdateStickyWorker sets the sticky worker for a workflow.
	UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error

	// ClearStickyWorker removes the sticky worker assignment.
	ClearStickyWorker(ctx context.Context, workflowID string) error

	// ---- Version Management methods ----

	// DeployWorkflowDef inserts or updates a workflow definition.
	DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error

	// ListWorkflowDefs returns all versions of a workflow, ordered by version DESC.
	// If name is empty, returns all workflow definitions across all workflows.
	ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error)

	// GetWorkflowDef returns a single workflow definition by name and version.
	GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error)

	// MarkVersionDeprecated sets the deprecated flag on a workflow version.
	MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error

	// PurgeWorkflowDef permanently deletes a workflow definition (WASM bytes and all).
	PurgeWorkflowDef(ctx context.Context, name string, version int) error

	// CountActiveInstances returns the number of running/ready instances for a version.
	CountActiveInstances(ctx context.Context, name string, version int) (int, error)

	// GetActiveInstanceCountsByVersion returns a map of "name:version" -> count for
	// all workflow definitions that have active instances.
	GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error)

	// RecordWorkflowMemorySample inserts a new memory sample and updates the EWMA summary.
	RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error

	// LoadMemoryEstimates returns EWMA mean bytes for all def_names.
	LoadMemoryEstimates(ctx context.Context) (map[string]float64, error)

	// LoadMemoryStats returns full distribution statistics for all def_names.
	LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error)

	// QueueDepth returns the count of ready workflows in the store's task queues.
	QueueDepth(ctx context.Context) (int64, error)

	// CleanupMemorySamples deletes samples beyond maxSamplesPerDef per def_name.
	CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error)
}

// PostgresStore implements WorkflowStore using a PostgreSQL database.
type PostgresStore struct {
	db         *sql.DB
	taskQueues []string
	tenantID   string
}

// NewPostgresStore creates a PostgresStore scoped to the given task queues.
// The taskQueues slice specifies which task queues this worker pool should poll
// (e.g., "default", "gpu", "high-memory"). Defaults to ["default"].
// The tenantID defaults to the default tenant UUID from the tenant foundation migration.
func NewPostgresStore(db *sql.DB, taskQueues ...string) *PostgresStore {
	tqs := taskQueues
	if len(tqs) == 0 {
		tqs = []string{"default"}
	}
	return &PostgresStore{
		db:         db,
		taskQueues: tqs,
		tenantID:   "00000000-0000-0000-0000-000000000000",
	}
}

// WithTenant returns a copy of the store scoped to the given tenant ID.
// This is used in the dispatch loop to set the correct tenant context
// before executing a workflow. The returned store's methods will set
// the RLS session variable via set_config.
func (s *PostgresStore) WithTenant(tenantID string) *PostgresStore {
	cp := *s
	cp.tenantID = tenantID
	return &cp
}

// setRLSOnTx executes SELECT set_config to set the RLS tenant_id
// for the given transaction. This ensures the RLS policy on tenant-scoped
// tables correctly filters rows by the current tenant. Must be called
// after BEGIN and before any tenant-scoped queries.
func (s *PostgresStore) setRLSOnTx(tx *sql.Tx) error {
	if s.tenantID == "" {
		return nil
	}
	_, err := tx.Exec("SELECT set_config('cleat.tenant_id', $1, true)", s.tenantID)
	return err
}

// beginTxWithRLS begins a transaction and sets the RLS tenant context,
// ensuring all subsequent queries in the transaction are scoped to the
// current tenant. The caller must commit or rollback the returned tx.
func (s *PostgresStore) beginTxWithRLS(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginTxWithRLS: begin tx: %w", err)
	}
	if err := s.setRLSOnTx(tx); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("set row-level security: %w", err)
	}
	return tx, nil
}

// ClaimWorkflow atomically claims a runnable workflow using SKIP LOCKED.
func (s *PostgresStore) ClaimWorkflow(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error) {
	return s.claimWorkflowImpl(ctx, workerID, namespace)
}

func (s *PostgresStore) claimWorkflowImpl(ctx context.Context, workerID, namespace string) (*WorkflowInstance, error) {
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
// Like ClaimWorkflow but batches multiple claims into one query.
func (s *PostgresStore) ClaimWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = $1,
		    heartbeat_at = now()
		WHERE id IN (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			  AND namespace = $2
			  AND task_queue = ANY($3)
			ORDER BY CASE WHEN sticky_worker_id = $1 THEN 0 ELSE 1 END, created_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, def_name, def_version, status, input, assigned_to, next_wake_at, tenant_id
	`, workerID, namespace, pq.Array(s.taskQueues), limit)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: %w", err)
	}
	defer rows.Close()

	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&wf.AssignedTo, &nextWakeAt, &tenantID); err != nil {
			return nil, fmt.Errorf("claim workflows scan: %w", err)
		}

		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
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
// that are sticky to this worker. Filters on sticky_worker_id to use the
// idx_instances_sticky partial index for low-contention claiming.
func (s *PostgresStore) ClaimStickyWorkflows(ctx context.Context, workerID, namespace string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = $1,
		    heartbeat_at = now()
		WHERE id IN (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			  AND sticky_worker_id = $1
			  AND namespace = $2
			  AND task_queue = ANY($3)
			ORDER BY created_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, def_name, def_version, status, input, assigned_to, next_wake_at, tenant_id
	`, workerID, namespace, pq.Array(s.taskQueues), limit)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: %w", err)
	}
	defer rows.Close()

	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&wf.AssignedTo, &nextWakeAt, &tenantID); err != nil {
			return nil, fmt.Errorf("claim sticky workflows scan: %w", err)
		}

		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
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

// LoadEventHistory returns all event records for a workflow, ordered by step.
func (s *PostgresStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error
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

// AppendEventHistoryBatch appends multiple events to the history.
func (s *PostgresStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append history batch: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return fmt.Errorf("append history batch: set rls: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
			duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
			defer_description, defer_id, child_name, child_input, run_id, new_input,
			plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
			promise_name, promise_id, promise_result, promise_error, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("append history batch: prepare: %w", err)
	}
	defer stmt.Close()

	for _, rec := range recs {
		payload, err := eventRecordToPayload(rec)
		payloadArg := nullStr("")
		if err == nil && len(payload) > 0 {
			payloadArg = sql.NullString{String: string(payload), Valid: true}
		}
		_, err = stmt.ExecContext(ctx, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(rec.Request), nullStr(rec.Response), nullStr(rec.Err),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(rec.SignalPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(rec.PluginInput), nullStr(rec.PluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(rec.PromiseResult), nullStr(rec.PromiseError),
			payloadArg)
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

// LoadDAGSpec returns the dag_spec JSON for a workflow definition, or nil if none.
func (s *PostgresStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	var spec json.RawMessage
	err := s.db.QueryRowContext(ctx, `
		SELECT dag_spec FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&spec)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load dag_spec: %w", err)
	}
	return spec, nil
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

// DeployWorkflowDef inserts or updates a workflow definition.
func (s *PostgresStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	pluginDepsJSON, _ := json.Marshal(def.PluginDeps)
	if pluginDepsJSON == nil {
		pluginDepsJSON = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, plugin_deps, deprecated)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (name, version) DO UPDATE SET
			wasm_bytes = EXCLUDED.wasm_bytes,
			abi_version = EXCLUDED.abi_version,
			min_version = EXCLUDED.min_version,
			plugin_deps = EXCLUDED.plugin_deps,
			deprecated = EXCLUDED.deprecated
	`, def.Name, def.Version, def.WASMBytes, def.ABIVersion, def.MinVersion, pluginDepsJSON, def.Deprecated)
	if err != nil {
		return fmt.Errorf("deploy workflow def: %w", err)
	}
	return nil
}

// ListWorkflowDefs returns all versions of a workflow, ordered by version DESC.
// If name is empty, returns all workflow definitions across all workflows.
func (s *PostgresStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	var rows *sql.Rows
	var err error
	if name == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs ORDER BY name, version DESC
		`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs WHERE name = $1 ORDER BY version DESC
		`, name)
	}
	if err != nil {
		return nil, fmt.Errorf("list workflow defs: %w", err)
	}
	defer rows.Close()

	var defs []WorkflowDef
	for rows.Next() {
		var def WorkflowDef
		var pluginDepsRaw []byte
		var createdAt time.Time
		if err := rows.Scan(&def.Name, &def.Version, &def.ABIVersion, &def.MinVersion,
			&pluginDepsRaw, &createdAt, &def.Deprecated); err != nil {
			return nil, fmt.Errorf("scan workflow def: %w", err)
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
func (s *PostgresStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) {
	var def WorkflowDef
	var pluginDepsRaw []byte
	var wasmBytes []byte
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT name, version, wasm_bytes, abi_version, min_version, plugin_deps, created_at, deprecated
		FROM workflow_defs WHERE name = $1 AND version = $2
	`, name, version).Scan(&def.Name, &def.Version, &wasmBytes, &def.ABIVersion,
		&def.MinVersion, &pluginDepsRaw, &createdAt, &def.Deprecated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow def: %w", err)
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
func (s *PostgresStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_defs SET deprecated = $3 WHERE name = $1 AND version = $2
	`, name, version, deprecated)
	if err != nil {
		return fmt.Errorf("mark version deprecated: %w", err)
	}
	return nil
}

// PurgeWorkflowDef permanently deletes a workflow definition.
func (s *PostgresStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_defs WHERE name = $1 AND version = $2
	`, name, version)
	if err != nil {
		return fmt.Errorf("purge workflow def: %w", err)
	}
	return nil
}

// CountActiveInstances returns the number of ready or running instances for a version.
func (s *PostgresStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE def_name = $1 AND def_version = $2
		  AND status IN ('ready', 'running')
	`, name, version).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active instances: %w", err)
	}
	return count, nil
}

// GetActiveInstanceCountsByVersion returns a map of "name:version" -> count.
func (s *PostgresStore) GetActiveInstanceCountsByVersion(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name, def_version, COUNT(*) as cnt
		FROM workflow_instances
		WHERE status IN ('ready', 'running')
		GROUP BY def_name, def_version
	`)
	if err != nil {
		return nil, fmt.Errorf("get active instance counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var name string
		var version, count int
		if err := rows.Scan(&name, &version, &count); err != nil {
			return nil, fmt.Errorf("scan active instance count: %w", err)
		}
		key := name + ":" + fmt.Sprintf("%d", version)
		counts[key] = count
	}
	return counts, rows.Err()
}

// CleanupMemorySamples deletes samples beyond maxSamplesPerDef per def_name.
func (s *PostgresStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM memory_samples
		WHERE (def_name, timestamp) NOT IN (
			SELECT def_name, timestamp FROM (
				SELECT def_name, timestamp,
					ROW_NUMBER() OVER (PARTITION BY def_name ORDER BY timestamp DESC) as rn
				FROM memory_samples
			) sub WHERE sub.rn <= $1
		)`, maxSamplesPerDef)
	if err != nil {
		return 0, fmt.Errorf("cleanup memory samples: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RecordWorkflowMemorySample inserts a new memory sample and updates the EWMA summary.
func (s *PostgresStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_memory_samples (def_name, sample_bytes)
		VALUES ($1, $2)`, defName, sampleBytes)
	if err != nil {
		return fmt.Errorf("record workflow memory sample: %w", err)
	}
	return nil
}

// LoadMemoryEstimates returns EWMA mean bytes for all def_names.
func (s *PostgresStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name, COALESCE(AVG(sample_bytes), 0)
		FROM workflow_memory_samples
		GROUP BY def_name`)
	if err != nil {
		return nil, fmt.Errorf("load memory estimates: %w", err)
	}
	defer rows.Close()
	estimates := make(map[string]float64)
	for rows.Next() {
		var name string
		var avg float64
		if err := rows.Scan(&name, &avg); err != nil {
			return nil, fmt.Errorf("scan memory estimate: %w", err)
		}
		estimates[name] = avg
	}
	return estimates, rows.Err()
}

// LoadMemoryStats returns full distribution statistics for all def_names.
func (s *PostgresStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name, MIN(sample_bytes), COALESCE(AVG(sample_bytes), 0), MAX(sample_bytes),
			COUNT(*)
		FROM workflow_memory_samples
		GROUP BY def_name`)
	if err != nil {
		return nil, fmt.Errorf("load memory stats: %w", err)
	}
	defer rows.Close()
	var stats []WorkflowMemoryStats
	for rows.Next() {
		var ws WorkflowMemoryStats
		if err := rows.Scan(&ws.DefName, &ws.MinBytes, &ws.AvgBytes, &ws.MaxBytes, &ws.SampleCount); err != nil {
			return nil, fmt.Errorf("scan memory stats: %w", err)
		}
		stats = append(stats, ws)
	}
	return stats, rows.Err()
}

// QueueDepth returns the count of ready workflows in the store's task queues.
func (s *PostgresStore) QueueDepth(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE status = 'ready'`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return count, nil
}

// Heartbeat updates the heartbeat timestamp.
func (s *PostgresStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM workflow_defs
		WHERE name = $1 AND NOT deprecated
	`, defName).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("resolve latest version: %w", err)
	}
	return version, nil
}

// ValidateVersion checks whether a specific workflow definition version
// exists and is not deprecated. Returns true if the version can be used.
//
//	SQL: SELECT EXISTS(SELECT 1 FROM workflow_defs
//	     WHERE name = $1 AND version = $2 AND NOT deprecated)
func (s *PostgresStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM workflow_defs
			WHERE name = $1 AND version = $2 AND NOT deprecated
		)
	`, defName, defVersion).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("validate version: %w", err)
	}
	return exists, nil
}

// Heartbeat updates the heartbeat timestamp.
func (s *PostgresStore) Heartbeat(ctx context.Context, workflowID, workerID string) (bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, fmt.Errorf("heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = now()
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, tx.Commit()
}

// CompleteWorkflow marks a workflow as done.
func (s *PostgresStore) CompleteWorkflow(ctx context.Context, workflowID, workerID, result string, queryState map[string]string) error {
	tx, err := s.beginTxWithRLS(ctx)
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
		SET status = 'done', result = $3, completed_at = now(), assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, result, qsJSON)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: record result in idempotency_keys if this workflow was started with a key.
	s.db.ExecContext(ctx,
		`UPDATE idempotency_keys SET result = $3 WHERE workflow_id = $1`,
		workflowID, result)

	// Best-effort: clear sticky worker assignment (Feature 10).
	s.ClearStickyWorker(context.Background(), workflowID)
	// Best-effort: release all concurrency keys (Feature 5).
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.
func (s *PostgresStore) FailWorkflow(ctx context.Context, workflowID, workerID, errMsg string, queryState map[string]string) error {
	tx, err := s.beginTxWithRLS(ctx)
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
		SET status = 'failed', error_msg = $3, completed_at = now(), assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, errMsg, qsJSON)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort: record error in idempotency_keys if this workflow was started with a key.
	s.db.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $3 WHERE workflow_id = $1`,
		workflowID, errMsg)

	// Best-effort: clear sticky worker assignment (Feature 10).
	s.ClearStickyWorker(context.Background(), workflowID)
	// Best-effort: release all concurrency keys (Feature 5).
	s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Runs as a best-effort operation.
func (s *PostgresStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	// Terminate children with TERMINATE policy.
	s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated'
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)

	// Request cancellation for children with REQUEST_CANCEL policy.
	s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
}

// ReleaseWorkflow returns a workflow to the queue with a next wake time.
func (s *PostgresStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, nextWakeAt time.Time) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("release workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = $3
		WHERE id = $1 AND assigned_to = $2
	`, workflowID, workerID, nextWakeAt)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// RequestCancellation sets the cancellation flag.
func (s *PostgresStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("request cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true, cancellation_reason = $2
		WHERE id = $1
	`, workflowID, reason)
	if err != nil {
		return err
	}

	return tx.Commit()
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

	if err := s.setRLSOnTx(tx); err != nil {
		return "", false, err
	}

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
// If idempotencyKey is non-empty, provides exactly-once semantics: a subsequent
// call with the same key returns the existing workflow ID without creating a
// duplicate. Returns the workflow ID, whether it already existed, and any error.
func (s *PostgresStore) StartNewRun(ctx context.Context, defName string, defVersion int, input json.RawMessage, idempotencyKey string) (string, bool, error) {
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
		if err != sql.ErrNoRows {
			return "", false, err
		}

		// Generate workflow ID early so we can insert into both tables atomically.
		var workflowID string
		if err := s.db.QueryRowContext(ctx, `SELECT gen_random_uuid()`).Scan(&workflowID); err != nil {
			return "", false, fmt.Errorf("generate id: %w", err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return "", false, err
		}
		defer tx.Rollback()

		// Insert idempotency key record. ON CONFLICT DO NOTHING handles the
		// race where two requests arrive with the same key simultaneously.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at)
			 VALUES ($1, $2, now() + INTERVAL '7 days')
			 ON CONFLICT (key_hash) DO NOTHING`,
			keyHash[:], workflowID)
		if err != nil {
			return "", false, err
		}

		n, _ := res.RowsAffected()
		if n == 0 {
			// Key was inserted concurrently — rollback and return the existing one.
			tx.Rollback()
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
		err = tx.QueryRowContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, namespace, task_queue)
			VALUES ($1, $2, $3, 'ready', $4,
			        COALESCE((SELECT namespace FROM workflow_defs WHERE name = $2 AND version = $3), 'default'),
			        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3), 'default'))
			RETURNING id
		`, workflowID, defName, defVersion, input).Scan(&workflowID)
		if err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		return workflowID, false, tx.Commit()
	}

	// No idempotency key — normal flow.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	var runID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, namespace, task_queue)
		VALUES (gen_random_uuid(), $1, $2, 'ready', $3,
		        COALESCE((SELECT namespace FROM workflow_defs WHERE name = $1 AND version = $2), 'default'),
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $1 AND version = $2), 'default'))
		RETURNING id
	`, defName, defVersion, input).Scan(&runID)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	return runID, false, tx.Commit()
}

// StartChildWorkflow creates a child workflow instance linked to a parent.
// The child inherits the namespace from the parent.
// If defVersion > 0, that version is used explicitly; otherwise the latest
// non-deprecated version is used (SELECT MAX(version)).
func (s *PostgresStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string) (string, error) {
	var runID string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, namespace, task_queue)
		VALUES (gen_random_uuid(), $1,
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated) END,
		        'ready', $2, $3,
		        COALESCE(NULLIF($5, ''), 'ABANDON'),
		        COALESCE((SELECT namespace FROM workflow_instances WHERE id = $3), 'default'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $3), 'default'))
		RETURNING id
	`, defName, inputJSON, parentID, defVersion, parentClosePolicy).Scan(&runID)
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
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("deliver signal: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload)
		VALUES ($1, $2, $3)
		ON CONFLICT (workflow_id, signal_name) DO UPDATE SET payload = $3, delivered_at = now()
	`, workflowID, signalName, payload)
	if err != nil {
		return err
	}

	return tx.Commit()
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
	var result sql.NullString
	var inputRaw json.RawMessage

	err := s.db.QueryRowContext(ctx, `
		SELECT id, def_name, def_version, status, input,
		       assigned_to, heartbeat_at, next_wake_at, completed_at, result::text, error_msg
		FROM workflow_instances WHERE id = $1
	`, id).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &inputRaw,
		&assignedTo, &heartbeatAt, &nextWakeAt, &completedAt, &result, &errorMsg)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	wf.Input = inputRaw
	wf.AssignedTo = assignedTo.String
	wf.Result = result.String
	wf.Error = errorMsg.String
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

// CompactHistory deletes old events and saves compaction state for a workflow.
func (s *PostgresStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("compact history: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return fmt.Errorf("compact history: set rls: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM event_history WHERE workflow_id = $1 AND step < $2 AND tenant_id = $3
	`, workflowID, keepStep, s.tenantID)
	if err != nil {
		return fmt.Errorf("compact history: delete: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET compaction_state = $1, compacted_at = now(), compaction_step = $2
		WHERE id = $3 AND tenant_id = $4
	`, compactionState, compactionStep, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("compact history: update: %w", err)
	}

	return tx.Commit()
}

// GetCompactionCandidates returns workflow IDs that need compaction.
func (s *PostgresStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id
		FROM workflow_instances w
		JOIN (
			SELECT workflow_id, COUNT(*) AS cnt
			FROM event_history
			GROUP BY workflow_id
		) e ON w.id = e.workflow_id
		WHERE e.cnt > $1
		  AND (w.compaction_step IS NULL OR w.compaction_step < e.cnt - $1)
		  AND w.tenant_id = $2
		ORDER BY e.cnt DESC
		LIMIT $3
	`, threshold, s.tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("get compaction candidates: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan compaction candidate: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LoadCompactionState loads the compaction state JSON for a workflow instance.
func (s *PostgresStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) {
	var rawJSON []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT compaction_state FROM workflow_instances
		WHERE id = $1 AND tenant_id = $2
	`, workflowID, s.tenantID).Scan(&rawJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load compaction state: %w", err)
	}
	if rawJSON == nil {
		return nil, nil
	}
	var cs CompactionState
	if err := json.Unmarshal(rawJSON, &cs); err != nil {
		return nil, fmt.Errorf("unmarshal compaction state: %w", err)
	}
	return &cs, nil
}

// ---- PromiseStore interface implementation ----

// CreatePromise creates a new promise for a workflow instance.
func (s *PostgresStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, status)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workflow_id, promise_id) DO NOTHING
	`, workflowID, promiseID, promiseName, "pending")
	return err
}

// ResolvePromise marks a promise as resolved with the given result.
// Also wakes the workflow instance so it can pick up the resolved promise
// on the next poll cycle instead of waiting for the original timeout.
func (s *PostgresStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_promises SET status = $3, result = $4, resolved_at = now()
		WHERE workflow_id = $1 AND promise_id = $2
	`, workflowID, promiseID, "resolved", result)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = now()
		WHERE id = $1 AND status = 'ready'
	`, workflowID)
	return nil
}

// RejectPromise marks a promise as rejected with the given error message.
// Also wakes the workflow instance so it can pick up the rejected promise
// on the next poll cycle instead of waiting for the original timeout.
func (s *PostgresStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_promises SET status = $3, error_msg = $4, resolved_at = now()
		WHERE workflow_id = $1 AND promise_id = $2
	`, workflowID, promiseID, "rejected", errMsg)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = now()
		WHERE id = $1 AND status = 'ready'
	`, workflowID)
	return nil
}

// GetPromise returns the current status and result of a promise.
func (s *PostgresStore) GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error) {
	var resultStr, errStr sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT status, result::text, error_msg FROM workflow_promises
		WHERE workflow_id = $1 AND promise_id = $2
	`, workflowID, promiseID).Scan(&status, &resultStr, &errStr)
	if err == sql.ErrNoRows {
		return "pending", "", "", nil
	}
	if err != nil {
		return "", "", "", err
	}
	return status, resultStr.String, errStr.String, nil
}

// ListPromises returns all promises for a workflow ordered by creation time.
func (s *PostgresStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT promise_id, promise_name, status, COALESCE(result::text, ''), COALESCE(error_msg, ''), created_at, resolved_at
		FROM workflow_promises
		WHERE workflow_id = $1
		ORDER BY created_at
	`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var promises []PromiseInfo
	for rows.Next() {
		var pi PromiseInfo
		var resolvedAt sql.NullTime
		if err := rows.Scan(&pi.PromiseID, &pi.PromiseName, &pi.Status, &pi.Result, &pi.ErrorMsg, &pi.CreatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			pi.ResolvedAt = &resolvedAt.Time
		}
		promises = append(promises, pi)
	}
	return promises, rows.Err()
}

// ---- Concurrency Key implementations (Feature 5) ----

// AcquireConcurrencyKey tries to acquire a concurrency key for a workflow.
// Returns true if acquired, false if already held by another workflow.
func (s *PostgresStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	// First delete expired keys for this key hash.
	_, err := s.db.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE key_hash = digest($1, 'sha256') AND expires_at < now()`, key)
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: delete expired: %w", err)
	}

	// Try to insert. ON CONFLICT DO NOTHING means if the key_hash already exists,
	// the RETURNING clause returns no rows.
	var returnedWorkflowID string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at)
		VALUES (digest($1, 'sha256'), $1, $2, now() + $3::interval)
		ON CONFLICT (key_hash) DO NOTHING
		RETURNING workflow_id
	`, key, workflowID, fmt.Sprintf("%d seconds", int(ttl.Seconds()))).Scan(&returnedWorkflowID)

	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: %w", err)
	}
	return true, nil
}

// ReleaseConcurrencyKey releases a specific concurrency key.
func (s *PostgresStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE key_hash = digest($1, 'sha256')`, key)
	if err != nil {
		return fmt.Errorf("release concurrency key: %w", err)
	}
	return nil
}

// ReleaseWorkflowConcurrencyKeys releases all concurrency keys held by a workflow.
func (s *PostgresStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE workflow_id = $1`, workflowID)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: %w", err)
	}
	return nil
}

// ReapExpiredConcurrencyKeys deletes all expired concurrency keys.
// Returns the number of keys deleted.
func (s *PostgresStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// ---- Sticky Session implementations (Feature 10) ----

// UpdateStickyWorker sets the sticky worker for a workflow.
func (s *PostgresStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = $2 WHERE id = $1
	`, workflowID, workerID)
	if err != nil {
		return fmt.Errorf("update sticky worker: %w", err)
	}
	return nil
}

// ClearStickyWorker removes the sticky worker assignment.
func (s *PostgresStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = NULL WHERE id = $1
	`, workflowID)
	if err != nil {
		return fmt.Errorf("clear sticky worker: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Update Request methods (Feature 3: Update Handler)
// ---------------------------------------------------------------------------

// CreateUpdateRequest registers an incoming update request for a workflow.
func (s *PostgresStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_update_requests (workflow_id, update_name, payload, promise_id, status)
		VALUES ($1, $2, $3, $4, 'pending')
	`, workflowID, updateName, payload, promiseID)
	return err
}

// GetPendingUpdateRequests returns all pending (not yet dispatched) update requests.
func (s *PostgresStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workflow_id, update_name, payload::text, COALESCE(promise_id, ''), status,
		       COALESCE(result::text, ''), COALESCE(error_msg, ''), created_at
		FROM workflow_update_requests
		WHERE workflow_id = $1 AND status = 'pending'
		ORDER BY created_at
	`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []UpdateRequestInfo
	for rows.Next() {
		var r UpdateRequestInfo
		if err := rows.Scan(&r.WorkflowID, &r.UpdateName, &r.Payload, &r.PromiseID,
			&r.Status, &r.Result, &r.ErrorMsg, &r.CreatedAt); err != nil {
			return nil, err
		}
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

// CompleteUpdateRequest marks an update request as completed with a result or error.
func (s *PostgresStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_update_requests
		SET status = 'completed', result = $3, error_msg = $4, completed_at = now()
		WHERE workflow_id = $1 AND update_name = $2 AND status = 'pending'
	`, workflowID, updateName, result, errMsg)
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

// eventRecordToPayload serializes event-type-specific fields into a JSON map.
func eventRecordToPayload(rec EventRecord) ([]byte, error) {
	payload := make(map[string]interface{})
	switch rec.EventType {
	case "call":
		payload["service"] = rec.Service
		payload["operation"] = rec.Op
		payload["request"] = rec.Request
		if rec.Response != "" {
			payload["response"] = rec.Response
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
		if rec.DurationMs > 0 {
			payload["duration_ms"] = rec.DurationMs
		}
	case "sleep":
		payload["duration_ms"] = rec.DurationMs
	case "await_signals":
		if rec.SignalNames != "" {
			payload["signal_names"] = rec.SignalNames
		}
		if rec.TimeoutMs > 0 {
			payload["timeout_ms"] = rec.TimeoutMs
		}
	case "signal_received":
		if rec.SignalName != "" {
			payload["signal_name"] = rec.SignalName
		}
		if rec.SignalPayload != "" {
			payload["signal_payload"] = rec.SignalPayload
		}
	case "defer":
		if rec.DeferDescription != "" {
			payload["defer_description"] = rec.DeferDescription
		}
		if rec.DeferID != "" {
			payload["defer_id"] = rec.DeferID
		}
	case "child_workflow":
		if rec.ChildName != "" {
			payload["child_name"] = rec.ChildName
		}
		if rec.ChildInput != "" {
			payload["child_input"] = rec.ChildInput
		}
		if rec.RunID != "" {
			payload["run_id"] = rec.RunID
		}
	case "continue_as_new":
		if rec.NewInput != "" {
			payload["new_input"] = rec.NewInput
		}
	case "plugin_call":
		if rec.PluginName != "" {
			payload["plugin_name"] = rec.PluginName
		}
		if rec.PluginFunc != "" {
			payload["plugin_func"] = rec.PluginFunc
		}
		if rec.PluginInput != "" {
			payload["plugin_input"] = rec.PluginInput
		}
		if rec.PluginOutput != "" {
			payload["plugin_output"] = rec.PluginOutput
		}
		if rec.PluginError != "" {
			payload["plugin_error"] = rec.PluginError
		}
	case "create_promise":
		payload["promise_name"] = rec.PromiseName
		payload["promise_id"] = rec.PromiseID
	case "await_promise", "promise_resolved", "promise_rejected":
		payload["promise_id"] = rec.PromiseID
		if rec.PromiseResult != "" {
			payload["promise_result"] = rec.PromiseResult
		}
		if rec.PromiseError != "" {
			payload["promise_error"] = rec.PromiseError
		}
	case "update_handler":
		if rec.UpdateHandlerName != "" {
			payload["update_handler_name"] = rec.UpdateHandlerName
		}
	case "state_mutation":
		if rec.StateKey != "" {
			payload["state_key"] = rec.StateKey
		}
		if rec.StateValue != "" {
			payload["state_value"] = rec.StateValue
		}
		if rec.StateDelta != 0 {
			payload["state_delta"] = rec.StateDelta
		}
		if rec.StateOp != "" {
			payload["state_op"] = rec.StateOp
		}
	case "run_detached":
		// No extra fields to store.
	case "side_effect":
		if rec.SideEffectResult != "" {
			payload["side_effect_result"] = rec.SideEffectResult
		}
	case "plugin_call_stream_chunk":
		if rec.PluginName != "" {
			payload["plugin_name"] = rec.PluginName
		}
		if rec.PluginFunc != "" {
			payload["plugin_func"] = rec.PluginFunc
		}
		if rec.PluginInput != "" {
			payload["plugin_input"] = rec.PluginInput
		}
		if rec.PluginOutput != "" {
			payload["plugin_output"] = rec.PluginOutput
		}
		if rec.PluginError != "" {
			payload["plugin_error"] = rec.PluginError
		}
	case "scope_acquired":
		if rec.ScopeKey != "" {
			payload["scope_key"] = rec.ScopeKey
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
	}
	return json.Marshal(payload)
}

// populateFromPayload fills event-type-specific fields from a JSONB payload.
func populateFromPayload(rec *EventRecord, payload []byte) {
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return
	}
	switch rec.EventType {
	case "call":
		if v, ok := m["service"].(string); ok { rec.Service = v }
		if v, ok := m["operation"].(string); ok { rec.Op = v }
		if v, ok := m["request"].(string); ok { rec.Request = v }
		if v, ok := m["response"].(string); ok { rec.Response = v }
		if v, ok := m["error"].(string); ok { rec.Err = v }
		if v, ok := m["duration_ms"].(float64); ok { rec.DurationMs = int64(v) }
	case "sleep":
		if v, ok := m["duration_ms"].(float64); ok { rec.DurationMs = int64(v) }
	case "await_signals":
		if v, ok := m["signal_names"].(string); ok { rec.SignalNames = v }
		if v, ok := m["timeout_ms"].(float64); ok { rec.TimeoutMs = int64(v) }
	case "signal_received":
		if v, ok := m["signal_name"].(string); ok { rec.SignalName = v }
		if v, ok := m["signal_payload"].(string); ok { rec.SignalPayload = v }
	case "defer":
		if v, ok := m["defer_description"].(string); ok { rec.DeferDescription = v }
		if v, ok := m["defer_id"].(string); ok { rec.DeferID = v }
	case "child_workflow":
		if v, ok := m["child_name"].(string); ok { rec.ChildName = v }
		if v, ok := m["child_input"].(string); ok { rec.ChildInput = v }
		if v, ok := m["run_id"].(string); ok { rec.RunID = v }
	case "continue_as_new":
		if v, ok := m["new_input"].(string); ok { rec.NewInput = v }
	case "plugin_call":
		if v, ok := m["plugin_name"].(string); ok { rec.PluginName = v }
		if v, ok := m["plugin_func"].(string); ok { rec.PluginFunc = v }
		if v, ok := m["plugin_input"].(string); ok { rec.PluginInput = v }
		if v, ok := m["plugin_output"].(string); ok { rec.PluginOutput = v }
		if v, ok := m["plugin_error"].(string); ok { rec.PluginError = v }
	case "create_promise", "await_promise", "promise_resolved", "promise_rejected":
		if v, ok := m["promise_name"].(string); ok { rec.PromiseName = v }
		if v, ok := m["promise_id"].(string); ok { rec.PromiseID = v }
		if v, ok := m["promise_result"].(string); ok { rec.PromiseResult = v }
		if v, ok := m["promise_error"].(string); ok { rec.PromiseError = v }
	case "update_handler":
		if v, ok := m["update_handler_name"].(string); ok { rec.UpdateHandlerName = v }
	case "state_mutation":
		if v, ok := m["state_key"].(string); ok { rec.StateKey = v }
		if v, ok := m["state_value"].(string); ok { rec.StateValue = v }
		if v, ok := m["state_delta"].(float64); ok { rec.StateDelta = int64(v) }
		if v, ok := m["state_op"].(string); ok { rec.StateOp = v }
	case "run_detached":
		// No extra fields to restore.
	case "side_effect":
		if v, ok := m["side_effect_result"].(string); ok { rec.SideEffectResult = v }
	case "plugin_call_stream_chunk":
		if v, ok := m["plugin_name"].(string); ok { rec.PluginName = v }
		if v, ok := m["plugin_func"].(string); ok { rec.PluginFunc = v }
		if v, ok := m["plugin_input"].(string); ok { rec.PluginInput = v }
		if v, ok := m["plugin_output"].(string); ok { rec.PluginOutput = v }
		if v, ok := m["plugin_error"].(string); ok { rec.PluginError = v }
	case "scope_acquired":
		if v, ok := m["scope_key"].(string); ok { rec.ScopeKey = v }
		if v, ok := m["error"].(string); ok { rec.Err = v }
	}
}

// RecordWorkflowMemorySample inserts a new sample and updates the EWMA summary.
func (s *PostgresStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record memory sample: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO workflow_memory_samples (def_name, sample_bytes) VALUES ($1, $2)`,
		defName, sampleBytes)
	if err != nil {
		return fmt.Errorf("record memory sample: insert sample: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_memory_stats (def_name, mean_bytes, sample_count, updated_at)
		VALUES ($1, $2, 1, now())
		ON CONFLICT (def_name) DO UPDATE SET
			mean_bytes   = (workflow_memory_stats.alpha * $2 + (1 - workflow_memory_stats.alpha) * workflow_memory_stats.mean_bytes),
			sample_count = workflow_memory_stats.sample_count + 1,
			updated_at   = now()
	`, defName, float64(sampleBytes))
	if err != nil {
		return fmt.Errorf("record memory sample: upsert stats: %w", err)
	}

	return tx.Commit()
}

// LoadMemoryEstimates returns EWMA mean bytes for all def_names.
func (s *PostgresStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_name, mean_bytes FROM workflow_memory_stats`)
	if err != nil {
		return nil, fmt.Errorf("load memory estimates: %w", err)
	}
	defer rows.Close()

	estimates := make(map[string]float64)
	for rows.Next() {
		var name string
		var mean float64
		if err := rows.Scan(&name, &mean); err != nil {
			return nil, fmt.Errorf("load memory estimates: scan: %w", err)
		}
		estimates[name] = mean
	}
	return estimates, rows.Err()
}

// LoadMemoryStats returns full distribution statistics for all def_names.
func (s *PostgresStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name,
		       MIN(sample_bytes)::BIGINT,
		       AVG(sample_bytes),
		       MAX(sample_bytes)::BIGINT,
		       COALESCE(percentile_cont(0.10) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.25) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.75) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.90) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY sample_bytes)::BIGINT, 0),
		       COUNT(*)::INTEGER
		FROM workflow_memory_samples
		GROUP BY def_name
		ORDER BY def_name
	`)
	if err != nil {
		return nil, fmt.Errorf("load memory stats: %w", err)
	}
	defer rows.Close()

	var stats []WorkflowMemoryStats
	for rows.Next() {
		var st WorkflowMemoryStats
		if err := rows.Scan(&st.DefName, &st.MinBytes, &st.AvgBytes, &st.MaxBytes,
			&st.P10, &st.P25, &st.P50, &st.P75, &st.P90, &st.P99, &st.SampleCount); err != nil {
			return nil, fmt.Errorf("load memory stats: scan: %w", err)
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

// QueueDepth returns the count of ready workflows in the store's task queues.
func (s *PostgresStore) QueueDepth(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready' AND task_queue = ANY($1)`,
		pq.Array(s.taskQueues)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return count, nil
}

// CleanupMemorySamples deletes samples beyond maxSamplesPerDef per def_name.
func (s *PostgresStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
	defRows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT def_name FROM workflow_memory_samples`)
	if err != nil {
		return 0, fmt.Errorf("cleanup memory samples: list defs: %w", err)
	}
	defer defRows.Close()

	var defNames []string
	for defRows.Next() {
		var name string
		if err := defRows.Scan(&name); err != nil {
			return 0, fmt.Errorf("cleanup memory samples: scan def: %w", err)
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
			WHERE def_name = $1
			  AND id NOT IN (
			      SELECT id FROM workflow_memory_samples
			      WHERE def_name = $1
			      ORDER BY recorded_at DESC
			      LIMIT $2
			  )
		`, defName, maxSamplesPerDef)
		if err != nil {
			return totalDeleted, fmt.Errorf("cleanup memory samples: delete %s: %w", defName, err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
	}
	return totalDeleted, nil
}
