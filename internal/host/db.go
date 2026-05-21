package host

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// WorkflowDef is a row from the workflow_defs table.
// It represents a deployed version of a workflow definition.
type WorkflowDef struct {
	Name       string            `json:"name"`
	Version    int               `json:"version"`
	WASMBytes  []byte            `json:"wasm_bytes,omitempty"`
	ABIVersion int               `json:"abi_version"`
	MinVersion int               `json:"min_version"`
	PluginDeps map[string]string `json:"plugin_deps,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	Deprecated bool              `json:"deprecated"`
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
	ErrorCode  string          `json:"error_code,omitempty"`
	ErrorOp    string          `json:"error_op,omitempty"`
	AssignedTo string          `json:"assigned_to"`
	NextWakeAt time.Time       `json:"next_wake_at"`
	TenantID   string          `json:"tenant_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at,omitempty"`
	Generation int64           `json:"generation"`
	Priority   int             `json:"priority"`
	TraceID    string          `json:"trace_id,omitempty"`
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

// WorkflowFilter contains optional filter parameters for listing workflow instances.
// Empty/zero values mean "no filter" for that parameter.
type WorkflowFilter struct {
	Status        string
	InputContains string
	ErrorContains string
	Search        string
	Offset        int
	Limit         int
}

// WorkflowStore is the database interface for the worker.
type WorkflowStore interface {
	// ClaimWorkflow atomically dequeues a runnable workflow instance.
	// Uses SELECT ... FOR UPDATE SKIP LOCKED.
	ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error)

	// ClaimWorkflows atomically claims up to limit runnable workflow instances.
	// Like ClaimWorkflow but batches multiple claims into one query.
	ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error)

	// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
	// that are sticky to this worker. Uses idx_instances_sticky for low-contention
	// claiming. Returns fewer than limit if not enough sticky workflows are ready.
	// Callers should fall back to ClaimWorkflows for remaining capacity.
	ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error)

	// LoadEventHistory returns the full event history for a workflow.
	LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error)

	// LoadEventHistoryPaginated returns a page of event history for a workflow.
	// offset is the number of events to skip (0-based), limit caps the page size
	// (defaults to 1000 if limit <= 0, capped at 1000).
	LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error)

	// CountEventHistory returns the total number of events for a workflow.
	CountEventHistory(ctx context.Context, workflowID string) (int, error)

	// AppendEventHistory appends a single event to the history.
	// Uses ON CONFLICT (workflow_id, step) DO NOTHING for idempotency.
	AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error

	// AppendEventHistoryBatch appends multiple events atomically.
	AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error

	// VerifyWorkflowEvents loads all events for a workflow and verifies their
	// integrity by recomputing SHA-256 checksums and comparing them against the
	// stored checksums (once the event_history.checksum column is migrated).
	// Before the migration, it loads and computes checksums silently and returns
	// nil. Returns an error if any checksum mismatch is detected.
	VerifyWorkflowEvents(ctx context.Context, workflowID string) error

	// LoadWASM returns the compiled WASM bytes for a workflow definition.
	LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error)

	// GetWASMLength returns the byte length of the stored WASM binary.
	GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error)

	// ListVersions returns all deployed versions of a workflow.
	ListVersions(ctx context.Context, defName string) ([]int, error)

	// Heartbeat updates the heartbeat timestamp to prevent timeout.
	// Returns false if the workflow is no longer assigned to this worker
	// or if the generation does not match (workflow was reaped).
	Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error)

	// BatchHeartbeat updates heartbeat_at for all workflows assigned to this
	// worker with status 'running'. Uses a single UPDATE instead of N calls.
	// NOTE: This intentionally does NOT check per-workflow generation because
	// it operates on ALL workflows for a worker, and generations differ per
	// workflow. Individual generation-guarded operations (Heartbeat,
	// CompleteWorkflow, FailWorkflow, etc.) prevent double-execution even if
	// the batch heartbeat refreshes a stale workflow's heartbeat_at.
	BatchHeartbeat(ctx context.Context, workerID string) (int64, error)

	// CompleteWorkflow marks a workflow as completed with a result.
	CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error

	// FailWorkflow marks a workflow as failed.
	FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error

	// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
	// after exhausting all retry attempts. This is a terminal status similar to
	// 'failed' but indicates the workflow was retried without success.
	MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error

	// RetryWorkflow moves a dead_lettered workflow back to a runnable state.
	// Resets status to 'ready', clears the assigned worker and all error fields,
	// and sets next_wake_at to now so the workflow is picked up immediately.
	RetryWorkflow(ctx context.Context, workflowID string) error

	// ReleaseWorkflow returns a workflow to the ready queue.
	// Used when a workflow suspends (sleep/await signals).
	ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error

	// ContinueAsNew atomically creates a new workflow run AND completes the
	// current one in a single database transaction.  If the transaction fails
	// neither operation takes effect.  Returns the new run ID on success.
	ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (newRunID string, err error)

	// FinalizeWorkflowSegment atomically appends new events and updates the
	// workflow status in a single database transaction.  finalStatus is one of
	// "done", "failed" or "ready" (suspend).  Fields not relevant to the chosen
	// status are ignored.  If the transaction fails neither events nor status
	// are written.
	FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error

	// RequestCancellation sets the cancellation flag on a workflow.
	RequestCancellation(ctx context.Context, workflowID, reason string) error

	// CheckCancellation checks if a workflow has been cancelled.
	CheckCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error)

	// DeliverSignal stores a signal for a workflow.
	DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error

	// PollSignal checks for a delivered signal.
	PollSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error)

	// PollCancellation checks whether the workflow has been cancelled.
	PollCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error)

	// PollAndClaimSignal atomically checks for and claims a pending signal.
	PollAndClaimSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error)

	// StartNewRun creates a new workflow instance.
	// If idempotencyKey is non-empty, provides exactly-once semantics: a
	// subsequent call with the same key returns the existing workflow ID
	// without creating a duplicate.
	// tenantID must be a valid UUID; the all-zeros default is accepted
	// for single-tenant installations without RLS.
	StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error)

	// StartChildWorkflow creates a child workflow instance linked to a parent.
	// defVersion is the explicit workflow definition version to use, or 0 to use
	// default resolution (SELECT MAX(version)).
	StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (runID string, err error)

	// StartChildWorkflowAtomic creates a child workflow and records the parent's
	// child_workflow event in a single transaction, guaranteeing exactly-once creation.
	StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (runID string, err error)

	// GetChildResult checks whether a child workflow has completed and returns its result.
	GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error)

	// ReapStaleInstances reclaims workflow instances that have been running
	// but whose heartbeat has not been updated within the given timeout.
	// Returns the number of instances reclaimed.
	ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error)

	// GetQueryState returns the query state for a workflow instance key.
	GetQueryState(ctx context.Context, workflowID, key string) (string, error)

	// ListWorkflows returns workflow instances filtered by the given filter parameters.
	// Supported filters: Status, InputContains, ErrorContains, Search.
	// Supports pagination via Offset and Limit (default 100, max 1000).
	ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error)

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
	TraceWorkflow(ctx context.Context, workflowID, traceID string) error

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

	// ResolveLatestVersion resolves the latest version for a named definition.
	ResolveLatestVersion(ctx context.Context, defName string) (int, error)

	// ValidateVersion checks whether the given version is valid (exists and not deprecated).
	ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error)

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

	// DeleteExpiredEvents deletes event history rows for workflows that are in a
	// terminal state (completed/failed) and whose last update is older than the
	// cutoff time.  It also deletes associated compaction states.
	// Returns the number of event rows deleted.
	DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error)

	// ResolveTenantFromAPIKey looks up a tenant UUID by API key hash.
	// Returns uuid.Nil if the key is not found or revoked.
	ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error)

	// TerminateWorkflow force-terminates a workflow, setting status to
	// 'terminated'. Unlike FailWorkflow, this does not require the worker
	// to own the workflow. Use sparingly — it leaves the workflow in an
	// indeterminate state and should only be used when a workflow is truly stuck.
	TerminateWorkflow(ctx context.Context, workflowID, reason string) error

	// DeleteDeadLetteredWorkflows permanently deletes workflow instances that are
	// in the dead_lettered state and whose completed_at is older than the cutoff.
	// Child rows (event_history, signals, promises, concurrency_keys, update_requests)
	// are deleted automatically via ON DELETE CASCADE. Returns the number of
	// workflow instances deleted.
	DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error)

	// StreamEventHistory loads event history for a workflow in pages, returning
	// events through a channel. Events are fetched in pages of pageSize as the
	// caller reads from the channel. The channel is closed when all events have
	// been sent. The context can be used to cancel the stream mid-way.
	StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan EventRecord, <-chan error)

	// GetChildCount returns the number of active (non-terminal) child workflows
	// for the given parent workflow. This is used for per-workflow child quota
	// enforcement. Terminal statuses ('done', 'failed', 'dead_lettered')
	// are excluded from the count.
	GetChildCount(ctx context.Context, parentWorkflowID string) (int, error)

	// GetConcurrencyKeyCount returns the number of non-expired concurrency keys
	// held by the given workflow. This is used for per-workflow concurrency key
	// quota enforcement. Keys whose expires_at is in the past are excluded.
	GetConcurrencyKeyCount(ctx context.Context, workflowID string) (int, error)

	// GetEventCount returns the event_count for a workflow instance.
	// Used by the engine for auto-ContinueAsNew when the event cap is hit.
	GetEventCount(ctx context.Context, workflowID string) (int, error)

	// GetAllowedSignalCallers returns the allowed_signals list for a workflow.
	// Returns nil when allowed_signals is NULL or empty (deny-all semantics).
	GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error)
}

// DefaultTenantUUID is the all-zeros UUID used when no tenant is specified.
const DefaultTenantUUID = "00000000-0000-0000-0000-000000000000"

// PostgresStore implements WorkflowStore using a PostgreSQL database.
type PostgresStore struct {
	db                *sql.DB
	taskQueues        []string
	tenantID          string
	dialect           Dialect
	idempotencyKeyTTL time.Duration

	// Encryption at rest for sensitive event payloads.
	encryption *PayloadEncryption
	// NOTE: encryption currently applies only to the per-event write path
	// (flushEvent). The batch write path (appendEventsInTx) stores events
	// in plaintext; adding encryption there would double-encrypt events
	// that flow through both paths. Until the paths are unified or
	// exclusive, full coverage requires routing all events through the per-event path.
	encryptSensitivePayloads bool

	// disableReadRedaction when true bypasses RedactOnRead on the read path.
	// Set to true during replay to avoid the overhead of retroactive redaction.
	disableReadRedaction bool
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
		db:                db,
		taskQueues:        tqs,
		tenantID:          "00000000-0000-0000-0000-000000000000",
		dialect:           DialectPostgres,
		idempotencyKeyTTL: 720 * time.Hour,
	}
}

// WithIdempotencyKeyTTL returns a copy of the store with the given idempotency key TTL.
func (s *PostgresStore) WithIdempotencyKeyTTL(ttl time.Duration) *PostgresStore {
	cp := *s
	cp.idempotencyKeyTTL = ttl
	return &cp
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

// WithEncryption returns a copy of the store with encryption at rest enabled.
func (s *PostgresStore) WithEncryption(enc *PayloadEncryption, enabled bool) *PostgresStore {
	cp := *s
	cp.encryption = enc
	cp.encryptSensitivePayloads = enabled
	return &cp
}

// WithReadRedactionDisabled returns a copy of the store with redaction on
// the read path disabled. Used during replay to avoid overhead.
func (s *PostgresStore) WithReadRedactionDisabled(disabled bool) *PostgresStore {
	cp := *s
	cp.disableReadRedaction = disabled
	return &cp
}

// decryptAndRedactEventRecord decrypts sensitive event record fields (when
// encryption is enabled) and applies retroactive redaction. Decryption errors
// are logged and the field is set to "[DECRYPTION_FAILED]" so it is clear the
// data is unreadable rather than silently keeping ciphertext.
func (s *PostgresStore) decryptAndRedactEventRecord(rec *EventRecord, workflowID string) {
	if s.encryption != nil && s.encryptSensitivePayloads {
		// Request and Response are base64-decoded by tryDecodeBase64,
		// so they hold raw ciphertext bytes and must be decrypted via Decrypt.
		if decrypted, err := s.encryption.Decrypt([]byte(rec.Request)); err == nil {
			rec.Request = string(decrypted)
		} else {
			log.Printf("[store] decrypt Request failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.Request = "[DECRYPTION_FAILED]"
		}
		if decrypted, err := s.encryption.Decrypt([]byte(rec.Response)); err == nil {
			rec.Response = string(decrypted)
		} else {
			log.Printf("[store] decrypt Response failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.Response = "[DECRYPTION_FAILED]"
		}
		// Err, SignalPayload, ChildInput, NewInput, PluginInput, PluginOutput,
		// PromiseResult, PromiseError are stored as base64-encoded ciphertexts
		// (no extra base64 layer), so DecryptString is correct.
		if decrypted, err := s.encryption.DecryptString(rec.Err); err == nil {
			rec.Err = decrypted
		} else {
			log.Printf("[store] decrypt Err failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.Err = "[DECRYPTION_FAILED]"
		}
		if decrypted, err := s.encryption.DecryptString(rec.SignalPayload); err == nil {
			rec.SignalPayload = decrypted
		} else {
			log.Printf("[store] decrypt SignalPayload failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.SignalPayload = "[DECRYPTION_FAILED]"
		}
		if decrypted, err := s.encryption.DecryptString(rec.ChildInput); err == nil {
			rec.ChildInput = decrypted
		} else {
			log.Printf("[store] decrypt ChildInput failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.ChildInput = "[DECRYPTION_FAILED]"
		}
		if decrypted, err := s.encryption.DecryptString(rec.NewInput); err == nil {
			rec.NewInput = decrypted
		} else {
			log.Printf("[store] decrypt NewInput failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.NewInput = "[DECRYPTION_FAILED]"
		}
		if decrypted, err := s.encryption.DecryptString(rec.PluginInput); err == nil {
			rec.PluginInput = decrypted
		} else {
			log.Printf("[store] decrypt PluginInput failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.PluginInput = "[DECRYPTION_FAILED]"
		}
		if decrypted, err := s.encryption.DecryptString(rec.PluginOutput); err == nil {
			rec.PluginOutput = decrypted
		} else {
			log.Printf("[store] decrypt PluginOutput failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.PluginOutput = "[DECRYPTION_FAILED]"
		}
		if decrypted, err := s.encryption.DecryptString(rec.PromiseResult); err == nil {
			rec.PromiseResult = decrypted
		} else {
			log.Printf("[store] decrypt PromiseResult failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.PromiseResult = "[DECRYPTION_FAILED]"
		}
		if decrypted, err := s.encryption.DecryptString(rec.PromiseError); err == nil {
			rec.PromiseError = decrypted
		} else {
			log.Printf("[store] decrypt PromiseError failed for workflow %s step %d: %v", workflowID, rec.Step, err)
			decryptionErrorsTotal.Inc()
			rec.PromiseError = "[DECRYPTION_FAILED]"
		}
	}

	// Retroactive redaction on read path.
	if !s.disableReadRedaction {
		rec.Request = RedactOnRead(rec.Request)
		rec.Response = RedactOnRead(rec.Response)
		rec.Err = RedactOnRead(rec.Err)
		rec.SignalPayload = RedactOnRead(rec.SignalPayload)
		rec.ChildInput = RedactOnRead(rec.ChildInput)
		rec.NewInput = RedactOnRead(rec.NewInput)
		rec.PluginInput = RedactOnRead(rec.PluginInput)
		rec.PluginOutput = RedactOnRead(rec.PluginOutput)
		rec.PromiseResult = RedactOnRead(rec.PromiseResult)
		rec.PromiseError = RedactOnRead(rec.PromiseError)
	}
}

// decryptPayloadJSON decrypts the payload JSONB column if encryption is
// enabled and returns the decrypted (or original) payload string.
func (s *PostgresStore) decryptPayloadJSON(payloadStr string) string {
	if s.encryption != nil && s.encryptSensitivePayloads && payloadStr != "" {
		if decrypted, err := s.encryption.DecryptJSON([]byte(payloadStr)); err == nil {
			return string(decrypted)
		} else {
			log.Printf("[store] decrypt payload JSON failed: %v", err)
			decryptionErrorsTotal.Inc()
		}
	}
	return payloadStr
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
	defer tx.Rollback()

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
			ORDER BY CASE WHEN sticky_worker_id = $1 THEN 0 ELSE 1 END, priority ASC, created_at
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
		tx.Rollback()
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
	defer tx.Rollback()

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
		tx.Rollback()
		return nil, nil
	}
	return wfs, tx.Commit()
}

// LoadEventHistory returns all event records for a workflow, ordered by step.
func (s *PostgresStore) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load history: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error,
		       EXTRACT(EPOCH FROM created_at)::BIGINT * 1000 AS timestamp_ms,
		       created_at
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
		var createdAt sql.NullTime

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError,
			&rec.TimestampMs, &createdAt); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}

		if createdAt.Valid {
			rec.CreatedAt = createdAt.Time
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

		// Decrypt and redact event record.
		s.decryptAndRedactEventRecord(&rec, workflowID)

		// Retroactive redaction on read path: ensure sensitive fields are
		// redacted even if they were stored before redaction was mandatory.
		// Redaction runs AFTER decryption (see block above) since redacting
		// ciphertext would yield meaningless "[REDACTED]" placeholders.
		if !s.disableReadRedaction {
			rec.Request = RedactOnRead(rec.Request)
			rec.Response = RedactOnRead(rec.Response)
			rec.Err = RedactOnRead(rec.Err)
			rec.SignalPayload = RedactOnRead(rec.SignalPayload)
			rec.ChildInput = RedactOnRead(rec.ChildInput)
			rec.NewInput = RedactOnRead(rec.NewInput)
			rec.PluginInput = RedactOnRead(rec.PluginInput)
			rec.PluginOutput = RedactOnRead(rec.PluginOutput)
			rec.PromiseResult = RedactOnRead(rec.PromiseResult)
			rec.PromiseError = RedactOnRead(rec.PromiseError)
		}

		if payload.Valid {
			payloadStr := s.decryptPayloadJSON(payload.String)
			populateFromPayload(&rec, []byte(payloadStr))
		}

		history = append(history, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return history, tx.Commit()
}

// LoadEventHistoryPaginated returns a page of event history for a workflow,
func (s *PostgresStore) LoadEventHistoryPaginated(ctx context.Context, workflowID string, offset, limit int) ([]EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load history paginated: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT step, event_type, service, operation, request, response, error,
		       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
		       defer_description, defer_id, child_name, child_input, run_id, new_input,
		       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
		       payload,
		       promise_name, promise_id, promise_result, promise_error,
		       created_at
		FROM event_history
		WHERE workflow_id = $1
		ORDER BY step
		OFFSET $2 LIMIT $3
	`, workflowID, offset, limit)
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
		var createdAt sql.NullTime

		if err := rows.Scan(&rec.Step, &rec.EventType,
			&service, &op, &request, &response, &errMsg,
			&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
			&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
			&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
			&payload,
			&promiseName, &promiseID, &promiseResult, &promiseError,
			&createdAt); err != nil {
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
		if createdAt.Valid {
			rec.CreatedAt = createdAt.Time
		}

		// Decrypt and redact event record.
		s.decryptAndRedactEventRecord(&rec, workflowID)

		// Retroactive redaction on read path.
		// Redaction runs AFTER decryption (see block above) since redacting
		// ciphertext would yield meaningless "[REDACTED]" placeholders.
		if !s.disableReadRedaction {
			rec.Request = RedactOnRead(rec.Request)
			rec.Response = RedactOnRead(rec.Response)
			rec.Err = RedactOnRead(rec.Err)
			rec.SignalPayload = RedactOnRead(rec.SignalPayload)
			rec.ChildInput = RedactOnRead(rec.ChildInput)
			rec.NewInput = RedactOnRead(rec.NewInput)
			rec.PluginInput = RedactOnRead(rec.PluginInput)
			rec.PluginOutput = RedactOnRead(rec.PluginOutput)
			rec.PromiseResult = RedactOnRead(rec.PromiseResult)
			rec.PromiseError = RedactOnRead(rec.PromiseError)
		}
		if payload.Valid {
			payloadStr := s.decryptPayloadJSON(payload.String)
			populateFromPayload(&rec, []byte(payloadStr))
		}

		history = append(history, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return history, tx.Commit()
}

// StreamEventHistory loads event history for a workflow in pages, returning
// events through a channel. This is the streaming counterpart to LoadEventHistory:
// instead of loading all events into memory at once, events are fetched in
// pages of pageSize as the caller reads from the channel.
//
// Example:
//
//	events, errs := store.StreamEventHistory(ctx, workflowID, 1000)
//	for ev := range events {
//	    process(ev)
//	}
//	if err := <-errs; err != nil {
//	    // handle error
//	}
func (s *PostgresStore) StreamEventHistory(ctx context.Context, workflowID string, pageSize int) (<-chan EventRecord, <-chan error) {
	eventCh := make(chan EventRecord, pageSize)
	errCh := make(chan error, 1)

	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 1000
	}

	go func() {
		defer close(eventCh)
		defer close(errCh)

		offset := 0
		for {
			if ctx.Err() != nil {
				errCh <- ctx.Err()
				return
			}

			rows, err := s.db.QueryContext(ctx, `
				SELECT step, event_type, service, operation, request, response, error,
				       duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
				       defer_description, defer_id, child_name, child_input, run_id, new_input,
				       plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
				       payload,
				       promise_name, promise_id, promise_result, promise_error,
				       created_at,
				       EXTRACT(EPOCH FROM created_at)::BIGINT * 1000 AS timestamp_ms
				FROM event_history
				WHERE workflow_id = $1
				ORDER BY step
				LIMIT $2 OFFSET $3
			`, workflowID, pageSize, offset)
			if err != nil {
				errCh <- err
				return
			}

			var pageCount int
			for rows.Next() {
				pageCount++
				var rec EventRecord
				var service, op, request, response, errMsg sql.NullString
				var durationMs, timeoutMs sql.NullInt64
				var signalNames, signalName, signalPayload sql.NullString
				var deferDesc, deferID sql.NullString
				var childName, childInput, runID, newInput sql.NullString
				var pluginName, pluginFunc, pluginInput, pluginOutput, pluginErr sql.NullString
				var payload sql.NullString
				var promiseName, promiseID, promiseResult, promiseError sql.NullString
				var createdAt sql.NullTime

				if err := rows.Scan(&rec.Step, &rec.EventType,
					&service, &op, &request, &response, &errMsg,
					&durationMs, &signalNames, &timeoutMs, &signalName, &signalPayload,
					&deferDesc, &deferID, &childName, &childInput, &runID, &newInput,
					&pluginName, &pluginFunc, &pluginInput, &pluginOutput, &pluginErr,
					&payload,
					&promiseName, &promiseID, &promiseResult, &promiseError,
					&createdAt, &rec.TimestampMs); err != nil {
					rows.Close()
					errCh <- err
					return
				}
				if createdAt.Valid {
					rec.CreatedAt = createdAt.Time
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

				// Decrypt and redact event record.
				s.decryptAndRedactEventRecord(&rec, workflowID)

				if payload.Valid {
					payloadStr := s.decryptPayloadJSON(payload.String)
					populateFromPayload(&rec, []byte(payloadStr))
				}

				select {
				case eventCh <- rec:
				case <-ctx.Done():
					rows.Close()
					errCh <- ctx.Err()
					return
				}
			}
			rows.Close()

			if err := rows.Err(); err != nil {
				errCh <- err
				return
			}

			// If we got fewer rows than the page size, we're done.
			if pageCount < pageSize {
				return
			}
			offset += pageSize
		}
	}()

	return eventCh, errCh
}

// CountEventHistory returns the total number of events for a workflow.
func (s *PostgresStore) CountEventHistory(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count event history: begin: %w", err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_history WHERE workflow_id = $1`, workflowID).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, tx.Commit()
}

// AppendEventHistoryBatch appends multiple events to the history.
// Computes a SHA-256 checksum of each event's payload for data integrity
// verification. The checksum is stored in the checksum column; if the column
// does not exist yet (pre-migration), the checksum is computed but not stored.
//
// Required migration:
//
//	ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT;
func (s *PostgresStore) AppendEventHistoryBatch(ctx context.Context, workflowID string, recs []EventRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append history batch: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return fmt.Errorf("append history batch: set rls: %w", err)
	}

	if err := s.appendEventsInTx(ctx, tx, workflowID, recs); err != nil {
		return err
	}
	return tx.Commit()
}

// appendEventsInTx inserts event records using an already-open transaction.
// This is shared by AppendEventHistoryBatch and FinalizeWorkflowSegment so
// that both can insert events atomically alongside other operations.
func (s *PostgresStore) appendEventsInTx(ctx context.Context, tx *sql.Tx, workflowID string, recs []EventRecord) error {
	if len(recs) == 0 {
		return nil
	}

	// Compute SHA-256 checksum for each event and include it in the INSERT.
	// Requires migration 011: ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT;
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
			duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
			defer_description, defer_id, child_name, child_input, run_id, new_input,
			plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
			promise_name, promise_id, promise_result, promise_error, payload,
			created_at, checksum, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32)
		ON CONFLICT (workflow_id, step) DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error WHERE event_history.response = '' AND event_history.error IS NULL
	`)
	if err != nil {
		return fmt.Errorf("append events in tx: prepare: %w", err)
	}
	defer stmt.Close()

	var prevChecksum string
	for _, rec := range recs {
		payload, err := eventRecordToPayload(rec)
		payloadArg := nullStr("")
		if err == nil && len(payload) > 0 {
			payloadArg = sql.NullString{String: string(payload), Valid: true}
		}
		checksum := computeEventChecksum(rec, prevChecksum)
		prevChecksum = checksum
		_, err = stmt.ExecContext(ctx, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Request))), nullStr(base64.StdEncoding.EncodeToString([]byte(rec.Response))), nullStr(rec.Err),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(rec.SignalPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(rec.ChildInput), nullStr(rec.RunID), nullStr(rec.NewInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(rec.PluginInput), nullStr(rec.PluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(rec.PromiseResult), nullStr(rec.PromiseError),
			payloadArg,
			time.UnixMilli(rec.TimestampMs),
			checksum, s.tenantID)
		if err != nil {
			return fmt.Errorf("append events in tx: exec step %d: %w", rec.Step, err)
		}
	}
	// Increment event_count on workflow_instances so quota enforcement
	// has an up-to-date count.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_instances SET event_count = event_count + $1 WHERE id = $2`,
		len(recs), workflowID); err != nil {
		return fmt.Errorf("append events in tx: increment event_count: %w", err)
	}

	return nil
}

// ContinueAsNew atomically creates a new workflow run, appends events, and
// completes the current run in a single database transaction.  If the
// transaction fails, neither events, the new run, nor the completion takes
// effect.  Returns the new run ID on success.
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
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
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
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = $3, completed_at = now(), assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2 AND generation = $5
	`, currentRunID, workerID, result, qsJSON, generation)
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

	// Update workflow status based on finalStatus.
	switch finalStatus {
	case "done":
		qsJSON, _ := json.Marshal(queryState)
		if qsJSON == nil {
			qsJSON = []byte("{}")
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'done', result = $3, completed_at = now(), assigned_to = NULL, query_state = $4
			WHERE id = $1 AND assigned_to = $2 AND generation = $5
		`, runID, workerID, result, qsJSON, generation)
	case "failed":
		qsJSON, _ := json.Marshal(queryState)
		if qsJSON == nil {
			qsJSON = []byte("{}")
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'failed',
			    error_msg = $3,
			    error_code = $4,
			    error_op = $5,
			    completed_at = now(),
			    assigned_to = NULL,
			    query_state = $6
			WHERE id = $1 AND assigned_to = $2 AND generation = $7
		`, runID, workerID, result, errorCode, errorOp, qsJSON, generation)
	case "ready":
		_, err = tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET status = 'ready', assigned_to = NULL, next_wake_at = $3
			WHERE id = $1 AND assigned_to = $2 AND generation = $4
		`, runID, workerID, nextWakeAt, generation)
	default:
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}
	if err != nil {
		return fmt.Errorf("finalize workflow: update status: %w", err)
	}

	// Record idempotency outcome within the transaction (best-effort).
	if finalStatus == "done" || finalStatus == "failed" {
		switch finalStatus {
		case "done":
			if _, err := tx.ExecContext(ctx,
				`UPDATE idempotency_keys SET result = $2 WHERE workflow_id = $1`,
				runID, result); err != nil {
				log.Printf("idempotency update failed (non-fatal): %v", err)
			}
		case "failed":
			if _, err := tx.ExecContext(ctx,
				`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1`,
				runID, result); err != nil {
				log.Printf("idempotency update failed (non-fatal): %v", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup for terminal statuses (post-commit).
	if finalStatus == "done" || finalStatus == "failed" {
		s.ClearStickyWorker(context.Background(), runID)
		s.ReleaseWorkflowConcurrencyKeys(context.Background(), runID)
		s.enforceParentClosePolicy(context.Background(), runID)
		// Wake the parent workflow immediately so it can pick up the child's result.
		s.wakeParent(context.Background(), runID)
	}

	return nil
}

// AppendEventHistory appends a single event to the history.
func (s *PostgresStore) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	return s.AppendEventHistoryBatch(ctx, workflowID, []EventRecord{rec})
}

// LoadWASM returns the WASM bytes for a workflow definition.
func (s *PostgresStore) LoadWASM(ctx context.Context, defName string, defVersion int) ([]byte, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load wasm: begin: %w", err)
	}
	defer tx.Rollback()

	var wasmBytes []byte
	err = tx.QueryRowContext(ctx, `
		SELECT wasm_bytes FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&wasmBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("wasm not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load wasm: %w", err)
	}
	return wasmBytes, tx.Commit()
}

// GetWASMLength returns the byte length of the stored WASM binary.
func (s *PostgresStore) GetWASMLength(ctx context.Context, defName string, defVersion int) (int64, error) {
	var length int64
	err := s.db.QueryRowContext(ctx, `SELECT length(wasm_bytes) FROM workflow_defs WHERE name = $1 AND version = $2`, defName, defVersion).Scan(&length)
	return length, err
}

// TraceWorkflow sets the W3C trace_id on a workflow instance.
func (s *PostgresStore) TraceWorkflow(ctx context.Context, workflowID, traceID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("trace workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET trace_id = $2 WHERE id = $1
	`, workflowID, traceID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveTenantFromAPIKey looks up a tenant UUID by API key hash.
func (s *PostgresStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id FROM tenant_api_keys
		 WHERE key_hash = $1 AND revoked_at IS NULL`, keyHash).Scan(&tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// LoadWorkflowConfig returns configuration for a workflow definition.
func (s *PostgresStore) LoadWorkflowConfig(ctx context.Context, defName string, defVersion int) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("load workflow config: begin: %w", err)
	}
	defer tx.Rollback()

	var maxHistoryLength int
	err = tx.QueryRowContext(ctx, `
		SELECT max_history_length FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&maxHistoryLength)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return 0, fmt.Errorf("load workflow config: %w", err)
	}
	return maxHistoryLength, tx.Commit()
}

// LoadDAGSpec returns the dag_spec JSON for a workflow definition, or nil if none.
func (s *PostgresStore) LoadDAGSpec(ctx context.Context, defName string, defVersion int) (json.RawMessage, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load dag_spec: begin: %w", err)
	}
	defer tx.Rollback()

	var spec json.RawMessage
	err = tx.QueryRowContext(ctx, `
		SELECT dag_spec FROM workflow_defs WHERE name = $1 AND version = $2
	`, defName, defVersion).Scan(&spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("workflow def not found: %s v%d", defName, defVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("load dag_spec: %w", err)
	}
	return spec, tx.Commit()
}

// ListVersions returns all deployed versions of a workflow.
func (s *PostgresStore) ListVersions(ctx context.Context, defName string) ([]int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list versions: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return versions, tx.Commit()
}

// DeployWorkflowDef inserts or updates a workflow definition.
func (s *PostgresStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("deploy workflow def: begin: %w", err)
	}
	defer tx.Rollback()

	pluginDepsJSON, _ := json.Marshal(def.PluginDeps)
	if pluginDepsJSON == nil {
		pluginDepsJSON = []byte("{}")
	}
	tenantID := "00000000-0000-0000-0000-000000000000"
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, plugin_deps, deprecated, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (name, version) DO UPDATE SET
			wasm_bytes = EXCLUDED.wasm_bytes,
			abi_version = EXCLUDED.abi_version,
			min_version = EXCLUDED.min_version,
			plugin_deps = EXCLUDED.plugin_deps,
			deprecated = EXCLUDED.deprecated
	`, def.Name, def.Version, def.WASMBytes, def.ABIVersion, def.MinVersion, pluginDepsJSON, def.Deprecated, tenantID)
	if err != nil {
		return fmt.Errorf("deploy workflow def: %w", err)
	}
	return tx.Commit()
}

// ListWorkflowDefs returns all versions of a workflow, ordered by version DESC.
// If name is empty, returns all workflow definitions across all workflows.
func (s *PostgresStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workflow defs: begin: %w", err)
	}
	defer tx.Rollback()

	var rows *sql.Rows
	if name == "" {
		rows, err = tx.QueryContext(ctx, `
			SELECT name, version, abi_version, min_version, plugin_deps, created_at, deprecated
			FROM workflow_defs ORDER BY name, version DESC
		`)
	} else {
		rows, err = tx.QueryContext(ctx, `
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return defs, tx.Commit()
}

// GetWorkflowDef returns a single workflow definition by name and version.
func (s *PostgresStore) GetWorkflowDef(ctx context.Context, name string, version int) (*WorkflowDef, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get workflow def: begin: %w", err)
	}
	defer tx.Rollback()

	var def WorkflowDef
	var pluginDepsRaw []byte
	var wasmBytes []byte
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT name, version, wasm_bytes, abi_version, min_version, plugin_deps, created_at, deprecated
		FROM workflow_defs WHERE name = $1 AND version = $2
	`, name, version).Scan(&def.Name, &def.Version, &wasmBytes, &def.ABIVersion,
		&def.MinVersion, &pluginDepsRaw, &createdAt, &def.Deprecated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
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
	return &def, tx.Commit()
}

// MarkVersionDeprecated sets the deprecated flag on a workflow version.
func (s *PostgresStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("mark version deprecated: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_defs SET deprecated = $3 WHERE name = $1 AND version = $2
	`, name, version, deprecated)
	if err != nil {
		return fmt.Errorf("mark version deprecated: %w", err)
	}
	return tx.Commit()
}

// PurgeWorkflowDef permanently deletes a workflow definition.
func (s *PostgresStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("purge workflow def: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		DELETE FROM workflow_defs WHERE name = $1 AND version = $2
	`, name, version)
	if err != nil {
		return fmt.Errorf("purge workflow def: %w", err)
	}
	return tx.Commit()
}

// CountActiveInstances returns the number of ready or running instances for a version.
func (s *PostgresStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count active instances: begin: %w", err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE def_name = $1 AND def_version = $2
		  AND status IN ('ready', 'running')
	`, name, version).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active instances: %w", err)
	}
	return count, tx.Commit()
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

// Heartbeat updates the heartbeat timestamp.
func (s *PostgresStore) ResolveLatestVersion(ctx context.Context, defName string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve latest version: begin: %w", err)
	}
	defer tx.Rollback()

	var version int
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM workflow_defs
		WHERE name = $1 AND NOT deprecated
	`, defName).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("resolve latest version: %w", err)
	}
	return version, tx.Commit()
}

// ValidateVersion checks whether a specific workflow definition version
// exists and is not deprecated. Returns true if the version can be used.
//
//	SQL: SELECT EXISTS(SELECT 1 FROM workflow_defs
//	     WHERE name = $1 AND version = $2 AND NOT deprecated)
func (s *PostgresStore) ValidateVersion(ctx context.Context, defName string, defVersion int) (bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, fmt.Errorf("validate version: begin: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM workflow_defs
			WHERE name = $1 AND version = $2 AND NOT deprecated
		)
	`, defName, defVersion).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("validate version: %w", err)
	}
	return exists, tx.Commit()
}

// Heartbeat updates the heartbeat timestamp.
func (s *PostgresStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, fmt.Errorf("heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = now()
		WHERE id = $1 AND assigned_to = $2 AND generation = $3
	`, workflowID, workerID, generation)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, tx.Commit()
}

// BatchHeartbeat updates heartbeat_at for all workflows assigned to this worker.
// NOTE: This intentionally does NOT check per-workflow generation because it
// operates on ALL running workflows for a worker, and generations differ per
// workflow. Individual generation-guarded operations (Heartbeat,
// CompleteWorkflow, FailWorkflow, etc.) prevent double-execution even if the
// batch heartbeat refreshes a stale workflow's heartbeat_at.
func (s *PostgresStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("batch heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = now()
		WHERE assigned_to = $1 AND status = 'running'
	`, workerID)
	if err != nil {
		return 0, fmt.Errorf("batch heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, tx.Commit()
}

// CompleteWorkflow marks a workflow as done.
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
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = $3, completed_at = now(), assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2 AND generation = $5
	`, workflowID, workerID, result, qsJSON, generation)
	if err != nil {
		return err
	}

	// Record idempotency result within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET result = $2 WHERE workflow_id = $1`,
		workflowID, result); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
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
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed',
		    error_msg = $3,
		    error_code = $4,
		    error_op = $5,
		    completed_at = now(),
		    assigned_to = NULL,
		    query_state = $6
		WHERE id = $1 AND assigned_to = $2 AND generation = $7
	`, workflowID, workerID, errorMsg, errorCode, errorOp, qsJSON, generation)
	if err != nil {
		return err
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1`,
		workflowID, errorMsg); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
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
// of the given parent workflow. Runs as a best-effort operation.
//
// Note: This runs without RLS transaction context because it is called with
// context.Background() from post-commit cleanup. For non-default tenants, the
// RLS default will filter to the default tenant only. This is acceptable
// because the operation is best-effort and child workflows typically share
// the parent's tenant (inherited at creation).
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

// wakeParent sets the parent workflow's next_wake_at to now so it immediately
// detects child completion. Runs as a best-effort post-commit operation.
func (s *PostgresStore) wakeParent(ctx context.Context, childID string) {
	s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET next_wake_at = now()
		WHERE id = (
			SELECT parent_workflow_id FROM workflow_instances WHERE id = $1
		)
		AND status = 'ready'
	`, childID)
}

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts.
func (s *PostgresStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = $3, error_code = $4, error_op = $5,
		    completed_at = now(), assigned_to = NULL
		WHERE id = $1 AND assigned_to = $2 AND generation = $6
	`, workflowID, workerID, errMsg, errorCode, errorOp, generation)
	if err != nil {
		return err
	}
	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1`,
		workflowID, errMsg); err != nil {
		log.Printf("idempotency update failed (non-fatal): %v", err)
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
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, "", fmt.Errorf("check cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	var cancelled bool
	var reason sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = $1
	`, workflowID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, tx.Commit()
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
	if errors.Is(err, sql.ErrNoRows) {
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
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
			VALUES ($1, $2, $3, 'ready', $4,
			        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3), 'default'),
			$5, $6)
		`, runID, defName, defVersion, input, tenantID, priority)
		if err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		return runID, false, tx.Commit()
	}

	// No idempotency key — normal flow.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES ($1, $2, $3, 'ready', $4,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3), 'default'),
			$5, $6)
	`, runID, defName, defVersion, input, tenantID, priority)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	return runID, false, tx.Commit()
}

// StartChildWorkflow creates a child workflow instance linked to a parent.
// The child is created with its own independent workflow instance.
// If defVersion > 0, that version is used explicitly; otherwise the latest
// non-deprecated version is used (SELECT MAX(version)).
func (s *PostgresStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("start child workflow: begin: %w", err)
	}
	defer tx.Rollback()

	var runID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES (gen_random_uuid(), $1,
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated) END,
		        'ready', $2, $3,
		        COALESCE(NULLIF($5, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $3), 'default'),
			$6, $7)
		RETURNING id
	`, defName, inputJSON, parentID, defVersion, parentClosePolicy, s.tenantID, priority).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}
	return runID, tx.Commit()
}

// StartChildWorkflowAtomic creates a child workflow and records the parent's
// child_workflow event in a single transaction, guaranteeing exactly-once creation.
func (s *PostgresStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	if childID == "" {
		childID = uuid.New().String()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return "", fmt.Errorf("start child workflow atomic: set rls: %w", err)
	}

	// Debug: check what MAX(version) resolves to.
	var resolvedVersion int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated), -1)`,
		defName).Scan(&resolvedVersion); err != nil {
		resolvedVersion = -2
	}
	log.Printf("[engine] StartChildWorkflowAtomic: defName=%q defVersion=%d resolvedVersion=%d tenantID=%s parentID=%s",
		defName, defVersion, resolvedVersion, s.tenantID, parentID)

	// 1. INSERT child workflow instance.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES ($1, $2,
		        CASE WHEN $5 > 0 THEN $5 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $2 AND NOT deprecated) END,
		        'ready', $3, $4,
		        COALESCE(NULLIF($6, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $4), 'default'),
			$7, $8)
	`, childID, defName, inputJSON, parentID, defVersion, parentClosePolicy, s.tenantID, priority)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert child: %w", err)
	}

	// 2. INSERT child_workflow event into the parent's event_history.
	event.RunID = childID
	var prevCS string
	if event.Step > 1 {
		s.db.QueryRowContext(ctx,
			`SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
			parentID, event.Step-1).Scan(&prevCS)
	}
	checksum := computeEventChecksum(event, prevCS)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow atomic: commit: %w", err)
	}
	return childID, nil
}

// GetChildResult checks whether a child workflow has completed (status 'done' or 'failed').
func (s *PostgresStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", false, fmt.Errorf("get child result: begin: %w", err)
	}
	defer tx.Rollback()

	var result string
	var status string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(result, '{}'), status FROM workflow_instances WHERE id = $1
	`, runID).Scan(&result, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, tx.Commit()
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result: %w", err)
	}
	if status == "done" || status == "failed" {
		return result, true, tx.Commit()
	}
	return "", false, tx.Commit()
}

// GetChildCount returns the number of active (non-terminal) child workflows
// for the given parent workflow. Terminal statuses are excluded.
func (s *PostgresStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: begin: %w", parentWorkflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE parent_workflow_id = $1 AND status NOT IN ('done', 'failed', 'dead_lettered')
	`, parentWorkflowID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: %w", parentWorkflowID, err)
	}
	return count, tx.Commit()
}

// StartChildWorkflowInSchema creates a child workflow in the given target schema.
// Implements CrossSchemaChildStore for cross-instance workflow cooperation.
func (s *PostgresStore) StartChildWorkflowInSchema(ctx context.Context, targetSchema, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	var runID string
	q := fmt.Sprintf(`
		INSERT INTO %s.workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, priority)
		VALUES (gen_random_uuid(), $1,
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM %s.workflow_defs WHERE name = $1 AND NOT deprecated) END,
		        'ready', $2, $3,
		        COALESCE(NULLIF($5, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM %s.workflow_instances WHERE id = $3), 'default'), $6)
		RETURNING id
	`, pq.QuoteIdentifier(targetSchema), pq.QuoteIdentifier(targetSchema), pq.QuoteIdentifier(targetSchema))
	if err := s.db.QueryRowContext(ctx, q, defName, inputJSON, parentID, defVersion, parentClosePolicy, priority).Scan(&runID); err != nil {
		return "", fmt.Errorf("start child workflow in schema %q: %w", targetSchema, err)
	}
	return runID, nil
}

// GetChildResultInSchema polls a child workflow in the given target schema.
func (s *PostgresStore) GetChildResultInSchema(ctx context.Context, targetSchema, runID string) (string, bool, error) {
	var result string
	var status string
	q := fmt.Sprintf(`SELECT COALESCE(result, '{}'), status FROM %s.workflow_instances WHERE id = $1`,
		pq.QuoteIdentifier(targetSchema))
	err := s.db.QueryRowContext(ctx, q, runID).Scan(&result, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result in schema %q: %w", targetSchema, err)
	}
	if status == "done" || status == "failed" {
		return result, true, nil
	}
	return "", false, nil
}

// ReapStaleInstances reclaims workflow instances with stale heartbeats.
func (s *PostgresStore) ReapStaleInstances(ctx context.Context, timeout time.Duration) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
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

// GetAllowedSignalCallers returns the allowed_signals list for a workflow.
// Returns nil when allowed_signals is NULL or the target workflow doesn't exist.
func (s *PostgresStore) GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get allowed signal callers: begin: %w", err)
	}
	defer tx.Rollback()

	var raw sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT allowed_signals FROM workflow_instances WHERE id = $1`, workflowID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, fmt.Errorf("get allowed signal callers: %w", err)
	}
	if !raw.Valid || raw.String == "" || raw.String == "null" {
		return nil, tx.Commit()
	}
	var callers []string
	if err := json.Unmarshal([]byte(raw.String), &callers); err != nil {
		return nil, fmt.Errorf("get allowed signal callers: parse: %w", err)
	}
	return callers, tx.Commit()
}

// GetQueryState returns the value for a key in the workflow's query_state JSONB.
func (s *PostgresStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("get query state: begin: %w", err)
	}
	defer tx.Rollback()

	var value sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT query_state ->> $2 FROM workflow_instances WHERE id = $1
	`, workflowID, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", tx.Commit()
	}
	if err != nil {
		return "", fmt.Errorf("get query state: %w", err)
	}
	return value.String, tx.Commit()
}

// ListWorkflows returns workflow instances filtered by the given filter parameters,
// ordered by creation time DESC. Supports search by input content, error message,
// and combined full-text search, as well as pagination via Offset/Limit.
func (s *PostgresStore) ListWorkflows(ctx context.Context, filter WorkflowFilter) ([]WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workflows: begin: %w", err)
	}
	defer tx.Rollback()

	d := s.dialect
	qb := NewQueryBuilder(d,
		"SELECT "+d.workflowInstanceColumns()+" FROM workflow_instances WHERE 1=1",
	)

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
		qb.AddRaw(fmt.Sprintf("AND (%s OR %s OR %s)",
			d.likeExpr(icol, n, true),
			d.likeExpr(rcol, n+1, true),
			d.likeExpr("error_msg", n+2, true)))
		qb.AddArgs(pattern, pattern, pattern)
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
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()

	var workflows []WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt, createdAt sql.NullTime
		var assignedTo, errorCode, errorOp, errorMsg sql.NullString
		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&assignedTo, &nextWakeAt, &errorCode, &errorOp, &errorMsg, &createdAt, &wf.Generation, &wf.Priority, &wf.TraceID); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.AssignedTo = assignedTo.String
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wf.Error = errorMsg.String
		workflows = append(workflows, wf)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return workflows, tx.Commit()
}

// GetWorkflowByID returns a single workflow instance by ID.
func (s *PostgresStore) GetWorkflowByID(ctx context.Context, id string) (*WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get workflow: begin: %w", err)
	}
	defer tx.Rollback()

	var wf WorkflowInstance
	var nextWakeAt, heartbeatAt, completedAt sql.NullTime
	var assignedTo, errorMsg sql.NullString
	var result sql.NullString
	var errorCode, errorOp sql.NullString
	var inputRaw json.RawMessage

	err = tx.QueryRowContext(ctx, `
		SELECT id, def_name, def_version, status, input,
		       assigned_to, heartbeat_at, next_wake_at, completed_at, result::text, error_msg, error_code, error_op,
		       generation, COALESCE(priority, 0) AS priority,
		       COALESCE(trace_id, '')
		FROM workflow_instances WHERE id = $1
	`, id).Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &inputRaw,
		&assignedTo, &heartbeatAt, &nextWakeAt, &completedAt, &result, &errorMsg, &errorCode, &errorOp,
		&wf.Generation, &wf.Priority,
		&wf.TraceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	wf.Input = inputRaw
	wf.AssignedTo = assignedTo.String
	wf.Result = result.String
	wf.Error = errorMsg.String
	wf.ErrorCode = errorCode.String
	wf.ErrorOp = errorOp.String
	if nextWakeAt.Valid {
		wf.NextWakeAt = nextWakeAt.Time
	}
	return &wf, tx.Commit()
}

// ---- Schedule methods ----

func (s *PostgresStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("create schedule: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, enabled, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, sch.Name, sch.DefName, sch.EntryPoint, sch.CronExpression, sch.Input, sch.Enabled, sch.NextRunAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list schedules: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return schedules, tx.Commit()
}

func (s *PostgresStore) DeleteSchedule(ctx context.Context, name string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("delete schedule: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM workflow_schedules WHERE name = $1`, name)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("set schedule enabled: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_schedules SET enabled = $2 WHERE name = $1
	`, name, enabled)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get due schedules: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return schedules, tx.Commit()
}

func (s *PostgresStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("update schedule next run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_schedules SET next_run_at = $2, last_run_at = now() WHERE name = $1
	`, name, nextRun)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CompactHistory deletes old events and saves compaction state for a workflow.
func (s *PostgresStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("compact history: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		DELETE FROM event_history WHERE workflow_id = $1 AND step < $2
	`, workflowID, keepStep)
	if err != nil {
		return fmt.Errorf("compact history: delete: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET compaction_state = $1, compacted_at = now(), compaction_step = $2
		WHERE id = $3
	`, compactionState, compactionStep, workflowID)
	if err != nil {
		return fmt.Errorf("compact history: update: %w", err)
	}

	return tx.Commit()
}

// GetCompactionCandidates returns workflow IDs that need compaction.
func (s *PostgresStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get compaction candidates: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT w.id
		FROM workflow_instances w
		JOIN (
			SELECT workflow_id, COUNT(*) AS cnt
			FROM event_history
			GROUP BY workflow_id
		) e ON w.id = e.workflow_id
		WHERE e.cnt > $1
		  AND (w.compaction_step IS NULL OR w.compaction_step < e.cnt - $1)
		ORDER BY e.cnt DESC
		LIMIT $2
	`, threshold, limit)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

// LoadCompactionState loads the compaction state JSON for a workflow instance.
func (s *PostgresStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load compaction state: begin: %w", err)
	}
	defer tx.Rollback()

	var rawJSON []byte
	err = tx.QueryRowContext(ctx, `
		SELECT compaction_state FROM workflow_instances
		WHERE id = $1
	`, workflowID).Scan(&rawJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, fmt.Errorf("load compaction state: %w", err)
	}
	if rawJSON == nil {
		return nil, tx.Commit()
	}
	var cs CompactionState
	if err := json.Unmarshal(rawJSON, &cs); err != nil {
		return nil, fmt.Errorf("unmarshal compaction state: %w", err)
	}
	return &cs, tx.Commit()
}

// ---- PromiseStore interface implementation ----

// CreatePromise creates a new promise for a workflow instance.
func (s *PostgresStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("create promise: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, status)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workflow_id, promise_id) DO NOTHING
	`, workflowID, promiseID, promiseName, "pending")
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ResolvePromise marks a promise as resolved with the given result.
// Also wakes the workflow instance so it can pick up the resolved promise
// on the next poll cycle instead of waiting for the original timeout.
func (s *PostgresStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("resolve promise: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_promises SET status = $3, result = $4, resolved_at = now()
		WHERE workflow_id = $1 AND promise_id = $2
	`, workflowID, promiseID, "resolved", result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = now()
		WHERE id = $1 AND status = 'ready'
	`, workflowID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RejectPromise marks a promise as rejected with the given error message.
// Also wakes the workflow instance so it can pick up the rejected promise
// on the next poll cycle instead of waiting for the original timeout.
func (s *PostgresStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("reject promise: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_promises SET status = $3, error_msg = $4, resolved_at = now()
		WHERE workflow_id = $1 AND promise_id = $2
	`, workflowID, promiseID, "rejected", errMsg)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET next_wake_at = now()
		WHERE id = $1 AND status = 'ready'
	`, workflowID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// GetPromise returns the current status and result of a promise.
func (s *PostgresStore) GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("get promise: begin: %w", err)
	}
	defer tx.Rollback()

	var resultStr, errStr sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT status, result::text, error_msg FROM workflow_promises
		WHERE workflow_id = $1 AND promise_id = $2
	`, workflowID, promiseID).Scan(&status, &resultStr, &errStr)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", "", "", tx.Commit()
	}
	if err != nil {
		return "", "", "", err
	}
	return status, resultStr.String, errStr.String, tx.Commit()
}

// ListPromises returns all promises for a workflow ordered by creation time.
func (s *PostgresStore) ListPromises(ctx context.Context, workflowID string) ([]PromiseInfo, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list promises: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT promise_id, promise_name, status, COALESCE(result::text, ''), COALESCE(error_msg, ''), created_at, resolved_at
		FROM workflow_promises
		WHERE workflow_id = $1
		ORDER BY priority ASC, created_at
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return promises, tx.Commit()
}

// ---- Concurrency Key implementations (Feature 5) ----

// AcquireConcurrencyKey tries to acquire a concurrency key for a workflow.
// Returns true if acquired, false if already held by another workflow.
func (s *PostgresStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: begin: %w", err)
	}
	defer tx.Rollback()

	// Delete expired keys for this key hash within the current tenant.
	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE key_hash = digest($1, 'sha256') AND expires_at < now() AND tenant_id = $2`, key, s.tenantID)
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: delete expired: %w", err)
	}

	// Try to insert. ON CONFLICT DO NOTHING means if the key_hash already exists,
	// the RETURNING clause returns no rows.
	var returnedWorkflowID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		VALUES (digest($1, 'sha256'), $1, $2, now() + $3::interval, $4)
		ON CONFLICT (key_hash) DO NOTHING
		RETURNING workflow_id
	`, key, workflowID, fmt.Sprintf("%d seconds", int(ttl.Seconds())), s.tenantID).Scan(&returnedWorkflowID)

	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire concurrency key: %w", err)
	}
	return true, tx.Commit()
}

// ReleaseConcurrencyKey releases a specific concurrency key.
func (s *PostgresStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("release concurrency key: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE key_hash = digest($1, 'sha256') AND tenant_id = $2`, key, s.tenantID)
	if err != nil {
		return fmt.Errorf("release concurrency key: %w", err)
	}
	return tx.Commit()
}

// ReleaseWorkflowConcurrencyKeys releases all concurrency keys held by a workflow.
func (s *PostgresStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE workflow_id = $1 AND tenant_id = $2`, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: %w", err)
	}
	return tx.Commit()
}

// ReapExpiredConcurrencyKeys deletes all expired concurrency keys
// for the current tenant. Returns the number of keys deleted.
func (s *PostgresStore) ReapExpiredConcurrencyKeys(ctx context.Context) (int64, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE expires_at < now() AND tenant_id = $1`, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("reap expired concurrency keys: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, tx.Commit()
}

// GetConcurrencyKeyCount returns the number of non-expired concurrency keys
// held by the given workflow.
func (s *PostgresStore) GetConcurrencyKeyCount(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get concurrency key count for %s: begin: %w", workflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM concurrency_keys
		WHERE workflow_id = $1 AND expires_at > now() AND tenant_id = $2
	`, workflowID, s.tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get concurrency key count for %s: %w", workflowID, err)
	}
	return count, tx.Commit()
}

// GetEventCount returns the event_count for a workflow instance.
func (s *PostgresStore) GetEventCount(ctx context.Context, workflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get event count for %s: begin: %w", workflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = $1`, workflowID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get event count for %s: %w", workflowID, err)
	}
	return count, tx.Commit()
}

// ---- Sticky Session implementations (Feature 10) ----

// UpdateStickyWorker sets the sticky worker for a workflow.
func (s *PostgresStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("update sticky worker: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = $2 WHERE id = $1
	`, workflowID, workerID)
	if err != nil {
		return fmt.Errorf("update sticky worker: %w", err)
	}
	return tx.Commit()
}

// ClearStickyWorker removes the sticky worker assignment.
func (s *PostgresStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("clear sticky worker: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = NULL WHERE id = $1
	`, workflowID)
	if err != nil {
		return fmt.Errorf("clear sticky worker: %w", err)
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Update Request methods (Feature 3: Update Handler)
// ---------------------------------------------------------------------------

// CreateUpdateRequest registers an incoming update request for a workflow.
func (s *PostgresStore) CreateUpdateRequest(ctx context.Context, workflowID, updateName, payload, promiseID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("create update request: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_update_requests (workflow_id, update_name, payload, promise_id, status)
		VALUES ($1, $2, $3, $4, 'pending')
	`, workflowID, updateName, payload, promiseID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// GetPendingUpdateRequests returns all pending (not yet dispatched) update requests.
func (s *PostgresStore) GetPendingUpdateRequests(ctx context.Context, workflowID string) ([]UpdateRequestInfo, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("get pending update requests: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT workflow_id, update_name, payload::text, COALESCE(promise_id, ''), status,
		       COALESCE(result::text, ''), COALESCE(error_msg, ''), created_at
		FROM workflow_update_requests
		WHERE workflow_id = $1 AND status = 'pending'
		ORDER BY priority ASC, created_at
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return requests, tx.Commit()
}

// CompleteUpdateRequest marks an update request as completed with a result or error.
func (s *PostgresStore) CompleteUpdateRequest(ctx context.Context, workflowID, updateName, result, errMsg string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("complete update request: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_update_requests
		SET status = 'completed', result = $3, error_msg = $4, completed_at = now()
		WHERE workflow_id = $1 AND update_name = $2 AND status = 'pending'
	`, workflowID, updateName, result, errMsg)
	if err != nil {
		return err
	}
	return tx.Commit()
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

// computeEventChecksum computes a SHA-256 checksum of the event record's data,
// chained with the previous event's checksum so that deleting an event breaks
// the chain for all subsequent events. When previousChecksum is empty (first
// event or unavailable), it is omitted from the computation.
func computeEventChecksum(rec EventRecord, previousChecksum string) string {
	payload, err := eventRecordToPayload(rec)
	if err != nil {
		// Fall back to checksum of the event type and step as a stable identifier.
		// The fallback path is a safety net; json.Marshal on the payload map never
		// fails in practice because all values are primitive types. The field list
		// here is a stable subset for defense-in-depth.
		data := fmt.Sprintf("%d:%s:%s:%s:%s:%s", rec.Step, rec.EventType, rec.Service, rec.Op, rec.Request, rec.Response)
		hash := sha256.Sum256([]byte(data))
		if previousChecksum == "" {
			return hex.EncodeToString(hash[:])
		}
		chain := fmt.Sprintf("%s:%s", previousChecksum, hex.EncodeToString(hash[:]))
		hash2 := sha256.Sum256([]byte(chain))
		return hex.EncodeToString(hash2[:])
	}
	hash := sha256.Sum256(payload)
	if previousChecksum == "" {
		return hex.EncodeToString(hash[:])
	}
	chain := fmt.Sprintf("%s:%s", previousChecksum, hex.EncodeToString(hash[:]))
	hash2 := sha256.Sum256([]byte(chain))
	return hex.EncodeToString(hash2[:])
}

// VerifyWorkflowEvents loads all events for a workflow, recomputes their
// SHA-256 checksums, and verifies integrity. When the checksum column is
// available (after migration), it compares stored vs. recomputed checksums.
// Before the migration, it computes checksums silently and returns nil.
//
// Required migration for full verification:
//
//	ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT;
func (s *PostgresStore) VerifyWorkflowEvents(ctx context.Context, workflowID string) error {
	// Load the full event history for the workflow.
	events, err := s.LoadEventHistory(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("verify events: load: %w", err)
	}

	// Try to load stored checksums from the DB. If the column doesn't exist
	// (pre-migration), this query will fail, and we skip verification.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("verify events: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT step, checksum FROM event_history
		WHERE workflow_id = $1
		ORDER BY step
	`, workflowID)
	if err != nil {
		// Column does not exist yet — skip verification (pre-migration).
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

	// Recompute and compare checksums with chaining.
	var prevChecksum string
	for _, ev := range events {
		expected, ok := storedChecksums[ev.Step]
		if !ok || expected == "" {
			prevChecksum = "" // Missing event breaks the chain
			continue          // No stored checksum for this step (pre-migration partial data).
		}
		actual := computeEventChecksum(ev, prevChecksum)
		if actual != expected {
			return fmt.Errorf("verify events: workflow %s step %d: checksum mismatch (expected %s, got %s)",
				workflowID, ev.Step, expected, actual)
		}
		prevChecksum = expected
	}
	return tx.Commit()
}
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
		payload["request_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Request))
		if rec.Response != "" {
			payload["response_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Response))
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
		if rec.DetachedName != "" {
			payload["detached_name"] = rec.DetachedName
		}
		if rec.DetachedInput != "" {
			payload["detached_input"] = rec.DetachedInput
		}
		if rec.DetachedRunID != "" {
			payload["detached_run_id"] = rec.DetachedRunID
		}
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
	case "durable_log":
		if rec.Message != "" {
			payload["message"] = rec.Message
		}
		if rec.LogLevel != "" {
			payload["log_level"] = rec.LogLevel
		}
		if rec.LogKV != "" {
			payload["log_kv"] = rec.LogKV
		}
	case "fetch":
		if rec.FetchMethod != "" {
			payload["fetch_method"] = rec.FetchMethod
		}
		if rec.FetchURL != "" {
			payload["fetch_url"] = rec.FetchURL
		}
		if rec.FetchHeaders != "" {
			payload["fetch_headers"] = rec.FetchHeaders
		}
		if rec.FetchBody != "" {
			payload["fetch_body"] = rec.FetchBody
		}
		if rec.FetchResponse != "" {
			payload["fetch_response_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.FetchResponse))
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
	case "acquire_lock":
		if rec.LockKey != "" {
			payload["lock_key"] = rec.LockKey
		}
		if rec.LockTTLMs > 0 {
			payload["lock_ttl_ms"] = rec.LockTTLMs
		}
		payload["lock_acquired"] = rec.LockAcquired
	case "release_lock":
		if rec.LockKey != "" {
			payload["lock_key"] = rec.LockKey
		}
	case "durable_send":
		if rec.Service != "" {
			payload["service"] = rec.Service
		}
		if rec.Op != "" {
			payload["operation"] = rec.Op
		}
		if rec.Request != "" {
			payload["request_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Request))
		}
	case "durable_schedule_invoke":
		if rec.Service != "" {
			payload["service"] = rec.Service
		}
		if rec.Op != "" {
			payload["operation"] = rec.Op
		}
		if rec.Request != "" {
			payload["request_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Request))
		}
		if rec.DurationMs > 0 {
			payload["duration_ms"] = rec.DurationMs
		}
	case "await_child":
		if rec.RunID != "" {
			payload["run_id"] = rec.RunID
		}
		if rec.Response != "" {
			payload["response_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Response))
		}
		if rec.Err != "" {
			payload["error"] = rec.Err
		}
	case "heartbeat":
		if rec.Service != "" {
			payload["service"] = rec.Service
		}
		if rec.Op != "" {
			payload["operation"] = rec.Op
		}
	case "await_all_children":
		if rec.Request != "" {
			payload["request_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Request))
		}
		if rec.Response != "" {
			payload["response_b64"] = base64.StdEncoding.EncodeToString([]byte(rec.Response))
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
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
		// Try base64-encoded fields first, fall back to raw strings for backward compat.
		if v, ok := m["request_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Request = string(decoded)
			}
		} else if v, ok := m["request"].(string); ok {
			rec.Request = v
		}
		if v, ok := m["response_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Response = string(decoded)
			}
		} else if v, ok := m["response"].(string); ok {
			rec.Response = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
		if v, ok := m["duration_ms"].(float64); ok {
			rec.DurationMs = int64(v)
		}
	case "sleep":
		if v, ok := m["duration_ms"].(float64); ok {
			rec.DurationMs = int64(v)
		}
	case "await_signals":
		if v, ok := m["signal_names"].(string); ok {
			rec.SignalNames = v
		}
		if v, ok := m["timeout_ms"].(float64); ok {
			rec.TimeoutMs = int64(v)
		}
	case "signal_received":
		if v, ok := m["signal_name"].(string); ok {
			rec.SignalName = v
		}
		if v, ok := m["signal_payload"].(string); ok {
			rec.SignalPayload = v
		}
	case "defer":
		if v, ok := m["defer_description"].(string); ok {
			rec.DeferDescription = v
		}
		if v, ok := m["defer_id"].(string); ok {
			rec.DeferID = v
		}
	case "child_workflow":
		if v, ok := m["child_name"].(string); ok {
			rec.ChildName = v
		}
		if v, ok := m["child_input"].(string); ok {
			rec.ChildInput = v
		}
		if v, ok := m["run_id"].(string); ok {
			rec.RunID = v
		}
	case "continue_as_new":
		if v, ok := m["new_input"].(string); ok {
			rec.NewInput = v
		}
	case "plugin_call":
		if v, ok := m["plugin_name"].(string); ok {
			rec.PluginName = v
		}
		if v, ok := m["plugin_func"].(string); ok {
			rec.PluginFunc = v
		}
		if v, ok := m["plugin_input"].(string); ok {
			rec.PluginInput = v
		}
		if v, ok := m["plugin_output"].(string); ok {
			rec.PluginOutput = v
		}
		if v, ok := m["plugin_error"].(string); ok {
			rec.PluginError = v
		}
	case "create_promise", "await_promise", "promise_resolved", "promise_rejected":
		if v, ok := m["promise_name"].(string); ok {
			rec.PromiseName = v
		}
		if v, ok := m["promise_id"].(string); ok {
			rec.PromiseID = v
		}
		if v, ok := m["promise_result"].(string); ok {
			rec.PromiseResult = v
		}
		if v, ok := m["promise_error"].(string); ok {
			rec.PromiseError = v
		}
	case "update_handler":
		if v, ok := m["update_handler_name"].(string); ok {
			rec.UpdateHandlerName = v
		}
	case "state_mutation":
		if v, ok := m["state_key"].(string); ok {
			rec.StateKey = v
		}
		if v, ok := m["state_value"].(string); ok {
			rec.StateValue = v
		}
		if v, ok := m["state_delta"].(float64); ok {
			rec.StateDelta = int64(v)
		}
		if v, ok := m["state_op"].(string); ok {
			rec.StateOp = v
		}
	case "run_detached":
		if v, ok := m["detached_name"].(string); ok {
			rec.DetachedName = v
		}
		if v, ok := m["detached_input"].(string); ok {
			rec.DetachedInput = v
		}
		if v, ok := m["detached_run_id"].(string); ok {
			rec.DetachedRunID = v
		}
	case "side_effect":
		if v, ok := m["side_effect_result"].(string); ok {
			rec.SideEffectResult = v
		}
	case "plugin_call_stream_chunk":
		if v, ok := m["plugin_name"].(string); ok {
			rec.PluginName = v
		}
		if v, ok := m["plugin_func"].(string); ok {
			rec.PluginFunc = v
		}
		if v, ok := m["plugin_input"].(string); ok {
			rec.PluginInput = v
		}
		if v, ok := m["plugin_output"].(string); ok {
			rec.PluginOutput = v
		}
		if v, ok := m["plugin_error"].(string); ok {
			rec.PluginError = v
		}
	case "scope_acquired":
		if v, ok := m["scope_key"].(string); ok {
			rec.ScopeKey = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
	case "durable_log":
		if v, ok := m["message"].(string); ok {
			rec.Message = v
		}
		if v, ok := m["log_level"].(string); ok {
			rec.LogLevel = v
		}
		if v, ok := m["log_kv"].(string); ok {
			rec.LogKV = v
		}
	case "fetch":
		if v, ok := m["fetch_method"].(string); ok {
			rec.FetchMethod = v
		}
		if v, ok := m["fetch_url"].(string); ok {
			rec.FetchURL = v
		}
		if v, ok := m["fetch_headers"].(string); ok {
			rec.FetchHeaders = v
		}
		if v, ok := m["fetch_body"].(string); ok {
			rec.FetchBody = v
		}
		if v, ok := m["fetch_response_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.FetchResponse = string(decoded)
			}
		} else if v, ok := m["fetch_response"].(string); ok {
			rec.FetchResponse = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
	case "acquire_lock":
		if v, ok := m["lock_key"].(string); ok {
			rec.LockKey = v
		}
		if v, ok := m["lock_ttl_ms"].(float64); ok {
			rec.LockTTLMs = int64(v)
		}
		if v, ok := m["lock_acquired"].(float64); ok {
			rec.LockAcquired = int(v)
		}
	case "release_lock":
		if v, ok := m["lock_key"].(string); ok {
			rec.LockKey = v
		}
	case "durable_send":
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
		if v, ok := m["request_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Request = string(decoded)
			}
		}
	case "durable_schedule_invoke":
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
		if v, ok := m["request_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Request = string(decoded)
			}
		}
		if v, ok := m["duration_ms"].(float64); ok {
			rec.DurationMs = int64(v)
		}
	case "await_child":
		if v, ok := m["run_id"].(string); ok {
			rec.RunID = v
		}
		if v, ok := m["response_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Response = string(decoded)
			}
		} else if v, ok := m["response"].(string); ok {
			rec.Response = v
		}
		if v, ok := m["error"].(string); ok {
			rec.Err = v
		}
	case "heartbeat":
		if v, ok := m["service"].(string); ok {
			rec.Service = v
		}
		if v, ok := m["operation"].(string); ok {
			rec.Op = v
		}
	case "await_all_children":
		if v, ok := m["request_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Request = string(decoded)
			}
		} else if v, ok := m["request"].(string); ok {
			rec.Request = v
		}
		if v, ok := m["response_b64"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
				rec.Response = string(decoded)
			}
		} else if v, ok := m["response"].(string); ok {
			rec.Response = v
		}
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
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("queue depth: begin: %w", err)
	}
	defer tx.Rollback()

	var count int64
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready' AND task_queue = ANY($1)`,
		pq.Array(s.taskQueues)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return count, tx.Commit()
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

// DeleteExpiredEvents deletes event history rows for completed/failed workflows
// whose completed_at is older than the cutoff. It uses batching to avoid
// locking the event_history table when there are millions of rows to delete.
func (s *PostgresStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM event_history
			WHERE workflow_id IN (
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < $1
				LIMIT 10000
			)
		`, olderThan)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: %w", err)
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
			UPDATE workflow_instances
			SET compaction_state = NULL, compaction_step = NULL, compacted_at = NULL
			WHERE id IN (
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < $1
				  AND compaction_state IS NOT NULL
				LIMIT 10000
			)
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
// Unlike FailWorkflow, this does not require the worker to own the workflow.
func (s *PostgresStore) TerminateWorkflow(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("terminate workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'terminated',
		    error_msg = $2,
		    completed_at = now(),
		    assigned_to = NULL
		WHERE id = $1
	`, workflowID, reason)
	if err != nil {
		return fmt.Errorf("terminate workflow: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("terminate workflow commit: %w", err)
	}
	// Best-effort cleanup.
	s.ClearStickyWorker(context.Background(), workflowID)
	if err := s.ReleaseWorkflowConcurrencyKeys(context.Background(), workflowID); err != nil {
		log.Printf("[db] release concurrency keys for run %s: %v", workflowID, err)
	}
	return nil
}

// DeleteDeadLetteredWorkflows permanently deletes dead-lettered workflow instances
// whose completed_at is older than the cutoff. Child rows (event_history, signals,
// promises, concurrency_keys, update_requests) are automatically deleted via
// ON DELETE CASCADE.
func (s *PostgresStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_instances
			WHERE id IN (
				SELECT id FROM workflow_instances
				WHERE status = 'dead_lettered'
				  AND completed_at IS NOT NULL
				  AND completed_at < $1
				  AND tenant_id = $2
				ORDER BY id
				LIMIT 10000
			)
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

// tryDecodeBase64 attempts to base64-decode s. If decoding fails (e.g. the
// value is a legacy plaintext that was never encoded), it returns s as-is.
// This provides backward compatibility for events stored before base64
// encoding was introduced.
func tryDecodeBase64(s string) string {
	if s == "" {
		return s
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s // not base64-encoded, return raw string
	}
	return string(decoded)
}

// tryEncodeBase64 is a symmetric counterpart to tryDecodeBase64.  It encodes
// s as base64 so that values read back through tryDecodeBase64 are restored
// correctly.  Call sites that previously stored plain text can switch to this
// function: the old (un-encoded) values are still handled by tryDecodeBase64's
// fallback, and new values are properly round-tripped.
func tryEncodeBase64(s string) string {
	if s == "" {
		return s
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// PostgresStoreFactory implements StoreFactory for PostgreSQL.
type PostgresStoreFactory struct {
	db                *sql.DB
	schemaName        string
	idempotencyKeyTTL time.Duration

	encryption               *PayloadEncryption
	encryptSensitivePayloads bool
}

// NewPostgresStoreFactory creates a PostgresStoreFactory.
// The db connection must already be open. schemaName is the PostgreSQL
// schema for cleat tables (defaults to "public").
func NewPostgresStoreFactory(db *sql.DB, schemaName string, idempotencyKeyTTL ...time.Duration) *PostgresStoreFactory {
	if schemaName == "" {
		schemaName = "public"
	}
	ttl := 720 * time.Hour
	if len(idempotencyKeyTTL) > 0 {
		ttl = idempotencyKeyTTL[0]
	}
	return &PostgresStoreFactory{
		db:                db,
		schemaName:        schemaName,
		idempotencyKeyTTL: ttl,
	}
}

// WithEncryption sets encryption at rest on the factory. When enabled,
// sensitive payload fields are encrypted before being written to the database.
func (f *PostgresStoreFactory) WithEncryption(enc *PayloadEncryption, enabled bool) *PostgresStoreFactory {
	f.encryption = enc
	f.encryptSensitivePayloads = enabled
	return f
}

// OpenStore creates a PostgresStore scoped to the given tenant and task queues.
func (f *PostgresStoreFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error) {
	// Ensure the schema exists.
	if f.schemaName != "" && f.schemaName != "public" {
		if _, err := f.db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+pq.QuoteIdentifier(f.schemaName)); err != nil {
			return nil, nil, fmt.Errorf("create schema %s: %w", f.schemaName, err)
		}
	}
	store := NewPostgresStore(f.db, taskQueues...)
	store.tenantID = tenantID
	if f.encryption != nil && f.encryptSensitivePayloads {
		store = store.WithEncryption(f.encryption, true)
	}
	store = store.WithIdempotencyKeyTTL(f.idempotencyKeyTTL)
	return store, nopCloser{}, nil
}

// DriverName returns "postgres".
func (f *PostgresStoreFactory) DriverName() string { return "postgres" }

// Dialect returns DialectPostgres.
func (f *PostgresStoreFactory) Dialect() Dialect { return DialectPostgres }

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
