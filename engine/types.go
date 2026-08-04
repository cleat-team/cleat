package engine

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MaxRetryAttempts is the worker-enforced ceiling for DurableCallWithRetry
// maxAttempts, preventing a misconfigured WASM module from retrying forever.
const MaxRetryAttempts = 100

// maxPayloadLen limits payload text included in divergence error messages
// to prevent log explosion. Full content is represented by its SHA-256 hash.
const maxPayloadLen = 4096

// EventType classifies event history records.
type EventType string

const (
	EventTypeCall                  EventType = "call"
	EventTypeAwaitSignals          EventType = "await_signals"
	EventTypeSignalReceived        EventType = "signal_received"
	EventTypeDefer                 EventType = "defer"
	EventTypeChildWorkflow         EventType = "child_workflow"
	EventTypeAwaitChild            EventType = "await_child"
	EventTypeContinueAsNew         EventType = "continue_as_new"
	EventTypeHeartbeat             EventType = "heartbeat"
	EventTypeAwaitAllChildren      EventType = "await_all_children"
	EventTypePluginCall            EventType = "plugin_call"
	EventTypeCreatePromise         EventType = "create_promise"
	EventTypeAwaitPromise          EventType = "await_promise"
	EventTypePromiseResolved       EventType = "promise_resolved"
	EventTypePromiseRejected       EventType = "promise_rejected"
	EventTypeUpdateHandler         EventType = "update_handler"
	EventTypeStateMutation         EventType = "state_mutation"
	EventTypeRunDetached           EventType = "run_detached"
	EventTypePluginCallStreamChunk EventType = "plugin_call_stream_chunk"
	EventTypeDurableLog            EventType = "durable_log"
	EventTypeAcquireLock           EventType = "acquire_lock"
	EventTypeReleaseLock           EventType = "release_lock"
	EventTypeSideEffect            EventType = "side_effect"
	EventTypeScopeAcquired         EventType = "scope_acquired"
	EventTypeDurableSend           EventType = "durable_send"
	EventTypeDurableScheduleInvoke EventType = "durable_schedule_invoke"
	EventTypeFetch                 EventType = "fetch"
	EventTypePollChild             EventType = "poll_child"
	EventTypeAwaitAnyChild         EventType = "await_any_child"
	EventTypeAdminAction           EventType = "admin_action"
)

// EventRecord is a single event in a workflow's execution history.
// It generalizes the previous CallRecord to support sleep, signal, and defer events.
type EventRecord struct {
	Step      int       `json:"step"`
	EventType EventType `json:"type"`

	// TimestampMs is the virtual time (ms since Unix epoch) that Now()
	// should return after this event completes. For non-sleep events it
	// is the wall-clock time when the event was recorded. For sleep
	// events it is the pre-sleep time plus the sleep duration, encoding
	// the post-sleep virtual time for deterministic replay.
	TimestampMs int64 `json:"timestamp_ms"`

	// CreatedAt is the wall-clock time when the event was recorded in the
	// database. Used for timeline visualization; may be zero for events
	// loaded without timestamps or for events created before this field
	// was added.
	CreatedAt time.Time `json:"created_at,omitempty"`

	// Call fields.
	Service  string `json:"service,omitempty"`
	Op       string `json:"op,omitempty"`
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
	Err      string `json:"err,omitempty"`

	// Sleep fields.
	DurationMs int64 `json:"duration_ms,omitempty"`

	// AwaitSignals fields.
	SignalNames   string `json:"signal_names,omitempty"`
	TimeoutMs     int64  `json:"timeout_ms,omitempty"`
	SignalName    string `json:"signal_name,omitempty"`
	SignalPayload string `json:"signal_payload,omitempty"`

	// Defer fields.
	DeferDescription string `json:"defer_description,omitempty"`
	DeferID          string `json:"defer_id,omitempty"`

	// Promise fields.
	PromiseName   string `json:"promise_name,omitempty"`
	PromiseID     string `json:"promise_id,omitempty"`
	PromiseResult string `json:"promise_result,omitempty"`
	PromiseError  string `json:"promise_error,omitempty"`

	// Child workflow fields.
	ChildName         string `json:"child_name,omitempty"`
	ChildInput        string `json:"child_input,omitempty"`
	RunID             string `json:"run_id,omitempty"`
	ParentWorkflowID  string `json:"parent_workflow_id,omitempty"`
	ParentClosePolicy string `json:"parent_close_policy,omitempty"`

	// ContinueAsNew fields.
	NewInput   string `json:"new_input,omitempty"`
	NewVersion int    `json:"new_version,omitempty"` // for versioned continue_as_new

	// Plugin call fields.
	PluginName   string `json:"plugin_name,omitempty"`
	PluginFunc   string `json:"plugin_func,omitempty"`
	PluginInput  string `json:"plugin_input,omitempty"`
	PluginOutput string `json:"plugin_output,omitempty"`
	PluginError  string `json:"plugin_error,omitempty"`
	Idempotent   bool   `json:"idempotent,omitempty"`

	// Stream chunk fields.
	StreamChunkIndex int  `json:"stream_chunk_index,omitempty"`
	StreamFinish     bool `json:"stream_finish,omitempty"`

	// Update handler fields.
	UpdateHandlerName string `json:"update_handler_name,omitempty"`
	UpdatePayload     string `json:"update_payload,omitempty"`
	UpdateResponse    string `json:"update_response,omitempty"`
	UpdateError       string `json:"update_error,omitempty"`

	// State mutation fields.
	StateKey   string `json:"state_key,omitempty"`
	StateValue string `json:"state_value,omitempty"`
	StateDelta int64  `json:"state_delta,omitempty"`
	StateOp    string `json:"state_op,omitempty"`

	// State list fields.
	StateKeys string `json:"state_keys,omitempty"`

	// HTTP fetch fields.
	FetchMethod   string `json:"fetch_method,omitempty"`
	FetchURL      string `json:"fetch_url,omitempty"`
	FetchHeaders  string `json:"fetch_headers,omitempty"`
	FetchBody     string `json:"fetch_body,omitempty"`
	FetchResponse string `json:"fetch_response,omitempty"`

	// Detached workflow fields.
	DetachedName  string `json:"detached_name,omitempty"`
	DetachedInput string `json:"detached_input,omitempty"`
	DetachedRunID string `json:"detached_run_id,omitempty"`

	// Lock fields.
	LockKey      string `json:"lock_key,omitempty"`
	LockTTLMs    int64  `json:"lock_ttl_ms,omitempty"`
	LockAcquired int    `json:"lock_acquired,omitempty"`

	// SideEffect fields.
	SideEffectResult string `json:"side_effect_result,omitempty"`

	// Scope / virtual object fields.
	ScopeKey string `json:"scope_key,omitempty"`

	// Durable log fields.
	Message  string `json:"message,omitempty"`
	LogLevel string `json:"log_level,omitempty"`
	LogKV    string `json:"log_kv,omitempty"`
}

// CallRecord is kept for backward compatibility in tests.
type CallRecord = EventRecord

// ServiceCaller makes actual external API calls on behalf of cleat workflows.
type ServiceCaller interface {
	Call(ctx context.Context, service, operation, requestJSON string) (responseJSON string, err error)
}

// Fetcher makes HTTP requests on behalf of cleat workflows (Stream R).
type Fetcher interface {
	Fetch(ctx context.Context, method, url, headersJSON, body string) (responseJSON string, err error)
}

// SignalStore provides signal delivery capabilities for running workflows.
type SignalStore interface {
	// DeliverSignal stores a signal for a workflow.
	DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error
	// PollSignal checks for a delivered signal.
	PollSignal(ctx context.Context, workflowID, signalName string) (payload string, found bool, err error)
	// PollCancellation checks whether the workflow has been cancelled.
	PollCancellation(ctx context.Context, workflowID string) (cancelled bool, reason string, err error)
}

// PromiseStore provides promise resolution capabilities for running workflows.
type PromiseStore interface {
	CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error
	ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error
	RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error
	GetPromise(ctx context.Context, workflowID, promiseID string) (status string, result string, errMsg string, err error)
}

// WorkflowState provides access to workflow instance state.
type WorkflowState interface {
	// Version returns the workflow definition version for this instance.
	Version() int
	// MinVersion returns the minimum version this code supports.
	MinVersion() int
	// ChildVersion returns the pinned version for a child workflow name
	// from compile-time WASM metadata. Returns (0, false) if no pin exists.
	ChildVersion(name string) (int, bool)
	// Priority returns the scheduling priority of this workflow instance.
	Priority() int
}

// SuspendError signals that the workflow should be suspended.
type SuspendError struct {
	Reason     string
	Until      time.Time // if non-zero, the workflow should wake at this time
	NewInput   string    // for continue_as_new: the new input payload
	NewVersion int       // for continue_as_new with version: the new workflow version
}

func (e *SuspendError) Error() string {
	if !e.Until.IsZero() {
		return fmt.Sprintf("cleat: suspend until %s: %s", e.Until, e.Reason)
	}
	return fmt.Sprintf("cleat: suspend: %s", e.Reason)
}

// SuspendResult holds the outcome of a suspended workflow execution.
type SuspendResult struct {
	History      []EventRecord
	SuspendUntil time.Time
	Reason       string
	NewInput     string            // for continue_as_new: the new input payload
	NewVersion   int               // for continue_as_new with version: the new workflow version
	Deferrals    map[string]string // registered defers (deferID -> description)

	// ContinueAsNewHandled is true when the engine has already persisted the
	// ContinueAsNew transition (events + new run + old run completion) as part
	// of the suspend path. The worker should NOT call store.ContinueAsNew again.
	ContinueAsNewHandled bool

	// NewRunID is the new workflow run ID when ContinueAsNew has been handled
	// by the engine. Empty if not handled or if the suspend is for another reason.
	NewRunID string
}

// ExecutionResult holds the complete outcome of a workflow run.
type ExecutionResult struct {
	Result     string
	History    []EventRecord
	Suspended  *SuspendResult
	Deferrals  map[string]string
	QueryState map[string]string
}

// ConcurrencyKeyStore provides concurrency key acquisition and release for the engine.
type ConcurrencyKeyStore interface {
	// AcquireConcurrencyKey tries to acquire a concurrency key for a workflow.
	// Returns true if acquired, false if already held by another workflow.
	// Automatically releases expired keys during acquisition.
	AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (acquired bool, err error)

	// ReleaseConcurrencyKey releases a specific concurrency key.
	ReleaseConcurrencyKey(ctx context.Context, key string) error
}

type ChildWorkflowStore interface {
	// StartChildWorkflow creates a child workflow instance linked to a parent.
	// defVersion is the explicit workflow definition version to use, or 0 to use
	// default resolution (SELECT MAX(version)).
	StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)

	// StartChildWorkflowAtomic creates a child workflow and records the parent's
	// child_workflow event in a single database transaction, guaranteeing
	// exactly-once creation even if the worker crashes mid-execution.
	// The event is written to event_history atomically with the child row.
	// The caller should still append the event to the in-memory history for
	// same-execution replay. The later event flush will skip it via
	// ON CONFLICT (workflow_id, step) DO NOTHING.
	StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (runID string, err error)

	GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error)

	// ResolveVersionByTag resolves a workflow version by tag name (e.g. "stable", "canary").
	// Returns the version number and nil error on success, or 0 and an error if the tag
	// is not found.
	ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error)
}

// CrossSchemaChildStore is an optional extension to ChildWorkflowStore for
// starting child workflows in a different PostgreSQL schema.  This enables
// cross-instance workflow cooperation: an instance in schema A can start a
// child workflow in schema B, and the B worker pool picks it up.
type CrossSchemaChildStore interface {
	ChildWorkflowStore

	// StartChildWorkflowInSchema creates a child workflow in the given target schema.
	// The schema must be part of the engine's configured peerSchemas.
	StartChildWorkflowInSchema(ctx context.Context, targetSchema, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)

	// GetChildResultInSchema polls a child workflow in the given target schema.
	GetChildResultInSchema(ctx context.Context, targetSchema, runID string) (resultJSON string, completed bool, err error)
}

// RetryableError is optionally implemented by errors to indicate retryability.
type RetryableError interface {
	Retryable() bool
}

// ReplayStepCallback is called after each event is consumed during replay.
// step is the 0-based index within the replay history.
// event is a pointer to the EventRecord that was just consumed (may be nil for
// inline paths that don't construct a full record).
// queryState is a snapshot (cloned copy) of the current key-value state.
// Return ReplayQuit to abort the replay immediately (cancels the execution context).
type ReplayStepCallback func(step int, event *EventRecord, queryState map[string]string) ReplayStepAction

// ReplayStepAction is the return value from a ReplayStepCallback.
type ReplayStepAction int

const (
	// ReplayNext continues replay to the next event.
	ReplayNext ReplayStepAction = iota
	// ReplayQuit aborts the replay immediately.
	ReplayQuit
)

// EngineOption configures an Engine.
type EngineOption func(*Engine)

// PluginCallObserver is called after every plugin function invocation with the
// call details and duration. Set via WithPluginCallObserver to record metrics.
type PluginCallObserver func(pluginName, functionName string, d time.Duration, err error)

const (
	sleepStatusCompleted = 0
	sleepStatusSuspend   = 1
)

// pendingSentinel marks a DurableCall whose external call has been dispatched
// but whose outcome is not yet persisted. On replay, a pending event means the
// call outcome is ambiguous — the external service may have processed it.
//
// NOTHING WRITES THIS. The detectors below are live and correct, but no
// production path ever stores a pendingSentinel, so in a real crash there is
// nothing for them to find. The contract today is exactly what
// docs/durable-calls.md states: at-least-once, with silent duplicates on crash.
//
// The write side that used to sit in flush.go (flushCallIntent /
// completeCallEvent) was deleted rather than wired in: every completion path
// guards its upsert on `error IS NULL`, so an intent row could never be
// completed, and the sentinel would have stuck forever. See
// docs/durable-call-intent-design.md for that analysis and for the replacement
// design, which drops this sentinel in favour of a dedicated intent_at column.
//
// Keep the constant and the detectors: they cost nothing, they are the read
// half of that design, and deleting them would lose the one part that works.
const pendingSentinel = "__CLEAT_PENDING_INTENT__"

// PendingSentinel is the exported form of pendingSentinel, provided so that
// external packages (notably the integrity test suite) can reference it
// without duplicating the sentinel value.
const PendingSentinel = pendingSentinel

// execSession implements HostHandler for a single execution or replay.
type execSession struct {
	engine           *Engine
	history          []EventRecord
	stepCount        int
	isReplay         bool
	replayJustEnded  bool // true when replay just ended (first sleep after replay completes)
	nowMs            int64
	randomSeq        int64 // monotonic counter for deterministic Random()
	suspendErr       *SuspendError
	deferrals        map[string]string // registered defer callbacks (deferID -> description)
	workflowID       string            // parent workflow instance ID (for child workflows)
	defName          string            // workflow definition name (for metrics labels)
	execRunID        string            // current execution run ID
	queryState       map[string]string // key-value state set via SetQueryState
	stateStore       map[string]string // workflow state for Stream R state operations
	tenantID         string            // tenant ID injected into plugin function context
	callerPluginName string            // for WASM plugins, the calling plugin's name (for call_plugin enforcement)
	queryHandlers    []string          // registered query handler names

	// Scope management for virtual object instances.
	scopePrefix  string   // "vo:<type>:<key>:" prefix, empty if no scope
	scopeObjType string   // current object type in scope
	scopeInstKey string   // current instance key in scope
	scopeSet     bool     // true when scope is active
	heldScopes   []string // concurrency keys held for virtual object scopes

	// originalInput stores the initial workflow input for auto-ContinueAsNew.
	originalInput string

	// autoContinueAsNewTriggered is set to true after the event cap is hit
	// to prevent repeated triggers during the same execution segment.
	autoContinueAsNewTriggered bool

	// lastCancellationCheck is the wall-clock time of the last cancellation
	// poll. Used to throttle checks to at most once per interval.
	lastCancellationCheck time.Time

	// eventCount tracks the number of durable call events in this session.
	// Incremented per freshCall; compared against maxEventsPerWorkflow for
	// auto-ContinueAsNew without querying the database.
	eventCount int

	// lastChecksum tracks the checksum of the most recently flushed event,
	// avoiding a DB round-trip to re-fetch it for the next step's chain.
	lastChecksum string

	// mu protects maps (queryState, stateStore, deferrals) from
	// concurrent access when wasmtime host functions race with Go dispatch.
	mu sync.Mutex

	// stepCallback is the installed ReplayStepCallback (nil means no callback).
	stepCallback ReplayStepCallback

	// stepCancel cancels the execution context when the step callback returns
	// ReplayQuit.
	stepCancel context.CancelFunc
}
