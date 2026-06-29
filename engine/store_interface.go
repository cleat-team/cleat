package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

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

	// AdminForceComplete sets a workflow to 'done' status regardless of current
	// worker assignment. Generation check prevents stale writes. An audit event
	// is written atomically with the status change.
	AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error

	// AdminForceFail sets a workflow to 'failed' status regardless of current
	// worker assignment. Generation check prevents stale writes. An audit event
	// is written atomically with the status change.
	AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error

	// AdminReReplay resets a workflow to 'ready' state for re-execution.
	// Generation check prevents stale writes. An audit event is written
	// atomically with the status change.
	AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error

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

	// ---- Tag methods (deployment channels) ----

	// SetWorkflowTag assigns a tag (e.g., "stable", "canary") to a specific version.
	SetWorkflowTag(ctx context.Context, workflowName string, version int, tag string) error

	// RemoveWorkflowTag deletes a tag assignment.
	RemoveWorkflowTag(ctx context.Context, workflowName string, tag string) error

	// GetWorkflowTag returns the version for the given tag.
	GetWorkflowTag(ctx context.Context, workflowName string, tag string) (int, error)

	// GetWorkflowTags returns all tag -> version mappings for a workflow.
	GetWorkflowTags(ctx context.Context, workflowName string) (map[string]int, error)

	// ---- Routing methods (A/B traffic splitting) ----

	// SetRoutingRule creates a traffic-splitting rule for a workflow version.
	SetRoutingRule(ctx context.Context, workflowName string, targetVersion int, weight float64) error

	// RemoveRoutingRule deletes a routing rule by its ID.
	RemoveRoutingRule(ctx context.Context, ruleID string) error

	// GetRoutingRules returns all routing rules for a workflow.
	GetRoutingRules(ctx context.Context, workflowName string) ([]RoutingRule, error)

	// PickVersionByRouting performs weighted random version selection.
	// Returns 0 if no routing rules exist (caller should use default resolution).
	PickVersionByRouting(ctx context.Context, workflowName string) (int, error)

	// ---- Version Resolution ----

	// ResolveVersionByTag resolves a version tag to a version number.
	// Special case: tag "latest" returns MAX(version) WHERE NOT deprecated.
	ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error)
}

// DefaultTenantUUID is the all-zeros UUID used when no tenant is specified.
const DefaultTenantUUID = "00000000-0000-0000-0000-000000000000"
