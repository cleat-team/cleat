package host

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/cleat-team/cleat/internal/plugin"
	"github.com/cleat-team/cleat/internal/telemetry"
	"github.com/cleat-team/cleat/internal/wasm"
)

// MaxRetryAttempts is the worker-enforced ceiling for DurableCallWithRetry
// maxAttempts, preventing a misconfigured WASM module from retrying forever.
const MaxRetryAttempts = 100

// maxPayloadLen limits payload text included in divergence error messages
// to prevent log explosion. Full content is represented by its SHA-256 hash.
const maxPayloadLen = 4096

// truncateWithHash truncates s to maxLen bytes, appending "... [sha256=<hash>]"
// if truncation occurred. Returns s unchanged if len(s) <= maxLen.
func truncateWithHash(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%s... [sha256=%x]", s[:maxLen], h)
}

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

// ChildWorkflowStore provides child workflow creation and polling for the engine.
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

// PluginFunc is a plugin host function implementation.
// Takes JSON input, returns JSON output. The engine handles WASM I/O.
type PluginFunc func(ctx context.Context, inputJSON string) (outputJSON string, err error)

// PluginStreamFunc is a plugin host function that returns a stream of events.
// Takes JSON input, returns a channel of stream events.
type PluginStreamFunc func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error)

// pluginFuncEntry stores a registered plugin function along with its
// idempotent flag. Idempotent functions are safe to re-invoke during replay.
type pluginFuncEntry struct {
	fn         PluginFunc
	idempotent bool
}

// PluginRegistry maps plugin function names to implementations.
// It also tracks plugin health: if a plugin function panics, the
// entire plugin is marked unhealthy and all its functions return
// an error without being invoked.
type PluginRegistry struct {
	funcs         map[string]pluginFuncEntry // key = lookupKey(pluginName, funcName)
	healthTracker *plugin.PluginHealthTracker
}

func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{
		funcs:         make(map[string]pluginFuncEntry),
		healthTracker: plugin.NewPluginHealthTracker(),
	}
}

// SetHealthTracker replaces the default health tracker with a shared one.
// Used to share a single tracker between PluginRegistry and
// PluginStreamRegistry so a panic in any function marks the plugin
// unhealthy across both registries.
func (pr *PluginRegistry) SetHealthTracker(t *plugin.PluginHealthTracker) {
	pr.healthTracker = t
}

// lookupKey returns a unique key for a plugin function. The \x00 separator
// prevents collisions between names like "a/b" and "a/b" (which would collide
// with "/") and is guaranteed not to appear in valid plugin or function names.
func lookupKey(pluginName, funcName string) string {
	return pluginName + "\x00" + funcName
}

// Register adds a plugin function. Returns an error if the function name
// is already registered for this plugin. The function is wrapped with
// panic recovery so a plugin crash does not take down the worker.
func (pr *PluginRegistry) Register(pluginName, funcName string, fn PluginFunc) error {
	key := lookupKey(pluginName, funcName)
	if _, exists := pr.funcs[key]; exists {
		return fmt.Errorf("plugin function %q already registered", key)
	}
	pluginFn := plugin.PluginFunc(fn)
	wrapped := plugin.RecoverPluginFunc(pluginName, pr.healthTracker, pluginFn)
	pr.funcs[key] = pluginFuncEntry{fn: PluginFunc(wrapped), idempotent: false}
	return nil
}

// RegisterIdempotent registers a plugin function that is safe to re-invoke
// during replay (e.g., read-only S3 GET operations). The function is wrapped
// with panic recovery.
func (pr *PluginRegistry) RegisterIdempotent(pluginName, funcName string, fn PluginFunc) error {
	key := lookupKey(pluginName, funcName)
	if _, exists := pr.funcs[key]; exists {
		return fmt.Errorf("plugin function %q already registered", key)
	}
	pluginFn := plugin.PluginFunc(fn)
	wrapped := plugin.RecoverPluginFunc(pluginName, pr.healthTracker, pluginFn)
	pr.funcs[key] = pluginFuncEntry{fn: PluginFunc(wrapped), idempotent: true}
	return nil
}

// Has reports whether a plugin function is registered.
func (pr *PluginRegistry) Has(pluginName, funcName string) bool {
	_, ok := pr.funcs[lookupKey(pluginName, funcName)]
	return ok
}

func (pr *PluginRegistry) Lookup(pluginName, funcName string) (PluginFunc, bool, bool) {
	entry, ok := pr.funcs[lookupKey(pluginName, funcName)]
	return entry.fn, entry.idempotent, ok
}

// IsPluginHealthy reports whether the given plugin has not panicked.
func (pr *PluginRegistry) IsPluginHealthy(pluginName string) bool {
	return pr.healthTracker.IsHealthy(pluginName)
}

// MarkPluginUnhealthy marks a plugin as unhealthy with the given error.
// All future invocations of the plugin's host functions are blocked.
func (pr *PluginRegistry) MarkPluginUnhealthy(pluginName string, err error) {
	pr.healthTracker.MarkUnhealthy(pluginName, err)
}

// PluginHealthStatus returns the current health status of all plugins
// that have been marked unhealthy. Healthy plugins are not included.
func (pr *PluginRegistry) PluginHealthStatus() []plugin.HealthStatus {
	return pr.healthTracker.UnhealthyStatus()
}

// UnhealthyError returns the error that caused the plugin to be marked
// unhealthy, or nil if the plugin is healthy.
func (pr *PluginRegistry) UnhealthyError(pluginName string) error {
	return pr.healthTracker.UnhealthyError(pluginName)
}

// PluginStreamRegistry maps plugin function names to streaming implementations.
type PluginStreamRegistry struct {
	funcs         map[string]PluginStreamFunc
	healthTracker *plugin.PluginHealthTracker
}

func NewPluginStreamRegistry() *PluginStreamRegistry {
	return &PluginStreamRegistry{
		funcs:         make(map[string]PluginStreamFunc),
		healthTracker: plugin.NewPluginHealthTracker(),
	}
}

// SetHealthTracker replaces the default health tracker with a shared one.
// Used to share a single tracker between PluginRegistry and
// PluginStreamRegistry.
func (psr *PluginStreamRegistry) SetHealthTracker(t *plugin.PluginHealthTracker) {
	psr.healthTracker = t
}

func (psr *PluginStreamRegistry) Register(pluginName, funcName string, fn PluginStreamFunc) error {
	key := lookupKey(pluginName, funcName)
	if _, exists := psr.funcs[key]; exists {
		return fmt.Errorf("plugin stream function %q already registered", key)
	}
	pluginFn := plugin.PluginStreamFunc(fn)
	wrapped := plugin.RecoverPluginStreamFunc(pluginName, psr.healthTracker, pluginFn)
	psr.funcs[key] = PluginStreamFunc(wrapped)
	return nil
}

func (psr *PluginStreamRegistry) Lookup(pluginName, funcName string) (PluginStreamFunc, bool) {
	fn, ok := psr.funcs[lookupKey(pluginName, funcName)]
	return fn, ok
}

// Has reports whether a streaming plugin function is registered.
func (psr *PluginStreamRegistry) Has(pluginName, funcName string) bool {
	_, ok := psr.funcs[lookupKey(pluginName, funcName)]
	return ok
}

// RegisterStream implements plugin.StreamFuncRegistry.
func (psr *PluginStreamRegistry) RegisterStream(pluginName string, opts plugin.FuncOptions, fn plugin.PluginStreamFunc) error {
	return psr.Register(pluginName, opts.Name, PluginStreamFunc(fn))
}

// IsPluginHealthy reports whether the given streaming plugin has not panicked.
func (psr *PluginStreamRegistry) IsPluginHealthy(pluginName string) bool {
	return psr.healthTracker.IsHealthy(pluginName)
}

// MarkPluginUnhealthy marks a streaming plugin as unhealthy with the given error.
func (psr *PluginStreamRegistry) MarkPluginUnhealthy(pluginName string, err error) {
	psr.healthTracker.MarkUnhealthy(pluginName, err)
}

// PluginHealthStatus returns the current health status of all streaming plugins
// that have been marked unhealthy. Healthy plugins are not included.
func (psr *PluginStreamRegistry) PluginHealthStatus() []plugin.HealthStatus {
	return psr.healthTracker.UnhealthyStatus()
}

// UnhealthyError returns the error that caused the streaming plugin to be
// marked unhealthy, or nil if the plugin is healthy.
func (psr *PluginStreamRegistry) UnhealthyError(pluginName string) error {
	return psr.healthTracker.UnhealthyError(pluginName)
}

// RetryableError is optionally implemented by errors to indicate retryability.
type RetryableError interface {
	Retryable() bool
}

// Engine provides cleat execution semantics (Execute/Replay) on top of a
// Runtime. It implements the checkpoint/replay model: on first execution,
// every DurableCall is recorded in the event history; on replay, cached
// results are returned and divergence is detected.
type Engine struct {
	rt                   *Runtime
	caller               ServiceCaller
	fetcher              Fetcher
	signalStore          SignalStore
	promiseStore         PromiseStore
	state                WorkflowState
	workflowID           string
	childWfStore         ChildWorkflowStore
	concurrencyKeyStore  ConcurrencyKeyStore
	compactionState      *CompactionState
	pluginRegistry       *PluginRegistry
	pluginStreamRegistry *PluginStreamRegistry
	updateHandler        func(name, payload string) (string, error)
	pluginCallGuard      *PluginCallGuard
	pluginCallObserver   PluginCallObserver
	tenantID             string
	db                   *sql.DB  // tenant-scoped database connection for plugin host functions
	maxRetries           int      // worker-configured ceiling for retry attempts; 0 means use MaxRetryAttempts
	schema               string   // this instance's PostgreSQL schema name
	peerSchemas          []string // peer cleat schemas for cross-instance child workflows and signals

	// defName is the workflow definition name (used for display and testing).
	defName string

	// defVersion is the workflow definition version (used for display and testing).
	defVersion int

	// versionValidateFn is an optional hook called at the start of replay
	// to validate version compatibility. If non-nil and the workflow is
	// replaying (history is non-nil), the engine calls this before execution.
	// If it returns an error, execution is aborted.
	// NOTE: WithVersionValidation is now always-on; this field is populated
	// automatically when version info is available. Use allowVersionMismatch
	// as the escape hatch.
	versionValidateFn func() error

	// allowVersionMismatch disables version validation at replay start.
	// When true, replays proceed even if version compatibility checks fail.
	// Use this as an escape hatch for emergency rollbacks.
	allowVersionMismatch bool

	// workflowEventVerifier is called at the start of replay to verify
	// event history integrity (checksum verification). If non-nil and the
	// workflow is replaying, the engine calls this before execution.
	// Verification failures are logged; if failOnChecksumMismatch is true,
	// the error is returned and replay is aborted.
	workflowEventVerifier func(ctx context.Context, workflowID string) error

	// failOnChecksumMismatch controls whether checksum verification
	// failures abort replay (true) or only log a warning (false).
	failOnChecksumMismatch bool

	// workerID identifies this worker instance for ContinueAsNew operations.
	workerID string

	// wasmInstanceTimeout is the maximum wall-clock duration for a single WASM
	// execution. If exceeded, the context is cancelled and the workflow fails
	// with a timeout error. Zero means no timeout.
	wasmInstanceTimeout time.Duration

	// defaultWorkflowTimeout is the maximum wall-clock duration for the entire
	// workflow execution including replay and fresh execution. If exceeded,
	// the context is cancelled and the workflow fails with a timeout error.
	// This is a broader timeout than wasmInstanceTimeout, which only covers
	// a single WASM invocation. Zero means no timeout.
	defaultWorkflowTimeout time.Duration

	// continueAsNewHandler is called by the engine in its suspend path
	// to atomically persist a ContinueAsNew transition (events + new run
	// creation + old run completion). This eliminates the race window
	// between the engine returning and the worker calling store.ContinueAsNew.
	// When set, the engine handles ContinueAsNew inline and marks the
	// SuspendResult as ContinueAsNewHandled.
	continueAsNewHandler func(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput string, newEvents []EventRecord, result string, queryState map[string]string, priority int) (newRunID string, err error)

	// Encryption at rest for sensitive event payloads.
	encryption               *PayloadEncryption
	encryptSensitivePayloads bool

	// WorkflowStore for per-backend-aware event count operations.
	workflowStore WorkflowStore

	// Per-workflow resource quotas.
	maxQuotaEvents          int
	maxQuotaChildren        int
	maxQuotaConcurrencyKeys int

	// backends maps language strings (e.g., "go", "python") to WasmBackend
	// implementations. When a backend is registered for the detected language,
	// the Engine delegates WASM compilation and execution to it.
	backends map[string]WasmBackend

	// defaultBackend is the language key used as fallback when no backend
	// matches the detected language. Defaults to "go".
	defaultBackend string

	// maxEventsPerWorkflow is the maximum number of events per workflow run.
	// When this limit is reached, auto-ContinueAsNew is triggered.
	// Zero means unlimited.
	maxEventsPerWorkflow int

	// initialEventCount is the event count at session start, loaded from
	// the store so the session can track events locally without DB queries.
	initialEventCount int

	// requireSignalAuth enables signal authorization checks. When true,
	// SignalWorkflow verifies the calling workflow's identity against the
	// target's allowed_signals before delivering.
	requireSignalAuth bool

	// signalAuthCheck is called to verify that a caller (by defName) is
	// authorized to signal a target workflow. Returns nil if authorized,
	// or an error if denied. Only consulted when requireSignalAuth is true.
	signalAuthCheck func(ctx context.Context, targetWorkflowID, callerDefName string) error

	// traceID is the W3C Trace Context trace-id propagated from the HTTP API
	// through to workflow execution spans. When non-empty, WorkflowSpan creates
	// a parent-linked span for end-to-end trace correlation.
	traceID string
	// stepCallback is an optional callback invoked after each event is consumed
	// during replay. When nil, step advancement is a no-op increment.
	stepCallback ReplayStepCallback

	// logger is the structured logger for engine diagnostic output.
	logger *slog.Logger
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

// WithSignalStore sets the signal store for signal delivery and cancellation.
func WithSignalStore(ss SignalStore) EngineOption {
	return func(e *Engine) { e.signalStore = ss }
}

// WithPromiseStore sets the promise store for promise resolution.
func WithPromiseStore(ps PromiseStore) EngineOption {
	return func(e *Engine) { e.promiseStore = ps }
}

// WithWorkflowState sets the workflow state for version info.
func WithWorkflowState(ws WorkflowState) EngineOption {
	return func(e *Engine) { e.state = ws }
}

// WithWorkflowID sets the workflow instance ID for parent-child tracking.
func WithWorkflowID(id string) EngineOption {
	return func(e *Engine) { e.workflowID = id }
}

// WithTraceID sets the W3C Trace Context trace-id for this workflow execution.
// When non-empty, the trace-id is assigned to generated OpenTelemetry spans,
// enabling end-to-end trace correlation.
func WithTraceID(id string) EngineOption {
	return func(e *Engine) { e.traceID = id }
}

// WithChildWorkflowStore sets the store used to create and poll child workflows.
func WithChildWorkflowStore(cws ChildWorkflowStore) EngineOption {
	return func(e *Engine) { e.childWfStore = cws }
}

// WithFetcher sets the HTTP fetcher for the Stream R fetch host function.
func WithFetcher(f Fetcher) EngineOption {
	return func(e *Engine) { e.fetcher = f }
}

func WithConcurrencyKeyStore(cks ConcurrencyKeyStore) EngineOption {
	return func(e *Engine) { e.concurrencyKeyStore = cks }
}

// WithCompactionState sets the compaction state for replaying a compacted workflow.
func WithCompactionState(cs *CompactionState) EngineOption {
	return func(e *Engine) { e.compactionState = cs }
}

// WithDefName sets the workflow definition name for display and testing.
func WithDefName(name string) EngineOption {
	return func(e *Engine) { e.defName = name }
}

// WithDefVersion sets the workflow definition version for display and testing.
func WithDefVersion(v int) EngineOption {
	return func(e *Engine) { e.defVersion = v }
}

// WithPluginRegistry sets the plugin registry for plugin host function dispatch.
func WithPluginRegistry(pr *PluginRegistry) EngineOption {
	return func(e *Engine) { e.pluginRegistry = pr }
}

// WithPluginStreamRegistry sets the plugin stream registry for streaming
// plugin host function dispatch.
func WithPluginStreamRegistry(psr *PluginStreamRegistry) EngineOption {
	return func(e *Engine) { e.pluginStreamRegistry = psr }
}

// WithUpdateHandler sets the update handler function for processing workflow updates.
func WithUpdateHandler(fn func(name, payload string) (string, error)) EngineOption {
	return func(e *Engine) { e.updateHandler = fn }
}

// WithTenantID sets the tenant ID for the engine, which is injected into
// the context before calling plugin host functions.
func WithTenantID(id string) EngineOption {
	return func(e *Engine) { e.tenantID = id }
}

// PluginCallObserver is called after every plugin function invocation with the
// call details and duration. Set via WithPluginCallObserver to record metrics.
type PluginCallObserver func(pluginName, functionName string, d time.Duration, err error)

// WithPluginCallGuard sets the plugin call guard for enforcing call_plugin
// capability restrictions on WASM plugins.
func WithPluginCallGuard(g *PluginCallGuard) EngineOption {
	return func(e *Engine) { e.pluginCallGuard = g }
}

// WithPluginCallObserver sets an observer called after each plugin function
// invocation with timing information. Useful for recording latency metrics.
func WithPluginCallObserver(o PluginCallObserver) EngineOption {
	return func(e *Engine) { e.pluginCallObserver = o }
}

// WithSchema sets the PostgreSQL schema name for this cleat instance.
func WithSchema(schema string) EngineOption {
	return func(e *Engine) { e.schema = schema }
}

// WithPeerSchemas sets the list of peer cleat schemas this engine can
// interact with for cross-instance child workflows and signals.
func WithPeerSchemas(schemas []string) EngineOption {
	return func(e *Engine) { e.peerSchemas = schemas }
}

// WithDB sets a tenant-scoped database connection for plugin host functions.
func WithDB(db *sql.DB) EngineOption {
	return func(e *Engine) { e.db = db }
}

// WithWorkflowStore sets the workflow store used for backend-aware event count
// quota checks. Required when MaxQuotaEvents > 0.
func WithWorkflowStore(store WorkflowStore) EngineOption {
	return func(e *Engine) { e.workflowStore = store }
}

// WithRequireSignalAuth enables or disables signal authorization checks.
// When enabled, SignalWorkflow verifies the calling workflow's identity
// against the target's allowed_signals before delivering. Default false.
func WithRequireSignalAuth(v bool) EngineOption {
	return func(e *Engine) { e.requireSignalAuth = v }
}

// WithSignalAuthCheck sets the function used to verify that a caller
// (by defName) is authorized to signal a target workflow.
func WithSignalAuthCheck(fn func(ctx context.Context, targetWorkflowID, callerDefName string) error) EngineOption {
	return func(e *Engine) { e.signalAuthCheck = fn }
}

// WithEncryption sets encryption at rest for sensitive event payloads.
func WithEncryption(enc *PayloadEncryption, enabled bool) EngineOption {
	return func(e *Engine) {
		e.encryption = enc
		e.encryptSensitivePayloads = enabled
	}
}

// WithMaxQuotaEvents sets the maximum number of events per workflow before
// quota enforcement kicks in and before auto-ContinueAsNew triggers.
// Zero means unlimited.
func WithMaxQuotaEvents(n int) EngineOption {
	return func(e *Engine) {
		e.maxQuotaEvents = n
		e.maxEventsPerWorkflow = n
	}
}

// WithMaxQuotaChildren sets the maximum number of child workflows per workflow
// before quota enforcement kicks in. Zero means unlimited.
func WithMaxQuotaChildren(n int) EngineOption {
	return func(e *Engine) { e.maxQuotaChildren = n }
}

// WithMaxQuotaConcurrencyKeys sets the maximum number of concurrency keys per
// workflow before quota enforcement kicks in. Zero means unlimited.
func WithMaxQuotaConcurrencyKeys(n int) EngineOption {
	return func(e *Engine) { e.maxQuotaConcurrencyKeys = n }
}

// WithMaxRetryAttempts sets a worker-configured ceiling on retry attempts
// for DurableCallWithRetry, overriding MaxRetryAttempts when set to a
// positive value less than the constant.
func WithMaxRetryAttempts(n int) EngineOption {
	return func(e *Engine) { e.maxRetries = n }
}

// WithInitialEventCount sets the starting event count for the session.
// The session tracks events locally (no DB queries) and triggers
// auto-ContinueAsNew when the count reaches maxEventsPerWorkflow.
func WithInitialEventCount(n int) EngineOption {
	return func(e *Engine) { e.initialEventCount = n }
}

// WithVersionValidation sets an optional function called at the start of
// replay (when event history is non-nil) to validate version compatibility.
// If the function returns an error, execution is aborted.
func WithVersionValidation(fn func() error) EngineOption {
	return func(e *Engine) { e.versionValidateFn = fn }
}

// WithAllowVersionMismatch allows the workflow to replay even when version
// compatibility checks fail. This is an escape hatch for emergency rollbacks
// where you need to replay a workflow against a different version of the
// WASM binary than the one it was created with.
func WithAllowVersionMismatch(allow bool) EngineOption {
	return func(e *Engine) { e.allowVersionMismatch = allow }
}

// WithWorkflowEventVerifier sets a function that verifies event history
// integrity (checksum verification) at the start of replay. If set,
// the verifier is called before execution when replaying. When
// failOnMismatch is true, verification failures abort replay.
func WithWorkflowEventVerifier(fn func(ctx context.Context, workflowID string) error, failOnMismatch bool) EngineOption {
	return func(e *Engine) {
		e.workflowEventVerifier = fn
		e.failOnChecksumMismatch = failOnMismatch
	}
}

// WithWorkerID sets the worker instance identifier on the engine.
// This is needed for ContinueAsNew operations so the engine can call
// the store method directly in its suspend path.
func WithWorkerID(id string) EngineOption {
	return func(e *Engine) { e.workerID = id }
}

// WithWASMInstanceTimeout sets a wall-clock timeout for each WASM execution.
// If the execution exceeds this duration, the context is cancelled and the
// workflow fails with a timeout error. Zero means no timeout.
func WithWASMInstanceTimeout(d time.Duration) EngineOption {
	return func(e *Engine) { e.wasmInstanceTimeout = d }
}

// WithDefaultWorkflowTimeout sets a wall-clock timeout for the entire workflow
// execution (replay + fresh run). If the total execution exceeds this duration,
// the context is cancelled and the workflow fails with a timeout error.
// Zero means no timeout. This wraps wasmInstanceTimeout, which is per-invocation.
func WithDefaultWorkflowTimeout(d time.Duration) EngineOption {
	return func(e *Engine) { e.defaultWorkflowTimeout = d }
}

// WithContinueAsNewHandler sets a handler that the engine calls when a
// ContinueAsNew suspend is detected. The handler should persist the
// transition atomically (events + new run + old run completion).
// When set, the engine calls this from its suspend path, eliminating
// the race between engine return and worker-side store.ContinueAsNew call.
func WithContinueAsNewHandler(fn func(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput string, newEvents []EventRecord, result string, queryState map[string]string, priority int) (newRunID string, err error)) EngineOption {
	return func(e *Engine) { e.continueAsNewHandler = fn }
}

// WithBackend registers a WasmBackend for the given language key.
// When a WASM module's detected language matches, the Engine delegates
// compilation and execution to the registered backend. If no backend
// matches, the legacy wazero-based Runtime is used as fallback.
func WithBackend(language string, backend WasmBackend) EngineOption {
	return func(e *Engine) {
		if e.backends == nil {
			e.backends = make(map[string]WasmBackend)
		}
		e.backends[language] = backend
	}
}

// WithReplayStepCallback sets a callback that is invoked after each event
// is consumed during replay. Return ReplayQuit to abort the replay.
func WithReplayStepCallback(cb ReplayStepCallback) EngineOption {
	return func(e *Engine) {
		e.stepCallback = cb
	}
}

// WithLogger sets the structured logger for the engine.
// If not set, slog.Default() is used.
func WithLogger(l *slog.Logger) EngineOption {
	return func(e *Engine) { e.logger = l }
}

// log returns the engine's structured logger, falling back to slog.Default()
// when no logger has been configured (e.g., in tests that construct Engine
// directly rather than through NewEngine).
func (e *Engine) log() *slog.Logger {
	if e.logger != nil {
		return e.logger
	}
	return slog.Default()
}

// NewEngine creates an Engine backed by the given Runtime and ServiceCaller.
func NewEngine(rt *Runtime, caller ServiceCaller, opts ...EngineOption) *Engine {
	e := &Engine{
		rt:             rt,
		caller:         caller,
		backends:       make(map[string]WasmBackend),
		defaultBackend: "go",
	}
	for _, o := range opts {
		o(e)
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	return e
}

// isComponentWasm detects if the WASM binary uses the Component Model format.
// Component Model binaries use version 0x0001000d in the header, distinguishing
// them from core WASM modules (version 1).
func isComponentWasm(wasmBytes []byte) bool {
	return len(wasmBytes) >= 8 &&
		wasmBytes[4] == 0x0d && wasmBytes[5] == 0x00 &&
		wasmBytes[6] == 0x01 && wasmBytes[7] == 0x00
}

// Execute runs a fresh execution of the workflow and returns the result
// along with the complete event history. If the workflow suspends (sleep,
// await signals), it returns a nil result with non-nil SuspendResult.
// deferrals maps deferID -> description for any defers registered during execution.
// queryState contains key-value state set via SetQueryState during execution.
//
// If the WASM binary is a Component Model binary (detected via header bytes),
// Execute decomposes it into its constituent core modules and instantiates
// them following the component's instance DAG before calling the entry point.
func (e *Engine) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage) (result string, history []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	// Check whether a WasmBackend is registered for this module's language.
	if backend := e.backendForWasm(wasmBytes); backend != nil {
		return e.executeWithBackend(ctx, backend, wasmBytes, entryPoint, input, nil)
	}

	// Check for WASM Component Model binary and dispatch to multi-module
	// path within wazero (no external backend needed).
	if isComponentWasm(wasmBytes) {
		bundle, parseErr := wasm.ParseComponentBundle(wasmBytes)
		if parseErr != nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: parse component bundle: %w", parseErr)
		}
		return e.executeComponent(ctx, bundle, entryPoint, input)
	}

	// Legacy path: compile and execute via the wazero Runtime.
	compiled, err := e.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: compile module: %w", err)
	}
	defer compiled.Close(ctx)
	return e.executeCompiled(ctx, compiled, entryPoint, input, nil, wasmBytes)
}

// ExecuteCompiled is like Execute but takes a pre-compiled module.
// Use this when the module has already been compiled and cached by a
// WorkflowLoader, avoiding redundant compilation.
func (e *Engine) ExecuteCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage) (result string, history []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	return e.executeCompiled(ctx, compiled, entryPoint, input, nil, nil)
}

// Replay replays a workflow from existing event history. Cached results are
// returned for matching steps; divergence triggers an error.
// queryState contains key-value state set via SetQueryState during execution.
func (e *Engine) Replay(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, history []EventRecord) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	// Check whether a WasmBackend is registered for this module's language.
	if backend := e.backendForWasm(wasmBytes); backend != nil {
		return e.executeWithBackend(ctx, backend, wasmBytes, entryPoint, input, history)
	}

	// Legacy path: compile and replay via the wazero Runtime.
	compiled, err := e.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: compile module: %w", err)
	}
	defer compiled.Close(ctx)
	return e.replayCompiled(ctx, compiled, entryPoint, input, history, wasmBytes)
}

// ReplayCompiled is like Replay but takes a pre-compiled module.
// Use this when the module has already been compiled and cached by a
// WorkflowLoader, avoiding redundant compilation.
func (e *Engine) ReplayCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage, history []EventRecord) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	return e.replayCompiled(ctx, compiled, entryPoint, input, history, nil)
}

// backendForWasm looks up a WasmBackend for the given WASM binary by
// detecting its language and checking the registered backends map.
// Returns nil if no backend is registered for the detected language.
func (e *Engine) backendForWasm(wasmBytes []byte) WasmBackend {
	if e.backends == nil {
		return nil
	}
	lang := wasm.DetectLanguage(wasmBytes)
	if backend, ok := e.backends[lang]; ok {
		return backend
	}
	return nil
}

// executeWithBackend runs a workflow execution (fresh or replay) using the
// given WasmBackend. The backend handles compilation and execution; the
// Engine manages the execSession, history, timeouts, and result handling.
func (e *Engine) executeWithBackend(
	ctx context.Context,
	backend WasmBackend,
	wasmBytes []byte,
	entryPoint string,
	input json.RawMessage,
	history []EventRecord,
) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	// If compaction state is set, merge virtual compacted events with tail
	// history to produce a complete replay history for deterministic replay.
	compactedStep := 0
	replayHistory := history
	if e.compactionState != nil && len(history) > 0 {
		replayHistory = buildFullHistoryFromCompaction(history, e.compactionState)
		compactedStep = e.compactionState.CompactedStep
	}

	now := nowMs.Load()
	if len(replayHistory) > 0 && replayHistory[0].TimestampMs > 0 {
		now = replayHistory[0].TimestampMs
	}

	session := &execSession{
		engine:     e,
		history:    replayHistory,
		isReplay:   len(replayHistory) > 0,
		nowMs:      now,
		deferrals:  make(map[string]string),
		workflowID: e.workflowID,
		defName:    e.defName,
		execRunID:  e.workflowID,
		tenantID:   e.tenantID,
		stepCallback: e.stepCallback,
	}

	execCtx, stepCancel := context.WithCancel(ctx)
	session.stepCancel = stepCancel
	execCtx = withHandler(execCtx, session)

	execCtx, workflowSpan := telemetry.WorkflowSpan(execCtx,
		e.workflowID, e.defName, e.defVersion, e.tenantID, e.traceID)
	defer workflowSpan.End()

	// Apply overall workflow execution timeout if configured.
	if e.defaultWorkflowTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.defaultWorkflowTimeout)
		defer cancel()
	}

	// Apply per-execution WASM instance timeout if configured.
	if e.wasmInstanceTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.wasmInstanceTimeout)
		defer cancel()
	}

	// If replaying, verify event history integrity (checksums) and
	// validate version compatibility before proceeding.
	if len(replayHistory) > 0 {
		// (a) Checksum verification.
		if e.workflowEventVerifier != nil {
			if verr := e.workflowEventVerifier(ctx, e.workflowID); verr != nil {
				e.log().WarnContext(ctx, "checksum verification failed", "workflow_id", e.workflowID, "tenant_id", e.tenantID, "error", verr)
				replayChecksumFailuresTotal.Inc()
				if e.failOnChecksumMismatch {
					return "", nil, nil, nil, nil, fmt.Errorf("host: workflow %s: checksum verification failed: %w", e.workflowID, verr)
				}
				e.log().WarnContext(ctx, "checksum verification failed but proceeding (failOnChecksumMismatch=false)", "workflow_id", e.workflowID, "tenant_id", e.tenantID)
			}
		}

		// (b) Version validation (always-on unless allowVersionMismatch).
		if e.versionValidateFn != nil && !e.allowVersionMismatch {
			if verr := e.versionValidateFn(); verr != nil {
				return "", nil, nil, nil, nil, fmt.Errorf("host: version validation failed: %w", verr)
			}
		}
	}

		// Use a per-execution backend instance to prevent data races on
		// the handler/work-data fields when Execute is called concurrently.
		execBackend := backend.PerExecution()
		res, callErr := execBackend.Execute(execCtx, wasmBytes, entryPoint, input, session)
		if callErr != nil {
			// Non-suspend error (trap, panic, timeout, or cancellation).
			// Try running defers on a fresh module.
			if len(session.deferrals) > 0 {
				e.runDefers(context.Background(), wasmBytes, session.deferrals)
			}
			session.releaseHeldScopes(context.Background())
			if enriched := resolveWasmTrap(wasmBytes, callErr.Error()); enriched != "" {
				return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("%s", enriched)
			}
			return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, callErr
	}

	if res.Suspended || session.suspendErr != nil {
		se := session.suspendErr
		if se == nil {
			se = &SuspendError{Reason: "workflow suspended"}
		}

		susResult := &SuspendResult{
			History:      session.history,
			SuspendUntil: se.Until,
			Reason:       se.Reason,
			NewInput:     se.NewInput,
			NewVersion:   se.NewVersion,
			Deferrals:    session.deferrals,
		}
		if se.Reason == "continue_as_new" && e.continueAsNewHandler != nil && !session.isReplay {
			newEvents := session.history[len(replayHistory):]
			// generation is 0 because the engine does not yet track generation
			// for continue-as-new; this code path is dormant (handler is never
			// wired in current deployments).
			priority := 0
			if e.state != nil {
				priority = e.state.Priority()
			}
			newRunID, cnErr := e.continueAsNewHandler(ctx, e.workflowID, e.workerID, int64(0), e.defName, e.defVersion, se.NewInput, newEvents, res.Result, session.queryState, priority)
			if cnErr != nil {
				return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("host: continue_as_new handler failed: %w", cnErr)
			}
			susResult.ContinueAsNewHandled = true
			susResult.NewRunID = newRunID
		}

		return "", stripCompactedEvents(session.history, compactedStep), susResult, session.deferrals, session.queryState, nil
	}

	// Workflow completed successfully. Release any held scopes.
	session.releaseHeldScopes(ctx)
	return res.Result, stripCompactedEvents(session.history, compactedStep), nil, session.deferrals, session.queryState, nil
}

// executeCompiled runs a fresh execution using a pre-compiled module.
// history is the event history to replay (nil for fresh execution).
func (e *Engine) executeCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage, history []EventRecord, wasmBytes []byte) (string, []EventRecord, *SuspendResult, map[string]string, map[string]string, error) {
	mod, err := e.rt.InstantiateModule(ctx, compiled)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: instantiate module: %w", err)
	}
	defer mod.Close(ctx)

	if err := e.rt.InitModule(ctx, mod); err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: init module: %w", err)
	}

	// If compaction state is set, merge virtual compacted events with tail history
	// to produce a complete replay history for deterministic replay.
	compactedStep := 0
	replayHistory := history
	if e.compactionState != nil && len(history) > 0 {
		replayHistory = buildFullHistoryFromCompaction(history, e.compactionState)
		compactedStep = e.compactionState.CompactedStep
	}

	now := nowMs.Load()
	if len(replayHistory) > 0 && replayHistory[0].TimestampMs > 0 {
		now = replayHistory[0].TimestampMs
	}

	session := &execSession{
		engine:        e,
		history:       replayHistory,
		isReplay:      len(replayHistory) > 0,
		nowMs:         now,
		deferrals:     make(map[string]string),
		workflowID:    e.workflowID,
		defName:       e.defName,
		execRunID:     e.workflowID,
		tenantID:      e.tenantID,
		originalInput: string(input),
		eventCount:    e.initialEventCount,
		stepCallback:  e.stepCallback,
	}

	execCtx, stepCancel := context.WithCancel(ctx)
	session.stepCancel = stepCancel
	execCtx = withHandler(execCtx, session)

	execCtx, workflowSpan := telemetry.WorkflowSpan(execCtx,
		e.workflowID, e.defName, e.defVersion, e.tenantID, e.traceID)
	defer workflowSpan.End()

	// Apply overall workflow execution timeout if configured.
	// This wraps the entire execution including replay and fresh run.
	if e.defaultWorkflowTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.defaultWorkflowTimeout)
		defer cancel()
	}

	// Apply per-execution WASM instance timeout if configured.
	if e.wasmInstanceTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.wasmInstanceTimeout)
		defer cancel()
	}

	// If replaying, verify event history integrity (checksums) and
	// validate version compatibility before proceeding.
	if len(replayHistory) > 0 {
		// (a) Checksum verification.
		if e.workflowEventVerifier != nil {
			if err := e.workflowEventVerifier(ctx, e.workflowID); err != nil {
				e.log().WarnContext(ctx, "replay checksum verification failed", "workflow_id", e.workflowID, "tenant_id", e.tenantID, "error", err)
				replayChecksumFailuresTotal.Inc()
				if e.failOnChecksumMismatch {
					return "", nil, nil, nil, nil, fmt.Errorf("host: workflow %s: checksum verification failed: %w", e.workflowID, err)
				}
				e.log().WarnContext(ctx, "replay checksum verification failed but proceeding (failOnChecksumMismatch=false)", "workflow_id", e.workflowID, "tenant_id", e.tenantID)
			}
		}

		// (b) Version validation (always-on unless allowVersionMismatch).
		if e.versionValidateFn != nil && !e.allowVersionMismatch {
			if err := e.versionValidateFn(); err != nil {
				return "", nil, nil, nil, nil, fmt.Errorf("host: version validation failed: %w", err)
			}
		}
	}

	result, err := e.rt.CallExport(execCtx, mod, entryPoint, input)
	if err != nil {
		if errors.Is(err, ErrSuspended) || session.suspendErr != nil {
			se := session.suspendErr
			if se == nil {
				se = &SuspendError{Reason: "workflow suspended"}
			}
			if se.Until.IsZero() {
				se.Until = time.Now().Add(30 * time.Second)
			}
			if se.Until.IsZero() {
				se.Until = time.Now().Add(30 * time.Second)
			}

			// If ContinueAsNew was triggered and the engine has a handler,
			// call it now to atomically persist the transition inline.
			// This eliminates the race window between returning and the
			// worker calling store.ContinueAsNew separately.
			susResult := &SuspendResult{
				History:      session.history,
				SuspendUntil: se.Until,
				Reason:       se.Reason,
				NewInput:     se.NewInput,
				NewVersion:   se.NewVersion,
				Deferrals:    session.deferrals,
			}
			if se.Reason == "continue_as_new" && e.continueAsNewHandler != nil && !session.isReplay {
				// generation is 0 because the engine does not yet track generation
				// for continue-as-new; this code path is dormant (handler is never
				// wired in current deployments).
				newEvents := session.history[len(replayHistory):]
				priority := 0
				if e.state != nil {
					priority = e.state.Priority()
				}
				newRunID, cnErr := e.continueAsNewHandler(ctx, e.workflowID, e.workerID, int64(0), e.defName, e.defVersion, se.NewInput, newEvents, result, session.queryState, priority)
				if cnErr != nil {
					return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("host: continue_as_new handler failed: %w", cnErr)
				}
				susResult.ContinueAsNewHandled = true
				susResult.NewRunID = newRunID
			}

			strippedHistory := stripCompactedEvents(session.history, compactedStep)
			return "", strippedHistory, susResult, session.deferrals, session.queryState, nil
		}
		// Workflow failed with a non-suspend error (trap, panic, timeout,
		// or cancellation). Try running defers on the still-live module
		// first, then fall back to fresh-module defers.
		// Use context.Background() so defer functions execute even when the
		// execCtx has been cancelled or timed out (e.g., workflow timeout).
		if len(session.deferrals) > 0 {
			e.invokeDefersOnTrap(context.Background(), mod, session.deferrals)
			e.runDefers(context.Background(), wasmBytes, session.deferrals)
		}
		session.releaseHeldScopes(context.Background())
		// Attempt to resolve the WASM trap to a source location using
		// DWARF debug info. wazero v1.9.0 already embeds DWARF-resolved
		// file:line locations in trap errors; resolveWasmTrap ensures
		// consistent formatting and serves as a hook for future custom
		// DWARF parsing from the raw wasm binary.
		if enriched := resolveWasmTrap(wasmBytes, err.Error()); enriched != "" {
			return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, fmt.Errorf("%s", enriched)
		}
		return "", stripCompactedEvents(session.history, compactedStep), nil, nil, nil, err
	}

	// Workflow completed successfully. Release any held scopes.
	session.releaseHeldScopes(ctx)
	return result, stripCompactedEvents(session.history, compactedStep), nil, session.deferrals, session.queryState, nil
}

// executeComponent runs a fresh execution using a WASM Component Model binary
// that has been decomposed into a ComponentBundle. It instantiates all core
// modules following the component's instance DAG, wires cross-module imports
// using wazero's experimental ImportResolver, and calls the component's entry
// point export.
//
// The implementation follows the same patterns as executeCompiled: it sets up
//
// NOTE: Fresh-execution only (isReplay: false, history: nil). stepCallback and
// stepCancel are intentionally NOT wired because there is no replay path.
// an execSession for host function routing, uses the standard CallExport
// calling convention, and handles suspension and event history the same way.
func (e *Engine) executeComponent(ctx context.Context, bundle *wasm.ComponentBundle,
	entryPoint string, input json.RawMessage) (string, []EventRecord, *SuspendResult, map[string]string, map[string]string, error) {

	// ---- Step 1: Compile all core modules ----
	const componentAdapterModule = "__component_adapter__"
	compiled := make([]wazero.CompiledModule, len(bundle.Modules))
	for i, w := range bundle.Modules {
		w = wasm.PatchEmptyImportModuleName(w, componentAdapterModule)
		var err error
		compiled[i], err = e.rt.CompileModule(ctx, w)
		if err != nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: compile core module %d: %w", i, err)
		}
		defer compiled[i].Close(ctx)
	}

	// ---- Step 2: Set up execution session ----
	now := nowMs.Load()
	session := &execSession{
		engine:     e,
		history:    nil, // fresh execution, no history
		isReplay:   false,
		nowMs:      now,
		deferrals:  make(map[string]string),
		workflowID: e.workflowID,
		defName:    e.defName,
		execRunID:  e.workflowID,
		tenantID:   e.tenantID,
	}
	execCtx := withHandler(ctx, session)

	execCtx, workflowSpan := telemetry.WorkflowSpan(execCtx,
		e.workflowID, e.defName, e.defVersion, e.tenantID, e.traceID)
	defer workflowSpan.End()

	if e.defaultWorkflowTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.defaultWorkflowTimeout)
		defer cancel()
	}
	if e.wasmInstanceTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.wasmInstanceTimeout)
		defer cancel()
	}

	// ---- Step 3: Walk instance DAG and instantiate core modules ----
	// resolvedInstances[i] is the wazero api.Module for instance i (nil for
	// FromExports-only instances).
	resolvedInstances := make([]api.Module, len(bundle.Instances))

	// resolveModuleForInstance returns the real module for an instance,
	// or nil for FromExports instances that have no module yet.
	resolveModuleForInstance := func(instIdx int) api.Module {
		if instIdx < 0 || instIdx >= len(resolvedInstances) {
			return nil
		}
		if m := resolvedInstances[instIdx]; m != nil {
			return m
		}
		return nil
	}

	// Keep track of all instantiated modules for cleanup.
	var cleanupMods []api.Module
	defer func() {
		for _, m := range cleanupMods {
			m.Close(ctx)
		}
	}()

	for i, inst := range bundle.Instances {
		if inst.ModuleIndex < 0 {
			// FromExports-only: no actual module instantiation.
			// The export aliases are resolved in Step 4.
			continue
		}

		cm := compiled[inst.ModuleIndex]

		// Build a map from import module name (as used by this module's
		// WASM import section) to the source instance index that provides it.
		importNameToInstance := make(map[string]int, len(inst.Args))
		for _, arg := range inst.Args {
			importNameToInstance[arg.Name] = arg.InstanceIndex
			// Also map the synthetic name used to replace empty module
			// names to the same source instance.
			if arg.Name == "" {
				importNameToInstance[componentAdapterModule] = arg.InstanceIndex
			}
		}

		// Use the experimental ImportResolver to redirect cross-module imports.
		// Host modules ("env", "wasi_snapshot_preview1", "teavm") are already
		// registered in wazero's store by NewRuntime and resolve via store
		// fallback (when the resolver returns nil).
		instantiateCtx := experimental.WithImportResolver(execCtx, func(name string) api.Module {
			// Host WASI and teavm modules are always resolved from the store.
			if name == "wasi_snapshot_preview1" || name == "teavm" {
				return nil
			}
			// DAG-mapped imports take priority (handles "env" routing to
			// component instances and cross-module references).
			if srcIdx, ok := importNameToInstance[name]; ok {
				if m := resolveModuleForInstance(srcIdx); m != nil {
					return m
				}
			}
			// "env" fallback to host store (when no DAG mapping exists).
			if name == "env" {
				return nil
			}
			return nil
		})

		mod, err := e.rt.InstantiateModuleNamed(instantiateCtx, cm, fmt.Sprintf("__core_%d__", i))
		if err != nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: instantiate instance %d (module %d): %w", i, inst.ModuleIndex, err)
		}
		resolvedInstances[i] = mod
		cleanupMods = append(cleanupMods, mod)
	}

	// ---- Step 4: Build resolved exports map per instance ----
	// resolvedExports[i] maps export name -> (actual export name, source module)
	// for each instance. This resolves FromExports chains.
	type resolvedExp struct {
		exportName string // actual export name on the source module
		mod        api.Module
	}
	resolvedExports := make([]map[string]resolvedExp, len(bundle.Instances))

	for i, inst := range bundle.Instances {
		resolvedExports[i] = make(map[string]resolvedExp)

		if inst.ModuleIndex >= 0 {
			// Collect all exports from the instantiated module.
			mod := resolvedInstances[i]
			if mod == nil {
				continue
			}

			// Function exports.
			for _, fd := range mod.ExportedFunctionDefinitions() {
				for _, en := range fd.ExportNames() {
					resolvedExports[i][en] = resolvedExp{exportName: en, mod: mod}
				}
			}

			// Memory exports.
			for memName := range mod.ExportedMemoryDefinitions() {
				resolvedExports[i][memName] = resolvedExp{exportName: memName, mod: mod}
			}
		}

		// Apply FromExports aliases (copies export references from source).
		for _, fe := range inst.FromExports {
			if fe.SourceInstance >= 0 && fe.SourceInstance < len(resolvedExports) {
				if exp, ok2 := resolvedExports[fe.SourceInstance][fe.SourceName]; ok2 {
					resolvedExports[i][fe.Name] = resolvedExp{
						exportName: exp.exportName,
						mod:        exp.mod,
					}
				}
			}
		}
	}

	// ---- Step 5: Find the entry point and initialize the module ----
	exp, ok := bundle.Exports[entryPoint]
	if !ok {
		return "", nil, nil, nil, nil, fmt.Errorf("host: component export %q not found", entryPoint)
	}

	// Resolve the entry point through the export chain (handles FromExports).
	var entryMod api.Module
	var entryExportName string

	if re, ok2 := resolvedExports[exp.InstanceIndex][exp.Name]; ok2 && re.mod != nil {
		entryMod = re.mod
		entryExportName = re.exportName
	} else if exp.InstanceIndex < len(resolvedInstances) && resolvedInstances[exp.InstanceIndex] != nil {
		// Fallback: try direct lookup on the module.
		entryMod = resolvedInstances[exp.InstanceIndex]
		entryExportName = exp.Name
		if fn := entryMod.ExportedFunction(entryExportName); fn == nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: export %q not found on instance %d", entryExportName, exp.InstanceIndex)
		}
	} else {
		return "", nil, nil, nil, nil, fmt.Errorf("host: cannot resolve component export %q (instance %d)", entryPoint, exp.InstanceIndex)
	}

	// Initialize the module (calls _start if present, e.g. for Go wasip1
	// runtime initialization; no-op for modules without _start).
	if err := e.rt.InitModule(execCtx, entryMod); err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: init component entry module: %w", err)
	}

	// ---- Step 6: Call the entry point ----
	result, err := e.rt.CallExport(execCtx, entryMod, entryExportName, input)
	if err != nil {
		if errors.Is(err, ErrSuspended) || session.suspendErr != nil {
			se := session.suspendErr
			if se == nil {
				se = &SuspendError{Reason: "workflow suspended"}
			}

			susResult := &SuspendResult{
				History:      session.history,
				SuspendUntil: se.Until,
				Reason:       se.Reason,
				NewInput:     se.NewInput,
				NewVersion:   se.NewVersion,
				Deferrals:    session.deferrals,
			}
			if se.Reason == "continue_as_new" && e.continueAsNewHandler != nil && !session.isReplay {
				// generation is 0 because the engine does not yet track generation
				// for continue-as-new; this code path is dormant (handler is never
				// wired in current deployments).
				newEvents := session.history
				priority := 0
				if e.state != nil {
					priority = e.state.Priority()
				}
				newRunID, cnErr := e.continueAsNewHandler(ctx, e.workflowID, e.workerID, int64(0), e.defName, e.defVersion, se.NewInput, newEvents, result, session.queryState, priority)
				if cnErr != nil {
					return "", session.history, nil, nil, nil, fmt.Errorf("host: continue_as_new handler failed: %w", cnErr)
				}
				susResult.ContinueAsNewHandled = true
				susResult.NewRunID = newRunID
			}

			return "", session.history, susResult, session.deferrals, session.queryState, nil
		}
		// Workflow failed with non-suspend error.
		if len(session.deferrals) > 0 {
			e.invokeDefersOnTrap(ctx, entryMod, session.deferrals)
		}
		session.releaseHeldScopes(ctx)
		return "", session.history, nil, nil, nil, err
	}

	// Workflow completed successfully.
	session.releaseHeldScopes(ctx)
	return result, session.history, nil, session.deferrals, session.queryState, nil
}

// replayCompiled runs a replay using a pre-compiled module.
func (e *Engine) replayCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage, history []EventRecord, wasmBytes []byte) (string, []EventRecord, *SuspendResult, map[string]string, map[string]string, error) {
	return e.executeCompiled(ctx, compiled, entryPoint, input, history, wasmBytes)
}

// RunDefer invokes a defer cleanup function in the WASM module.
// This is called by the worker on workflow exit (after the main entry point
// returns) to run registered defer callbacks in LIFO order.
func (e *Engine) RunDefer(ctx context.Context, wasmBytes []byte, deferName string, input json.RawMessage) (string, error) {
	compiled, err := e.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", fmt.Errorf("host: compile module for defer: %w", err)
	}
	defer compiled.Close(ctx)
	return e.RunDeferCompiled(ctx, compiled, deferName, input)
}

// RunDeferCompiled is like RunDefer but takes a pre-compiled module.
func (e *Engine) RunDeferCompiled(ctx context.Context, compiled wazero.CompiledModule, deferName string, input json.RawMessage) (string, error) {
	mod, err := e.rt.InstantiateModule(ctx, compiled)
	if err != nil {
		return "", fmt.Errorf("host: instantiate module for defer: %w", err)
	}
	defer mod.Close(ctx)

	if err := e.rt.InitModule(ctx, mod); err != nil {
		return "", fmt.Errorf("host: init module for defer: %w", err)
	}

	// Defer functions don't need history replay — they're always fresh.
	return e.rt.CallExport(ctx, mod, deferName, input)
}

// invokeDefersOnTrap attempts to invoke registered defer callbacks after a WASM trap.
// Each defer is called as a separate export. Failures are logged but not returned —
// the original trap error takes priority.
func (e *Engine) invokeDefersOnTrap(ctx context.Context, mod api.Module, deferrals map[string]string) {
	for deferID, description := range deferrals {
		exportName := "cleat_defer_" + deferID
		fn := mod.ExportedFunction(exportName)
		if fn == nil {
			e.log().WarnContext(ctx, "defer export not found", "defer_id", deferID, "description", description, "export_name", exportName)
			continue
		}
		_, _, err := e.rt.CallExportWithSuspend(ctx, mod, exportName, []byte("{}"))
		if err != nil {
			e.log().WarnContext(ctx, "defer execution failed", "defer_id", deferID, "description", description, "error", err)
		}
	}
}

// DispatchUpdate dispatches an update to a workflow by invoking its registered handler.
// The handler receives the update name and payload JSON, and returns the result JSON.
// Returns an error if no update handler is configured on the engine.
func (e *Engine) DispatchUpdate(ctx context.Context, name, payload string) (string, error) {
	if e.updateHandler == nil {
		return "", fmt.Errorf("host: no update handler configured for this engine. Call WithUpdateHandler before DispatchUpdate.")
	}
	return e.updateHandler(name, payload)
}

// Deferrals returns the defers registered during the execution that produced
// the given history. It scans the history for defer events.
func DeferralsFromHistory(history []EventRecord) map[string]string {
	defs := make(map[string]string)
	for _, rec := range history {
		if rec.EventType == EventTypeDefer {
			defs[rec.DeferID] = rec.DeferDescription
		}
	}
	return defs
}

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
	signals          map[string]string // pending signals delivered during this session
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

	// eventCount tracks the number of durable call events in this session.
	// Incremented per freshCall; compared against maxEventsPerWorkflow for
	// auto-ContinueAsNew without querying the database.
	eventCount int

	// mu protects maps (queryState, stateStore, signals, deferrals) from
	// concurrent access when wasmtime host functions race with Go dispatch.
	mu sync.Mutex

	// stepCallback is the installed ReplayStepCallback (nil means no callback).
	stepCallback ReplayStepCallback

	// stepCancel cancels the execution context when the step callback returns
	// ReplayQuit.
	stepCancel context.CancelFunc
}

// advanceReplayStep increments stepCount and invokes the step callback if set.
// rec may be nil for inline replay paths without a full EventRecord.
// Returns false if the callback returned ReplayQuit (caller should abort).
func (s *execSession) advanceReplayStep(ctx context.Context, rec *EventRecord) bool {
	s.stepCount++
	if s.stepCallback == nil {
		return true
	}
	return s.invokeStepCallback(ctx, rec)
}

// invokeStepCallback invokes the step callback if set, building a queryState
// snapshot. Returns false if the callback returned ReplayQuit.
func (s *execSession) invokeStepCallback(ctx context.Context, rec *EventRecord) bool {
	if s.stepCallback == nil {
		return true
	}
	// Snapshot queryState to prevent callback from mutating it.
	qs := make(map[string]string, len(s.queryState))
	for k, v := range s.queryState {
		qs[k] = v
	}
	action := s.stepCallback(s.stepCount-1, rec, qs)
	if action == ReplayQuit {
		if s.stepCancel != nil {
			s.stepCancel()
		}
		return false
	}
	return true
}

var _ HostHandler = (*execSession)(nil)

// writeResult writes a string to WASM linear memory. In the normal (wazero)
// path, it writes through the api.Memory obtained from m. In the wasmtime
// path, it uses a raw byte buffer stored in the context via
// contextWithRawMemBuf, in which case m can be nil.
func (s *execSession) writeResult(ctx context.Context, m api.Module, ptr uint32, val string, maxLen uint32) (uint32, error) {
	if rawBuf, ok := ctx.Value(wasmMemBufKey{}).([]byte); ok && rawBuf != nil {
		data := []byte(val)
		if uint32(len(data)) > maxLen {
			data = data[:maxLen]
		}
		n := copy(rawBuf[ptr:], data)
		return uint32(n), nil
	}
	if m != nil {
		if mem := m.Memory(); mem != nil {
			return writeWasmString(mem, ptr, val, maxLen)
		}
	}
	return 0, nil
}

// ---- HostHandler implementation ----

func (s *execSession) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
	}
	return s.freshCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
}

func (s *execSession) freshCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {

	durableCallsTotal.Inc()
	freshStepsTotal.Inc()
	atomic.AddInt64(&freshStepCount, 1)

	// Check cancellation before making the call.
	callCtx := ctx
	if s.engine.signalStore != nil {
		cancelled, _, err := s.engine.signalStore.PollCancellation(ctx, "")
		if err == nil && cancelled {
			written, _ := s.writeResult(ctx, m, responsePtr, "workflow cancelled", responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}
	}

	// Check event cap: if the number of events has reached the limit, auto-trigger
	// ContinueAsNew to start a fresh run with reset event_count. Events are
	// tracked locally in the session (no DB query per call).
	if s.engine.maxEventsPerWorkflow > 0 && s.eventCount >= s.engine.maxEventsPerWorkflow && !s.autoContinueAsNewTriggered {
		s.autoContinueAsNewTriggered = true
		continueAsNewTotal.WithLabelValues("event_cap").Inc()
		s.engine.log().InfoContext(ctx, "auto-ContinueAsNew triggered", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "event_count", s.eventCount, "max", s.engine.maxEventsPerWorkflow)
		s.ContinueAsNew(ctx, m, s.originalInput)
		m.CloseWithExitCode(ctx, 0)
		written, _ := s.writeResult(ctx, m, responsePtr, "", responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}
	s.eventCount++

	step := s.stepCount
	callCtx, eventSpan := telemetry.EventSpan(callCtx, step, "call", service, operation)
	defer eventSpan.End()
	resp, err := s.engine.caller.Call(callCtx, service, operation, requestJSON)

	var callErr string
	if err != nil {
		callErr = err.Error()
	}

	rec := EventRecord{
		Step:      step,
		EventType: EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Response:  resp,
		Err:       callErr,
	}
	s.recordEvent(rec)

	// Flush the event immediately to guarantee at-least-once: if the worker
	// crashes before the workflow completes, replay will find this event
	// and return the cached response.  (Use DurableCallIdempotent for
	// exactly-once with write-ahead logging and ambiguity detection.)
	if s.engine.db != nil {
		if flushErr := s.engine.flushEvent(context.Background(), s.workflowID, rec); flushErr != nil {
			s.engine.log().ErrorContext(ctx, "freshCall flushEvent failed", "workflow_id", s.workflowID, "step", rec.Step, "error", flushErr)
		}
	}

	if err != nil {
		written, _ := s.writeResult(ctx, m, responsePtr, err.Error(), responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, resp, responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) replayCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {

	replayStepsTotal.WithLabelValues(s.defName).Inc()
	atomic.AddInt64(&replayStepCount, 1)

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		if rec.EventType != EventTypeCall {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: expected call event, got %s.\n  actual request: %s\n  expected request: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, rec.EventType,
				truncateWithHash(requestJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Service != service || rec.Op != operation {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s.%s but history has %s.%s.\n  actual request: %s\n  expected request: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, service, operation, rec.Service, rec.Op,
				truncateWithHash(requestJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		// Detect a pending call intent: the external call was dispatched
		// but the outcome was never persisted.  Return ErrAmbiguous so
		// the caller can check the external service before retrying.
		if rec.Err == pendingSentinel {
			ambiguousCallsTotal.Inc()
			ambiguousErr := fmt.Sprintf(
				"[AMBIGUOUS] call outcome unknown at step %d: the external call to %s.%s was dispatched but the response was not recorded before a crash. Check the external service before retrying.",
				rec.Step, rec.Service, rec.Op)
			written, _ := s.writeResult(ctx, m, responsePtr, ambiguousErr, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Err != "" {
			written, _ := s.writeResult(ctx, m, responsePtr, rec.Err, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written, _ := s.writeResult(ctx, m, responsePtr, rec.Response, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	// Past recorded history — switch to fresh execution.
	s.exitReplay()
	return s.freshCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
}

func (s *execSession) PluginCall(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayPluginCall(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
	}
	return s.freshPluginCall(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
}

func (s *execSession) replayPluginCall(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		if rec.EventType != EventTypePluginCall {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: expected plugin_call event, got %s.\n  actual input: %s\n  expected (cached) input: %s\n  expected (cached) output: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, rec.EventType,
				truncateWithHash(inputJSON, maxPayloadLen),
				truncateWithHash(rec.PluginInput, maxPayloadLen),
				truncateWithHash(rec.PluginOutput, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.PluginName != pluginName || rec.PluginFunc != functionName {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s/%s but history has %s/%s.\n  actual input: %s\n  expected (cached) input: %s\n  expected (cached) output: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, pluginName, functionName, rec.PluginName, rec.PluginFunc,
				truncateWithHash(inputJSON, maxPayloadLen),
				truncateWithHash(rec.PluginInput, maxPayloadLen),
				truncateWithHash(rec.PluginOutput, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Idempotent {
			// Safe to re-invoke during replay -- read-only operation (S3 GET).
			// Look up the function and call it, returning fresh output.
			// Do NOT append to newEvents (the event is already in history).
			return s.freshPluginCallWithHistory(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
		}

		// Idempotent flag may not be persisted in DB (no event_history column).
		// Fall back to registry lookup: if the function is currently registered
		// as idempotent, re-invoke instead of returning cached output.
		if s.engine.pluginRegistry != nil {
			_, idempotent, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)
			if ok && idempotent {
				return s.freshPluginCallWithHistory(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
			}
		}

		if rec.PluginError != "" {
			written, _ := s.writeResult(ctx, m, responsePtr, rec.PluginError, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written, _ := s.writeResult(ctx, m, responsePtr, rec.PluginOutput, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	// Past recorded history -- switch to fresh execution.
	s.exitReplay()
	return s.freshPluginCall(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
}

func (s *execSession) freshPluginCall(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	return s.freshPluginCallInternal(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen, true)
}

// freshPluginCallWithHistory is like freshPluginCall but does not record the
// event in history or advance the step counter. Used for replay re-invocation
// of idempotent functions where the event is already in history.
func (s *execSession) freshPluginCallWithHistory(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	return s.freshPluginCallInternal(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen, false)
}

func (s *execSession) freshPluginCallInternal(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32, recordEvent bool) int64 {

	// Look up the plugin function.
	if s.engine.pluginRegistry == nil {
		errMsg := fmt.Sprintf("plugin function %s/%s not available: no plugin registry configured. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}
	fn, idempotent, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)

	var outputJSON string
	var fnErr error
	if !ok {
		fnErr = fmt.Errorf("plugin function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
	} else {
		// Check plugin call guard (enforces call_plugin capability for WASM plugins).
		if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
			if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
				fnErr = err
			}
		}
		if fnErr == nil {
			// Inject call context (tenant ID + workflow ID) for plugin functions.
			callCtx := ctx
			cc := &plugin.CallContext{}
			if s.tenantID != "" {
				cc.TenantID = s.tenantID
			}
			if s.workflowID != "" {
				cc.WorkflowID = s.workflowID
			}
			if s.engine.db != nil {
				cc.DB = s.engine.db
			}
			callCtx = plugin.WithCallContext(callCtx, cc)

			// Actually call the plugin.
			step := s.stepCount
			callCtx, eventSpan := telemetry.EventSpan(callCtx, step, "plugin_call", pluginName, functionName)
			t0 := time.Now()
			outputJSON, fnErr = fn(callCtx, inputJSON)
			pluginCallDuration.WithLabelValues(pluginName, functionName).Observe(time.Since(t0).Seconds())
			eventSpan.End()
		}
	}

	var errStr string
	if fnErr != nil {
		errStr = fnErr.Error()
	}

	// Record in event history BEFORE checking for errors, so that all
	// plugin calls are captured (even failed lookups). This ensures
	// replay determinism — the history must include every call attempt.
	if recordEvent {
		rec := EventRecord{
			Step:         s.stepCount,
			EventType:    EventTypePluginCall,
			PluginName:   pluginName,
			PluginFunc:   functionName,
			PluginInput:  inputJSON,
			PluginOutput: outputJSON,
			PluginError:  errStr,
			Idempotent:   idempotent,
		}
		s.recordEvent(rec)

		// Flush immediately so plugin results survive worker crashes.
		if s.engine.db != nil {
			if flushErr := s.engine.flushEvent(context.Background(), s.workflowID, rec); flushErr != nil {
				s.engine.log().ErrorContext(ctx, "PluginCall flushEvent failed", "workflow_id", s.workflowID, "step", rec.Step, "error", flushErr)
			}
		}
	}

	if fnErr != nil {
		written, _ := s.writeResult(ctx, m, responsePtr, errStr, responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, outputJSON, responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) PluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayPluginCallStreaming(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
	}
	return s.freshPluginCallStreaming(ctx, m, pluginName, functionName, inputJSON, responsePtr, responseMaxLen)
}

// recordStreamError records a synthetic stream chunk event representing a
// stream-level error (e.g. registry not found, call guard rejection). This
// ensures replayPluginCallStreaming can reproduce the same error result.
func (s *execSession) recordStreamError(pluginName, functionName, inputJSON, errMsg string) {
	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypePluginCallStreamChunk,
		PluginName:       pluginName,
		PluginFunc:       functionName,
		PluginInput:      inputJSON,
		PluginOutput:     errMsg,
		StreamChunkIndex: 0,
		StreamFinish:     true,
	}
	s.recordEvent(rec)
}

func (s *execSession) freshPluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	// Look up the streaming plugin function.
	if s.engine.pluginStreamRegistry == nil {
		errMsg := "plugin_call_streaming: no plugin stream registry configured"
		s.recordStreamError(pluginName, functionName, inputJSON, errMsg)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	fn, ok := s.engine.pluginStreamRegistry.Lookup(pluginName, functionName)
	if !ok {
		errMsg := fmt.Sprintf("plugin stream function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		s.recordStreamError(pluginName, functionName, inputJSON, errMsg)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	// Check plugin call guard for streaming calls too.
	if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
		if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
			errMsg := err.Error()
			s.recordStreamError(pluginName, functionName, inputJSON, errMsg)
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 0, 1)
		}
	}

	// Inject call context.
	callCtx := ctx
	cc := &plugin.CallContext{}
	if s.tenantID != "" {
		cc.TenantID = s.tenantID
	}
	if s.workflowID != "" {
		cc.WorkflowID = s.workflowID
	}
	if s.engine.db != nil {
		cc.DB = s.engine.db
	}
	callCtx = plugin.WithCallContext(callCtx, cc)

	// Call the streaming plugin function and collect chunks.
	chunkCh, err := fn(callCtx, inputJSON)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: %v", pluginName, functionName, err)
		s.recordStreamError(pluginName, functionName, inputJSON, errMsg)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	var collected []plugin.StreamEvent
	index := 0
	// Drain the channel on exit to prevent goroutine leak when the context
	// is cancelled mid-stream. The producer blocks on send until the receiver
	// reads; draining ensures it can exit.
	defer func() {
		for range chunkCh {
		}
	}()
	for {
		select {
		case <-callCtx.Done():
			// Context cancelled — return partial results.
			goto done
		case chunk, ok := <-chunkCh:
			if !ok {
				goto done
			}
			collected = append(collected, chunk)

			// Record each chunk as an event.
			rec := EventRecord{
				Step:             s.stepCount,
				EventType:        EventTypePluginCallStreamChunk,
				PluginName:       pluginName,
				PluginFunc:       functionName,
				PluginInput:      inputJSON,
				PluginOutput:     chunk.Content,
				StreamChunkIndex: index,
				StreamFinish:     chunk.Finish,
			}
			s.recordEvent(rec)
			index++
		}
	}
done:
	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) replayPluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	var collected []plugin.StreamEvent
	index := 0

	// Read consecutive stream chunk events from history.
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if rec.EventType != EventTypePluginCallStreamChunk {
			break
		}
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		chunk := plugin.StreamEvent{
			Index:   rec.StreamChunkIndex,
			Content: rec.PluginOutput,
			Finish:  rec.StreamFinish,
		}
		if rec.StreamChunkIndex > 0 || (rec.StreamChunkIndex == 0 && rec.StreamFinish) {
			chunk.Index = rec.StreamChunkIndex
		} else {
			chunk.Index = index
		}
		collected = append(collected, chunk)
		index++
	}

	// A single finished chunk with no real chunk content is a stream-level
	// error recorded by recordStreamError. Return it with error status to
	// match what freshPluginCallStreaming produced on the error path.
	if len(collected) == 1 && collected[0].Finish {
		written, _ := s.writeResult(ctx, m, responsePtr, collected[0].Content, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
	}
	return s.freshCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
}

func (s *execSession) freshCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {

	step := s.stepCount
	ctx, eventSpan := telemetry.EventSpan(ctx, step, "call_heartbeat", service, operation)
	defer eventSpan.End()

	type callResult struct {
		resp string
		err  error
	}
	resultCh := make(chan callResult, 1)

	// Create a cancellable context for the call so we can cancel it if
	// the workflow is cancelled during a long-running heartbeat call.
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()

	go func() {
		resp, err := s.engine.caller.Call(callCtx, service, operation, requestJSON)
		resultCh <- callResult{resp: resp, err: err}
	}()

	ticker := time.NewTicker(time.Duration(heartbeatIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeHeartbeat,
				Service:   service,
				Op:        operation,
			}
			s.recordEvent(rec)

			// Check for cancellation on each heartbeat tick.
			if s.engine.signalStore != nil {
				cancelled, _, pollErr := s.engine.signalStore.PollCancellation(ctx, "")
				if pollErr == nil && cancelled {
					cancelCall() // Cancel the in-flight call.
				}
			}

		case res := <-resultCh:
			var callErr string
			if res.err != nil {
				callErr = res.err.Error()
			}

			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeCall,
				Service:   service,
				Op:        operation,
				Request:   requestJSON,
				Response:  res.resp,
				Err:       callErr,
			}
			s.recordEvent(rec)

			if res.err != nil {
				written, _ := s.writeResult(ctx, m, responsePtr, res.err.Error(), responseMaxLen)
				return packDurableCallResult(int(written), 1, 1)
			}
			written, _ := s.writeResult(ctx, m, responsePtr, res.resp, responseMaxLen)
			return packDurableCallResult(int(written), 0, 0)
		}
	}
}

func (s *execSession) replayCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {

	// Consume any heartbeat events that occurred during the call.
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if rec.EventType == EventTypeHeartbeat {
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			continue
		}
		break
	}

	// Now find the matching call event.
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		if rec.EventType != EventTypeCall {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: expected call event, got %s.\n  actual request: %s\n  expected request: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, rec.EventType,
				truncateWithHash(requestJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Service != service || rec.Op != operation {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s.%s but history has %s.%s.\n  actual request: %s\n  expected request: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, service, operation, rec.Service, rec.Op,
				truncateWithHash(requestJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		// Detect a pending call intent: the external call was dispatched
		// but the outcome was never persisted.  Return ErrAmbiguous so
		// the caller can check the external service before retrying.
		if rec.Err == pendingSentinel {
			ambiguousCallsTotal.Inc()
			ambiguousErr := fmt.Sprintf(
				"[AMBIGUOUS] call outcome unknown at step %d: the external call to %s.%s was dispatched but the response was not recorded before a crash. Check the external service before retrying.",
				rec.Step, rec.Service, rec.Op)
			written, _ := s.writeResult(ctx, m, responsePtr, ambiguousErr, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Err != "" {
			written, _ := s.writeResult(ctx, m, responsePtr, rec.Err, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written, _ := s.writeResult(ctx, m, responsePtr, rec.Response, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	// Past recorded history — switch to fresh execution.
	s.exitReplay()
	return s.freshCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
}

const (
	sleepStatusCompleted = 0
	sleepStatusSuspend   = 1
)

func (s *execSession) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	// Sleep is local (not recorded in event history).
	// It advances virtual time by the duration and either suspends
	// (forward execution) or completes immediately (first sleep
	// after replay, which is the resume-from-sleep case).
	//
	// Local model rationale: if the worker crashes during a sequence
	// of sleeps before the next durable event, replay re-executes
	// them from scratch — which is correct because they had no
	// external side effects.
	s.nowMs += durationMs

	if s.replayJustEnded {
		// This is the sleep that originally suspended the workflow.
		// The real wait already happened (the timer fired).
		// Just advance virtual time and continue.
		s.replayJustEnded = false
		return packSleepResult(sleepStatusCompleted, 0)
	}

	// Forward execution: suspend until the sleep duration elapses.
	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("cleat_sleep(%dms)", durationMs),
		Until:  time.UnixMilli(s.nowMs),
	}

	return packSleepResult(sleepStatusSuspend, durationMs)
}

func (s *execSession) DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				written, _ := s.writeResult(ctx, m, sigNamePtr, rec.SignalName, sigNameMaxLen)
				_, _ = s.writeResult(ctx, m, payloadPtr, rec.SignalPayload, payloadMaxLen)
				return packAwaitSignalsResult(uint32(written), uint32(len(rec.SignalPayload)), false, 0)
			}
			if rec.EventType == EventTypeAwaitSignals {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				// Check if there's a following signal_received event.
				if s.stepCount < len(s.history) {
					nextRec := s.history[s.stepCount]
					if nextRec.EventType == EventTypeSignalReceived {
						if !s.advanceReplayStep(ctx, &nextRec) { return 0 }
						written, _ := s.writeResult(ctx, m, sigNamePtr, nextRec.SignalName, sigNameMaxLen)
						_, _ = s.writeResult(ctx, m, payloadPtr, nextRec.SignalPayload, payloadMaxLen)
						return packAwaitSignalsResult(uint32(written), uint32(len(nextRec.SignalPayload)), false, 0)
					}
				}
				// No signal yet — this is a replay of a wait that hasn't resolved.
				// Should not happen in practice (we only wake when signal arrives),
				// but handle gracefully.
				return packAwaitSignalsResult(0, 0, true, 0)
			}
		}
		s.exitReplay()
	}

	// Fresh execution: check signal store first.
	if s.engine.signalStore != nil {
		names := splitSignalNames(signalNames)
		for _, name := range names {
			payload, found, err := s.engine.signalStore.PollSignal(ctx, "", name)
			if err == nil && found {
				rec := EventRecord{
					Step:          s.stepCount,
					EventType:     EventTypeSignalReceived,
					SignalName:    name,
					SignalPayload: payload,
				}
				s.recordEvent(rec)

				written, _ := s.writeResult(ctx, m, sigNamePtr, name, sigNameMaxLen)
				_, _ = s.writeResult(ctx, m, payloadPtr, payload, payloadMaxLen)
				return packAwaitSignalsResult(uint32(written), uint32(len(payload)), false, 0)
			}
		}
	}

	// Record await and suspend.
	rec := EventRecord{
		Step:        s.stepCount,
		EventType:   EventTypeAwaitSignals,
		SignalNames: signalNames,
		TimeoutMs:   timeoutMs,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_signals(%s, %dms)", signalNames, timeoutMs),
		Until:  time.UnixMilli(s.nowMs).Add(time.Duration(timeoutMs) * time.Millisecond),
	}

	return packAwaitSignalsResult(0, 0, true, 0)
}

func (s *execSession) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeDefer {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }

				written, _ := s.writeResult(ctx, m, deferIDPtr, rec.DeferID, deferIDMaxLen)
				return int64(uint64(written)<<32 | 0)
			}
		}
		s.exitReplay()
	}

	deferID := fmt.Sprintf("defer-%d", s.stepCount)

	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypeDefer,
		DeferDescription: description,
		DeferID:          deferID,
	}
	s.recordEvent(rec)

	s.mu.Lock()
	s.deferrals[deferID] = description
	s.mu.Unlock()

	written, _ := s.writeResult(ctx, m, deferIDPtr, deferID, deferIDMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	// Non-durable: no event recorded, no replay matching.
	// Log output goes via the worker's stdout/stderr capture.
	return 0
}
func (s *execSession) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	if s.isReplay {
		return 0 // never cancelled during replay
	}

	if s.engine.signalStore != nil {
		cancelled, reason, err := s.engine.signalStore.PollCancellation(ctx, "")
		if err == nil && cancelled {

			_, _ = s.writeResult(ctx, m, reasonPtr, reason, reasonMaxLen)
			return int64(uint64(len(reason))<<32 | 1) // cancelled=true
		}
	}
	return 0
}

func (s *execSession) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	if s.engine.signalStore != nil {
		payload, found, err := s.engine.signalStore.PollSignal(ctx, "", signalName)
		if err == nil && found {

			written, _ := s.writeResult(ctx, m, payloadPtr, payload, payloadMaxLen)
			flags := uint32(0x0100) // found=true
			return int64(uint64(written)<<32 | uint64(flags))
		}
	}
	return 0 // not found
}

func (s *execSession) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeContinueAsNew {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				s.suspendErr = &SuspendError{
					Reason:   "continue_as_new",
					NewInput: rec.NewInput,
				}
				return 0
			}
		}
		s.exitReplay()
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeContinueAsNew,
		NewInput:  newInputJSON,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason:   "continue_as_new",
		NewInput: newInputJSON,
	}
	return 0
}

// ContinueAsNewWithVersion restarts the workflow with new input and optionally
// a new version. If newVersion is 0, uses the current version (same as ContinueAsNew).
func (s *execSession) ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeContinueAsNew {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				s.suspendErr = &SuspendError{
					Reason:     "continue_as_new",
					NewInput:   rec.NewInput,
					NewVersion: rec.NewVersion,
				}
				return 0
			}
		}
		s.exitReplay()
	}

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeContinueAsNew,
		NewInput:   newInputJSON,
		NewVersion: newVersion,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason:     "continue_as_new",
		NewInput:   newInputJSON,
		NewVersion: newVersion,
	}
	return 0
}

func (s *execSession) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	return s.childWorkflowWithVersion(ctx, m, name, inputJSON, 0, 0, "", runIDPtr, runIDMaxLen)
}

func (s *execSession) ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return s.childWorkflowWithVersion(ctx, m, name, inputJSON, int(version), int(priority), parentClosePolicy, runIDPtr, runIDMaxLen)
}

// ChildWorkflowInSchema starts a child workflow in a target PostgreSQL schema.
// This enables cross-instance cooperation: a workflow in schema A can spawn a
// child in schema B, where B's worker pool claims and executes it.
//
// The target schema MUST be in the engine's configured peerSchemas (or be the
// engine's own schema).  An empty targetSchema falls back to the local schema.
func (s *execSession) ChildWorkflowInSchema(ctx context.Context, m api.Module, targetSchema, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	// Validate: target schema must be a peer or our own schema.
	if targetSchema != "" && targetSchema != s.engine.schema {
		allowed := false
		for _, p := range s.engine.peerSchemas {
			if p == targetSchema {
				allowed = true
				break
			}
		}
		if !allowed {
			errMsg := fmt.Sprintf("child workflow %q: target schema %q is not an allowed peer", name, targetSchema)
			errWritten, _ := s.writeResult(ctx, m, runIDPtr, errMsg, runIDMaxLen)
			return int64(uint64(errWritten)<<32 | 4) // errCode 4 = invalid
		}
	}

	return s.childWorkflowWithVersion(ctx, m, name, inputJSON, int(version), int(priority), parentClosePolicy, runIDPtr, runIDMaxLen, targetSchema)
}

// childWorkflowWithVersion is the shared implementation for creating child workflows.
// If version <= 0, the parent's version is used as the default.
// If targetSchema is non-empty, the child is created in that PostgreSQL schema
// (cross-instance cooperation); otherwise the child is created locally.
func (s *execSession) childWorkflowWithVersion(ctx context.Context, m api.Module, name, inputJSON string, version int, priority int, parentClosePolicy string, runIDPtr, runIDMaxLen uint32, targetSchema ...string) int64 {
	ts := ""
	if len(targetSchema) > 0 {
		ts = targetSchema[0]
	}

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeChildWorkflow {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }

				written, _ := s.writeResult(ctx, m, runIDPtr, rec.RunID, runIDMaxLen)
				return int64(uint64(written)<<32 | 0)
			}
		}
		s.exitReplay()
	}

	// Resolve version priority:
	//   1. Explicit version from ChildWorkflowOptions (version > 0 from WASM ABI)
	//   2. Pinned child version from WASM metadata (compile-time pin)
	//   3. DB resolves version <= 0 to MAX(version) via CASE in INSERT
	// Cross-schema children skip pinned versions (target schema may differ).
	childVersion := version
	if childVersion <= 0 && ts == "" {
		if s.engine.state != nil {
			if pinnedVersion, ok := s.engine.state.ChildVersion(name); ok && pinnedVersion > 0 {
				childVersion = pinnedVersion
			}
		}
	}

	// Fresh execution: create child workflow atomically with event.
	var runID string
	parentID := s.workflowID
	if parentID == "" {
		parentID = fmt.Sprintf("unknown-%s-%d", name, s.stepCount)
	}

	if s.engine.childWfStore != nil {
		// Check child workflow quota before creating the child.
		if s.engine.maxQuotaChildren > 0 && s.engine.workflowStore != nil {
			count, err := s.engine.workflowStore.GetChildCount(context.Background(), s.workflowID)
			if err != nil {
				errMsg := fmt.Sprintf("workflow %s: failed to check child quota: %v", s.workflowID, err)
				s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
				errWritten, _ := s.writeResult(ctx, m, runIDPtr, errMsg, runIDMaxLen)
				return int64(uint64(errWritten)<<32 | 4)
			}
			if count >= s.engine.maxQuotaChildren {
				errMsg := fmt.Sprintf("workflow %s: child workflow quota exceeded (current %d, max %d)", s.workflowID, count, s.engine.maxQuotaChildren)
				s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
				errWritten, _ := s.writeResult(ctx, m, runIDPtr, errMsg, runIDMaxLen)
				return int64(uint64(errWritten)<<32 | 4)
			}
		}

		// Build the event record before the store call so the store can
		// INSERT it atomically with the child row.
		rec := EventRecord{
			Step:              s.stepCount,
			EventType:         EventTypeChildWorkflow,
			ChildName:         name,
			ChildInput:        inputJSON,
			ParentWorkflowID:  s.workflowID,
			ParentClosePolicy: parentClosePolicy,
			TimestampMs:       time.Now().UnixMilli(),
		}

		var err error
		if ts != "" {
			css, ok := s.engine.childWfStore.(CrossSchemaChildStore)
			if !ok {
				// Cross-schema requested but store doesn't support it.
				// Fail loudly rather than silently creating the child in the wrong schema.
				err := fmt.Errorf("child workflow %q: cross-schema requested (target=%q) but store does not implement CrossSchemaChildStore", name, ts)

				errWritten, _ := s.writeResult(ctx, m, runIDPtr, err.Error(), runIDMaxLen)
				return int64(uint64(errWritten)<<32 | 4) // error code 4 = invalid
			}
			runID, err = css.StartChildWorkflowInSchema(context.Background(), ts, parentID, name, inputJSON, childVersion, parentClosePolicy, priority)
		} else {
			runID, err = s.engine.childWfStore.StartChildWorkflowAtomic(context.Background(), "", parentID, name, inputJSON, childVersion, parentClosePolicy, rec, priority)
		}
		if err != nil {
			errMsg := fmt.Sprintf("child workflow %q: start failed: %v", name, err)
			s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
			errWritten, _ := s.writeResult(ctx, m, runIDPtr, errMsg, runIDMaxLen)
			return int64(uint64(errWritten)<<32 | 3) // errCode 3 = not_found
		}
		// Append event to in-memory history for same-execution replay.
		// The store already wrote it to event_history atomically;
		// the later flush will skip it via ON CONFLICT DO NOTHING.
		rec.RunID = runID
		s.history = append(s.history, rec)
		s.nowMs = rec.TimestampMs
		s.stepCount++
	} else {
		runID = fmt.Sprintf("child-%s-%d", name, s.stepCount)
		rec := EventRecord{
			Step:              s.stepCount,
			EventType:         EventTypeChildWorkflow,
			ChildName:         name,
			ChildInput:        inputJSON,
			RunID:             runID,
			ParentWorkflowID:  s.workflowID,
			ParentClosePolicy: parentClosePolicy,
		}
		s.recordEvent(rec)
	}

	written, _ := s.writeResult(ctx, m, runIDPtr, runID, runIDMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeAwaitChild {
				if rec.Response != "" || rec.Err != "" {
					// Cached result available — return it.
					if !s.advanceReplayStep(ctx, &rec) { return 0 }
					if rec.Err != "" {
						written, _ := s.writeResult(ctx, m, resultPtr, rec.Err, resultMaxLen)
						return packAwaitChildResult(uint32(written), 1)
					}
					written, _ := s.writeResult(ctx, m, resultPtr, rec.Response, resultMaxLen)
					return packAwaitChildResult(uint32(written), 0)
				}
				// No cached result yet — fall through to fresh to re-check.
				// Don't advance stepCount; the fresh execution will record
				// the result at this same step, overwriting the empty event.
				s.exitReplay()
			}
		} else {
			s.exitReplay()
		}
	}

	// Fresh execution: check child result via store.
	if s.engine.childWfStore != nil {
		result, completed, err := s.engine.childWfStore.GetChildResult(context.Background(), runID)
		if completed && err == nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeAwaitChild,
				RunID:     runID,
				Response:  result,
			}
			s.recordEvent(rec)

			written, _ := s.writeResult(ctx, m, resultPtr, result, resultMaxLen)
			return packAwaitChildResult(uint32(written), 0)
		}
		if err != nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeAwaitChild,
				RunID:     runID,
				Err:       err.Error(),
			}
			s.recordEvent(rec)

			written, _ := s.writeResult(ctx, m, resultPtr, err.Error(), resultMaxLen)
			return packAwaitChildResult(uint32(written), 1)
		}
	}

	// Child not completed — record event and suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitChild,
		RunID:     runID,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_child(%s)", runID),
	}

	return packAwaitChildResultSuspend()
}

func (s *execSession) PollChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	// Non-blocking check of a child's status. Never suspends.
	// Returns: {"status":"running|completed|failed", "result":"...", "error":"..."}

	type pollResult struct {
		Status string `json:"status"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	var pr pollResult
	if s.engine.childWfStore != nil {
		result, completed, err := s.engine.childWfStore.GetChildResult(context.Background(), runID)
		if err != nil {
			pr = pollResult{Status: "failed", Error: err.Error()}
		} else if completed {
			if result != "" {
				pr = pollResult{Status: "completed", Result: result}
			} else {
				pr = pollResult{Status: "failed", Error: "child workflow failed (empty result)"}
			}
		} else {
			pr = pollResult{Status: "running"}
		}
	} else {
		pr = pollResult{Status: "failed", Error: "no child workflow store"}
	}

	out, _ := json.Marshal(pr)
	written, _ := s.writeResult(ctx, m, resultPtr, string(out), resultMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) AwaitAnyChild(ctx context.Context, m api.Module, runIDsJSON string, resultPtr, resultMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeAwaitAnyChild {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				if rec.Response != "" {
					written, _ := s.writeResult(ctx, m, resultPtr, rec.Response, resultMaxLen)
					return int64(uint64(written)<<32 | 0)
				}
				// Empty response: this was a suspend (no child was done yet).
				// Peek at the next event — if it is also an AwaitAnyChild with
				// a non-empty response, that is the re-execution result. Consume
				// both events and return the cached result. This avoids a
				// non-deterministic fall-through to fresh where multiple children
				// might show as completed on replay.
				if s.stepCount < len(s.history) {
					nextRec := s.history[s.stepCount]
					if nextRec.EventType == EventTypeAwaitAnyChild && nextRec.Response != "" {
						if !s.advanceReplayStep(ctx, &nextRec) { return 0 }
						written, _ := s.writeResult(ctx, m, resultPtr, nextRec.Response, resultMaxLen)
						return int64(uint64(written)<<32 | 0)
					}
				}
				// No cached re-execution result — fall through to fresh.
				s.exitReplay()
			} else {
				// Event type mismatch — replay divergence.
				replayFailuresTotal.Inc()
				errMsg := fmt.Sprintf("replay divergence at step %d: expected await_any_child, got %s", rec.Step, rec.EventType)
				written, _ := s.writeResult(ctx, m, resultPtr, errMsg, resultMaxLen)
				return int64(uint64(written)<<32 | 1)
			}
		} else {
			s.exitReplay()
		}
	}

	// Fresh execution: poll children in deterministic order and return the
	// first completed one. Sorted order guarantees that replay after a suspend
	// produces the same result as the original execution when multiple children
	// happen to be complete.
	var runIDs []string
	if err := json.Unmarshal([]byte(runIDsJSON), &runIDs); err != nil {
		written, _ := s.writeResult(ctx, m, resultPtr, fmt.Sprintf(`{"error":"invalid runIDs: %v"}`, err), resultMaxLen)
		return int64(uint64(written)<<32 | 1)
	}

	// Sort for deterministic polling order.
	sort.Strings(runIDs)

	type outcome struct {
		RunID  string `json:"run_id"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	if s.engine.childWfStore != nil {
		for _, rid := range runIDs {
			result, completed, err := s.engine.childWfStore.GetChildResult(context.Background(), rid)
			if err != nil || completed {
				var out outcome
				out.RunID = rid
				if err != nil {
					out.Error = err.Error()
				} else {
					out.Result = result
				}
				outJSON, _ := json.Marshal(out)
				rec := EventRecord{
					Step:      s.stepCount,
					EventType: EventTypeAwaitAnyChild,
					Request:   runIDsJSON,
					Response:  string(outJSON),
				}
				s.recordEvent(rec)
				written, _ := s.writeResult(ctx, m, resultPtr, string(outJSON), resultMaxLen)
				return int64(uint64(written)<<32 | 0)
			}
		}
	}

	// No child completed — suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitAnyChild,
		Request:   runIDsJSON,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_any_child(%s)", runIDsJSON),
	}

	return packAwaitChildResultSuspend()
}

func (s *execSession) AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayAwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen)
	}
	return s.freshAwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen)
}

func (s *execSession) freshAwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {

	var runIDs []string
	if err := json.Unmarshal([]byte(runIDsJSON), &runIDs); err != nil {
		written, _ := s.writeResult(ctx, m, resultsPtr, fmt.Sprintf(`[{"error":"invalid runIDs: %v"}]`, err), resultsMaxLen)
		return packAwaitChildResult(uint32(written), 1)
	}

	// Concurrently await all children.
	type childOutcome struct {
		RunID  string `json:"run_id"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	outcomes := make([]childOutcome, len(runIDs))
	var wg sync.WaitGroup

	for i, runID := range runIDs {
		wg.Add(1)
		go func(idx int, rid string) {
			defer wg.Done()
			if s.engine.childWfStore != nil {
				result, completed, err := s.engine.childWfStore.GetChildResult(context.Background(), rid)
				if err != nil {
					outcomes[idx] = childOutcome{RunID: rid, Error: err.Error()}
				} else if completed {
					outcomes[idx] = childOutcome{RunID: rid, Result: result}
				} else {
					outcomes[idx] = childOutcome{RunID: rid, Error: "child not completed"}
				}
			} else {
				outcomes[idx] = childOutcome{RunID: rid, Error: "no child workflow store"}
			}
		}(i, runID)
	}
	wg.Wait()

	// Record event.
	outcomesJSON, _ := json.Marshal(outcomes)
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitAllChildren,
		Request:   runIDsJSON,
		Response:  string(outcomesJSON),
	}
	s.recordEvent(rec)

	written, _ := s.writeResult(ctx, m, resultsPtr, string(outcomesJSON), resultsMaxLen)
	return packAwaitChildResult(uint32(written), 0)
}

func (s *execSession) replayAwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		if rec.EventType != EventTypeAwaitAllChildren {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: expected await_all_children, got %s.\n  actual run IDs: %s\n  expected run IDs: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, rec.EventType,
				truncateWithHash(runIDsJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, resultsPtr, errMsg, resultsMaxLen)
			return packAwaitChildResult(uint32(written), 1)
		}

		if runIDsJSON != rec.Request {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: await_all_children run IDs mismatch.\n  actual run IDs: %s\n  expected run IDs: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step,
				truncateWithHash(runIDsJSON, maxPayloadLen),
				truncateWithHash(rec.Request, maxPayloadLen))
			written, _ := s.writeResult(ctx, m, resultsPtr, errMsg, resultsMaxLen)
			return packAwaitChildResult(uint32(written), 1)
		}

		written, _ := s.writeResult(ctx, m, resultsPtr, rec.Response, resultsMaxLen)
		return packAwaitChildResult(uint32(written), 0)
	}

	s.exitReplay()
	return s.freshAwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen)
}

func (s *execSession) DurableCallWithRetry(ctx context.Context, m api.Module,
	service, operation, requestJSON string,
	maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
	nonRetryableErrorsJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	// Worker-enforced ceiling on retry attempts to prevent runaway retries
	// from misconfigured WASM modules.  Use the engine-configured limit if
	// set (it comes from --max-retries on the command line), otherwise the
	// package-level constant.
	ceiling := MaxRetryAttempts
	if s.engine.maxRetries > 0 && s.engine.maxRetries < ceiling {
		ceiling = s.engine.maxRetries
	}
	if maxAttempts > int64(ceiling) {
		maxAttempts = int64(ceiling)
	}
	if s.isReplay {
		return s.replayCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
	}
	return s.freshCallWithRetry(ctx, m, service, operation, requestJSON,
		maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs,
		nonRetryableErrorsJSON, responsePtr, responseMaxLen)
}

func (s *execSession) freshCallWithRetry(ctx context.Context, m api.Module,
	service, operation, requestJSON string,
	maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
	nonRetryableErrorsJSON string,
	responsePtr, responseMaxLen uint32) int64 {

	// Parse non-retryable error patterns.
	var nonRetryableErrors []string
	if nonRetryableErrorsJSON != "" {
		json.Unmarshal([]byte(nonRetryableErrorsJSON), &nonRetryableErrors)
	}

	var lastErr error
	exhausted := true

	for attempt := int64(1); attempt <= maxAttempts; attempt++ {
		resp, callErr := s.engine.caller.Call(ctx, service, operation, requestJSON)

		if callErr == nil {
			// Success — record one event and return.
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeCall,
				Service:   service,
				Op:        operation,
				Request:   requestJSON,
				Response:  resp,
			}
			s.recordEvent(rec)

			written, _ := s.writeResult(ctx, m, responsePtr, resp, responseMaxLen)
			return packDurableCallResult(int(written), 0, 0)
		}

		lastErr = callErr

		// Check if error is definitively non-retryable.
		if isDefinitelyNonRetryable(callErr, nonRetryableErrors) {
			exhausted = false
			break
		}

		if attempt < maxAttempts {
			// Exponential backoff using host time (not DurableSleep).
			backoffMs := initialIntervalMs * int64(math.Pow(float64(backoffCoefficient100x)/100.0, float64(attempt-1)))
			if backoffMs > maxIntervalMs {
				backoffMs = maxIntervalMs
			}
			if backoffMs < 1 {
				backoffMs = 1 // minimum backoff to prevent a tight retry loop
			}
			select {
			case <-ctx.Done():
				return packDurableCallResult(0, 0, 0)
			case <-time.After(time.Duration(backoffMs) * time.Millisecond):
			}
		}
	}

	// All retries exhausted or non-retryable error — record failure event.
	errMsg := lastErr.Error()
	if exhausted {
		errMsg = "retries exhausted: " + errMsg
	}
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Err:       errMsg,
	}
	s.recordEvent(rec)

	written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
	return packDurableCallResult(int(written), 1, 1)
}

func (s *execSession) Version(ctx context.Context) int64 {
	if s.engine.state != nil {
		return int64(s.engine.state.Version())
	}
	return 1
}

func (s *execSession) MinVersion(ctx context.Context) int64 {
	if s.engine.state != nil {
		return int64(s.engine.state.MinVersion())
	}
	return 1
}

func (s *execSession) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	s.mu.Lock()
	if s.queryState == nil {
		s.queryState = make(map[string]string)
	}
	s.queryState[key] = value
	s.mu.Unlock()
	return 0
}

func (s *execSession) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeUpdateHandler {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				return 0
			}
		}
		s.exitReplay()
	}

	// Fresh execution: record the handler registration event.
	rec := EventRecord{
		Step:              s.stepCount,
		EventType:         EventTypeUpdateHandler,
		UpdateHandlerName: name,
	}
	s.recordEvent(rec)
	return 0
}

// exitReplay transitions from replay to forward execution.
// It sets replayJustEnded so that the first DurableSleep after replay
// can detect the resume-from-sleep case and complete without suspending.
func (s *execSession) exitReplay() {
	s.isReplay = false
	s.replayJustEnded = true
}

// recordEvent timestamps a fresh event, advances the session clock,
// and appends it to the history. It must only be called during fresh
// execution (not replay).
func (s *execSession) recordEvent(rec EventRecord) {
	if rec.TimestampMs == 0 {
		rec.TimestampMs = time.Now().UnixMilli()
	}
	s.nowMs = rec.TimestampMs
	s.history = append(s.history, rec)
	s.stepCount++

	// Persist immediately so events survive worker crashes.
	if s.engine.db != nil && !s.isReplay {
		if flushErr := s.engine.flushEvent(context.Background(), s.workflowID, rec); flushErr != nil {
			s.engine.log().ErrorContext(context.Background(), "recordEvent flushEvent failed", "workflow_id", s.workflowID, "step", rec.Step, "event_type", rec.EventType, "error", flushErr)
		}
	}
}

func (s *execSession) Now(ctx context.Context) int64 {
	// During replay, read the timestamp from the last consumed event
	// to produce deterministic Now() values matching the original
	// execution. Before any event is consumed (stepCount==0), s.nowMs
	// is seeded from the first history event or wall clock.
	if s.stepCount > 0 && s.stepCount <= len(s.history) {
		if ts := s.history[s.stepCount-1].TimestampMs; ts > 0 {
			return ts
		}
	}
	return s.nowMs
}

func (s *execSession) Random(ctx context.Context) int64 {
	// Deterministic random: seeded from workflow ID and step count.
	// On replay, stepCount is the same for each call, so Random()
	// always returns the same sequence.
	data := fmt.Sprintf("%s:%d:%d", s.workflowID, s.stepCount, s.randomSeq)
	s.randomSeq++
	hash := sha256.Sum256([]byte(data))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

func (s *execSession) CreatePromise(ctx context.Context, m api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeCreatePromise {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }

				written, _ := s.writeResult(ctx, m, promiseIDPtr, rec.PromiseID, promiseIDMaxLen)
				return packSimpleResult(0, written)
			}
		}
		s.exitReplay()
	}

	// Fresh execution: generate promise ID.
	id, err := uuid.NewRandom()
	var promiseID string
	if err != nil {
		promiseID = fmt.Sprintf("prom-%s-%d", s.workflowID, s.stepCount)
	} else {
		promiseID = id.String()
	}

	rec := EventRecord{
		Step:        s.stepCount,
		EventType:   EventTypeCreatePromise,
		PromiseName: name,
		PromiseID:   promiseID,
	}
	s.recordEvent(rec)

	// Also persist to promise store if available.
	if s.engine.promiseStore != nil {
		if err := s.engine.promiseStore.CreatePromise(ctx, s.workflowID, name, promiseID); err != nil {
			s.engine.log().ErrorContext(ctx, "create_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		}
	}

	written, _ := s.writeResult(ctx, m, promiseIDPtr, promiseID, promiseIDMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypePromiseResolved {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				written, _ := s.writeResult(ctx, m, resultPtr, rec.PromiseResult, resultMaxLen)
				return packAwaitPromiseResult(uint32(written), false, 0)
			}
			if rec.EventType == EventTypePromiseRejected {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				written, _ := s.writeResult(ctx, m, resultPtr, rec.PromiseError, resultMaxLen)
				return packAwaitPromiseResult(uint32(written), false, 1)
			}
			if rec.EventType == EventTypeAwaitPromise {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				// Promise was pending in original execution. Check if resolved now.
				s.exitReplay()
			}
		} else {
			s.exitReplay()
		}
	}

	// Fresh execution: check promise store.
	if s.engine.promiseStore != nil {
		status, result, errMsg, err := s.engine.promiseStore.GetPromise(ctx, s.workflowID, promiseID)
		if err == nil && status == "resolved" {
			rec := EventRecord{
				Step:          s.stepCount,
				EventType:     EventTypePromiseResolved,
				PromiseID:     promiseID,
				PromiseResult: result,
			}
			s.recordEvent(rec)
			written, _ := s.writeResult(ctx, m, resultPtr, result, resultMaxLen)
			return packAwaitPromiseResult(uint32(written), false, 0)
		}
		if err == nil && status == "rejected" {
			rec := EventRecord{
				Step:         s.stepCount,
				EventType:    EventTypePromiseRejected,
				PromiseID:    promiseID,
				PromiseError: errMsg,
			}
			s.recordEvent(rec)
			written, _ := s.writeResult(ctx, m, resultPtr, errMsg, resultMaxLen)
			return packAwaitPromiseResult(uint32(written), false, 1)
		}
	}

	// Record await and suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitPromise,
		PromiseID: promiseID,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_promise(%s)", promiseID),
		Until:  time.UnixMilli(s.nowMs).Add(time.Duration(timeoutMs) * time.Millisecond),
	}

	return packAwaitPromiseResult(0, true, 0)
}

func (s *execSession) SendSignalAndWait(ctx context.Context, m api.Module, targetRunID, signalName, payload string, timeoutMs int64, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }

				written, _ := s.writeResult(ctx, m, responsePtr, rec.SignalPayload, responseMaxLen)
				return packSimpleResult(0, written)
			}
		}
		s.exitReplay()
	}

	// Check signal authorization before delivering.
	if s.engine.requireSignalAuth && s.engine.signalAuthCheck != nil {
		if err := s.engine.signalAuthCheck(ctx, targetRunID, s.defName); err != nil {
			s.engine.log().ErrorContext(ctx, "signal_auth failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			return errSignalAuthRequiredInt
		}
	}

	// Fresh execution: check if target has responded via signal store.
	if s.engine.signalStore != nil {
		payload, found, err := s.engine.signalStore.PollSignal(ctx, targetRunID, signalName)
		if err == nil && found {
			rec := EventRecord{
				Step:          s.stepCount,
				EventType:     EventTypeSignalReceived,
				SignalName:    signalName,
				SignalPayload: payload,
			}
			s.recordEvent(rec)

			written, _ := s.writeResult(ctx, m, responsePtr, payload, responseMaxLen)
			return packSimpleResult(0, written)
		}
	}

	// No response yet — record event and suspend.
	rec := EventRecord{
		Step:        s.stepCount,
		EventType:   EventTypeAwaitSignals,
		SignalNames: signalName,
		TimeoutMs:   timeoutMs,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("send_signal_and_wait(%s, %s)", targetRunID, signalName),
		Until:  time.UnixMilli(s.nowMs).Add(time.Duration(timeoutMs) * time.Millisecond),
	}

	return packSimpleResult(1, 0)
}

func (s *execSession) ReplyToSignal(ctx context.Context, m api.Module, correlationID, response string) int64 {
	// Record the reply event for replay fidelity.
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				return 0
			}
		}
		s.exitReplay()
	}

	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeSignalReceived,
		SignalName:    correlationID,
		SignalPayload: response,
	}
	s.recordEvent(rec)

	return 0
}

func (s *execSession) SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64 {
	// Fire-and-forget: record the signal event.
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				if !s.advanceReplayStep(ctx, &rec) { return 0 }
				return 0
			}
		}
		s.exitReplay()
	}

	// Check signal authorization before delivering.
	if s.engine.requireSignalAuth && s.engine.signalAuthCheck != nil {
		if err := s.engine.signalAuthCheck(ctx, targetRunID, s.defName); err != nil {
			s.engine.log().ErrorContext(ctx, "signal_auth failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			return errSignalAuthRequiredInt
		}
	}

	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeSignalReceived,
		SignalName:    signalName,
		SignalPayload: payload,
		RunID:         targetRunID,
	}
	s.recordEvent(rec)

	// Deliver to target via signal store if available.
	if s.engine.signalStore != nil {
		if err := s.engine.signalStore.DeliverSignal(ctx, targetRunID, signalName, payload); err != nil {
			s.engine.log().ErrorContext(ctx, "deliver_signal failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		}
	}

	return 0
}

func (s *execSession) SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {
	if s.isReplay {
		return s.replaySetScope(ctx, m, objectType, instanceKey, prevScopePtr, prevScopeMaxLen)
	}
	return s.freshSetScope(ctx, m, objectType, instanceKey, prevScopePtr, prevScopeMaxLen)
}

func (s *execSession) ClearScope(ctx context.Context) {
	if s.scopeSet && s.scopePrefix != "" {
		scopeKey := "vo:" + s.scopeObjType + ":" + s.scopeInstKey
		if s.engine.concurrencyKeyStore != nil {
			if err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, scopeKey); err != nil {
				s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			}
		}
		for i, held := range s.heldScopes {
			if held == scopeKey {
				s.heldScopes = append(s.heldScopes[:i], s.heldScopes[i+1:]...)
				break
			}
		}
	}
	s.scopeSet = false
	s.scopePrefix = ""
	s.scopeObjType = ""
	s.scopeInstKey = ""
}

func (s *execSession) freshSetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {

	// Save previous scope prefix to output buffer.
	prevScope := ""
	if s.scopeSet && s.scopePrefix != "" {
		prevScope = s.scopePrefix
		_, _ = s.writeResult(ctx, m, prevScopePtr, prevScope, prevScopeMaxLen)
	}

	if objectType == "" && instanceKey == "" {
		s.ClearScope(ctx)
		return 0
	}

	// If switching from an existing scope, release the old key first.
	if s.scopeSet && s.scopePrefix != "" {
		oldKey := "vo:" + s.scopeObjType + ":" + s.scopeInstKey
		if s.engine.concurrencyKeyStore != nil {
			if err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, oldKey); err != nil {
				s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			}
		}
		for i, held := range s.heldScopes {
			if held == oldKey {
				s.heldScopes = append(s.heldScopes[:i], s.heldScopes[i+1:]...)
				break
			}
		}
	}

	// Acquire new scope key.
	scopeKey := "vo:" + objectType + ":" + instanceKey
	if s.engine.concurrencyKeyStore != nil {
		acquired, err := s.engine.concurrencyKeyStore.AcquireConcurrencyKey(ctx, scopeKey, s.workflowID, 24*time.Hour)
		if err != nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeScopeAcquired,
				ScopeKey:  scopeKey,
				Err:       err.Error(),
			}
			s.recordEvent(rec)
			return packSimpleResult(1, 0)
		}
		if !acquired {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeScopeAcquired,
				ScopeKey:  scopeKey,
				Err:       "scope held by another workflow",
			}
			s.recordEvent(rec)
			s.suspendErr = &SuspendError{
				Reason: fmt.Sprintf("virtual object scope %s held by another workflow", scopeKey),
				Until:  time.UnixMilli(s.nowMs).Add(5 * time.Second),
			}
			return 0
		}
		s.heldScopes = append(s.heldScopes, scopeKey)
	}

	// Record successful acquisition.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeScopeAcquired,
		ScopeKey:  scopeKey,
	}
	s.recordEvent(rec)

	s.scopeSet = true
	s.scopeObjType = objectType
	s.scopeInstKey = instanceKey
	s.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
	return 0
}

func (s *execSession) replaySetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {

	// Save previous scope prefix to output buffer (reconstructed from replayed scope state).
	prevScope := ""
	if s.scopeSet && s.scopePrefix != "" {
		prevScope = s.scopePrefix
		_, _ = s.writeResult(ctx, m, prevScopePtr, prevScope, prevScopeMaxLen)
	}

	if objectType == "" && instanceKey == "" {
		s.scopeSet = false
		s.scopePrefix = ""
		s.scopeObjType = ""
		s.scopeInstKey = ""
		return 0
	}

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		if rec.EventType != EventTypeScopeAcquired {
			return packSimpleResult(1, 0)
		}

		if rec.Err != "" {
			// Previous attempt failed.
			// Do not set scope fields; switch to fresh to retry acquisition.
			s.exitReplay()
			return s.freshSetScope(ctx, m, objectType, instanceKey, prevScopePtr, prevScopeMaxLen)
		}

		// Acquisition was successful.
		s.scopeSet = true
		s.scopeObjType = objectType
		s.scopeInstKey = instanceKey
		s.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
		s.heldScopes = append(s.heldScopes, "vo:"+objectType+":"+instanceKey)
		return 0
	}

	// Past recorded history -- switch to fresh execution.
	s.exitReplay()
	return s.freshSetScope(ctx, m, objectType, instanceKey, prevScopePtr, prevScopeMaxLen)
}

func (s *execSession) releaseHeldScopes(ctx context.Context) {
	if s.engine.concurrencyKeyStore == nil {
		return
	}
	for _, scopeKey := range s.heldScopes {
		if err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, scopeKey); err != nil {
			s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		}
	}
	s.heldScopes = nil
}

func (s *execSession) GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64 {

	var objTypeLen, instKeyLen uint32
	if s.scopeSet {
		objTypeLen, _ = s.writeResult(ctx, m, objTypePtr, s.scopeObjType, objTypeMaxLen)
		instKeyLen, _ = s.writeResult(ctx, m, instKeyPtr, s.scopeInstKey, instKeyMaxLen)
	}

	return int64(uint64(objTypeLen)<<32 | uint64(instKeyLen))
}

func (s *execSession) UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64 {

	wfID := s.workflowID
	if wfID == "" {
		wfID = "unknown"
	}
	data := wfID + ":" + seed
	hash := sha256.Sum256([]byte(data))
	// Format as UUIDv5-like value (first 16 bytes of SHA-256, version bits set).
	hash[6] = (hash[6] & 0x0f) | 0x50 // Version 5
	hash[8] = (hash[8] & 0x3f) | 0x80 // Variant 1
	uuidStr := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])

	written, _ := s.writeResult(ctx, m, uuidPtr, uuidStr, uuidMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	if s.isReplay {
		return s.replayAcquireLock(ctx, m, key, ttlMs)
	}
	return s.freshAcquireLock(ctx, m, key, ttlMs)
}

func (s *execSession) freshAcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	var acquired bool
	if s.engine.concurrencyKeyStore != nil {
		// Check concurrency key quota before acquiring.
		if s.engine.maxQuotaConcurrencyKeys > 0 && s.engine.workflowStore != nil {
			count, err := s.engine.workflowStore.GetConcurrencyKeyCount(ctx, s.workflowID)
			if err != nil {
				s.engine.log().ErrorContext(ctx, "concurrency key quota check failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
				rec := EventRecord{
					Step:         s.stepCount,
					EventType:    EventTypeAcquireLock,
					LockKey:      key,
					LockTTLMs:    ttlMs,
					LockAcquired: 0,
					Err:          err.Error(),
				}
				s.recordEvent(rec)
				return packAcquireLockResult(false, 1)
			}
			if count >= s.engine.maxQuotaConcurrencyKeys {
				errMsg := fmt.Sprintf("workflow %s: concurrency key quota exceeded (current %d, max %d)", s.workflowID, count, s.engine.maxQuotaConcurrencyKeys)
				s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
				rec := EventRecord{
					Step:         s.stepCount,
					EventType:    EventTypeAcquireLock,
					LockKey:      key,
					LockTTLMs:    ttlMs,
					LockAcquired: 0,
					Err:          errMsg,
				}
				s.recordEvent(rec)
				return packAcquireLockResult(false, 1)
			}
		}

		var err error
		acquired, err = s.engine.concurrencyKeyStore.AcquireConcurrencyKey(ctx, key, s.workflowID, time.Duration(ttlMs)*time.Millisecond)
		if err != nil {
			rec := EventRecord{
				Step:         s.stepCount,
				EventType:    EventTypeAcquireLock,
				LockKey:      key,
				LockTTLMs:    ttlMs,
				LockAcquired: 0,
				Err:          err.Error(),
			}
			s.recordEvent(rec)
			return packAcquireLockResult(false, 1)
		}
	}

	a := 0
	if acquired {
		a = 1
	}
	rec := EventRecord{
		Step:         s.stepCount,
		EventType:    EventTypeAcquireLock,
		LockKey:      key,
		LockTTLMs:    ttlMs,
		LockAcquired: a,
	}
	s.recordEvent(rec)

	return packAcquireLockResult(acquired, 0)
}

func (s *execSession) replayAcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		if rec.EventType != EventTypeAcquireLock {
			return packAcquireLockResult(false, 1)
		}
		if rec.Err != "" {
			return packAcquireLockResult(false, 1)
		}
		return packAcquireLockResult(rec.LockAcquired != 0, 0)
	}
	s.exitReplay()
	return s.freshAcquireLock(ctx, m, key, ttlMs)
}

func (s *execSession) ReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	if s.isReplay {
		return s.replayReleaseLock(ctx, m, key)
	}
	return s.freshReleaseLock(ctx, m, key)
}

func (s *execSession) freshReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	if s.engine.concurrencyKeyStore != nil {
		err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, key)
		if err != nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeReleaseLock,
				LockKey:   key,
				Err:       err.Error(),
			}
			s.recordEvent(rec)
			return int64(1)
		}
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeReleaseLock,
		LockKey:   key,
	}
	s.recordEvent(rec)

	return 0
}

func (s *execSession) replayReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		if rec.EventType != EventTypeReleaseLock {
			return int64(1)
		}
		if rec.Err != "" {
			return int64(1)
		}
		return 0
	}
	s.exitReplay()
	return s.freshReleaseLock(ctx, m, key)
}

// ---- SideEffect ----

func (s *execSession) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	if s.isReplay {
		return s.replaySideEffect(ctx, m, computedResult, respPtr, respMaxLen)
	}
	return s.freshSideEffect(ctx, m, computedResult, respPtr, respMaxLen)
}

func (s *execSession) freshSideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {

	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypeSideEffect,
		SideEffectResult: computedResult,
	}
	s.recordEvent(rec)

	written, _ := s.writeResult(ctx, m, respPtr, computedResult, respMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) replaySideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) { return 0 }

		if rec.EventType != EventTypeSideEffect {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: expected side_effect event, got %s", rec.Step, rec.EventType)
			written, _ := s.writeResult(ctx, m, respPtr, errMsg, respMaxLen)
			return packSimpleResult(1, written)
		}

		// Verify that the replayed SideEffect computedResult matches the
		// recorded value. A mismatch means the WASM module produced a
		// different result on replay — a non-determinism bug.
		if rec.SideEffectResult != computedResult {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf(
				"replay divergence at step %d: SideEffect produced %q but history recorded %q. "+
					"Your workflow may have a non-determinism bug (time.Now(), random values, "+
					"map iteration, goroutines). Run 'cleat vet' to check for common issues.",
				rec.Step, computedResult, rec.SideEffectResult,
			)
			written, _ := s.writeResult(ctx, m, respPtr, errMsg, respMaxLen)
			return packSimpleResult(1, written)
		}

		written, _ := s.writeResult(ctx, m, respPtr, rec.SideEffectResult, respMaxLen)
		return packSimpleResult(0, written)
	}

	s.exitReplay()
	return s.freshSideEffect(ctx, m, computedResult, respPtr, respMaxLen)
}

func (s *execSession) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {

	id := s.workflowID
	if id == "" {
		id = "unknown"
	}
	written, _ := s.writeResult(ctx, m, idPtr, id, idMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {

	runID := s.execRunID
	if runID == "" {
		runID = "unknown"
	}
	written, _ := s.writeResult(ctx, m, idPtr, runID, idMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType == EventTypePromiseResolved {
				return 0
			}
		}
		s.exitReplay()
	}

	// Fresh execution: record and dispatch.
	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypePromiseResolved,
		PromiseID:     promiseID,
		PromiseResult: value,
	}
	s.recordEvent(rec)

	if s.engine.promiseStore != nil {
		if err := s.engine.promiseStore.ResolvePromise(ctx, s.workflowID, promiseID, value); err != nil {
			s.engine.log().ErrorContext(ctx, "resolve_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		}
	}
	return 0
}

func (s *execSession) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType == EventTypePromiseRejected {
				return 0
			}
		}
		s.exitReplay()
	}

	// Fresh execution: record and dispatch.
	rec := EventRecord{
		Step:         s.stepCount,
		EventType:    EventTypePromiseRejected,
		PromiseID:    promiseID,
		PromiseError: errMsg,
	}
	s.recordEvent(rec)

	if s.engine.promiseStore != nil {
		if err := s.engine.promiseStore.RejectPromise(ctx, s.workflowID, promiseID, errMsg); err != nil {
			s.engine.log().ErrorContext(ctx, "reject_promise failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		}
	}
	return 0
}

func (s *execSession) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	if s.isReplay {
		// On replay, skip (fire-and-forget is recorded but not re-executed).
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
		}
		return 0
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeDurableSend,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
	}
	s.recordEvent(rec)

	// Execute the fire-and-forget call through the caller.
	// Wrap in a timeout context to bound goroutine lifetime in case
	// the external Call blocks indefinitely.
	if s.engine.caller != nil {
		go func() {
			if ctx.Err() != nil {
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			_, _ = s.engine.caller.Call(callCtx, service, operation, requestJSON)
		}()
	}
	return 0
}

func (s *execSession) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
		}
		return 0
	}

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeDurableScheduleInvoke,
		Service:    service,
		Op:         operation,
		Request:    requestJSON,
		DurationMs: delayMs,
	}
	s.recordEvent(rec)

	// Schedule the call. For now, run in a goroutine after the delay.
	// Wrap the call in a timeout context to bound goroutine lifetime
	// in case the external Call blocks indefinitely.
	if s.engine.caller != nil {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(delayMs) * time.Millisecond):
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				_, _ = s.engine.caller.Call(callCtx, service, operation, requestJSON)
			}
		}()
	}
	return 0
}

func (s *execSession) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	// Query handlers are registered but don't produce event history entries.
	// They are invoked out-of-band by the worker, not during replay.
	s.queryHandlers = append(s.queryHandlers, name)
	return 0
}

// QueryHandlers returns the list of registered query handler names.
func (s *execSession) QueryHandlers() []string { return s.queryHandlers }

// ---- Stream R host functions ----

func (s *execSession) SetState(ctx context.Context, m api.Module, key, value string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "set" || rec.StateKey != key {
				return 1
			}
			s.mu.Lock()
			if s.stateStore == nil {
				s.stateStore = make(map[string]string)
			}
			s.stateStore[key] = rec.StateValue
			s.mu.Unlock()
			return 0
		}
		s.exitReplay()
	}

	s.mu.Lock()
	if s.stateStore == nil {
		s.stateStore = make(map[string]string)
	}
	s.stateStore[key] = value
	s.mu.Unlock()

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeStateMutation,
		StateKey:   key,
		StateValue: value,
		StateOp:    "set",
	}
	s.recordEvent(rec)
	return 0
}

func (s *execSession) GetState(ctx context.Context, m api.Module, key string, valuePtr, valueMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "get" || rec.StateKey != key {
				return packSimpleResult(1, 0)
			}
			written, _ := s.writeResult(ctx, m, valuePtr, rec.StateValue, valueMaxLen)
			return packSimpleResult(0, written)
		}
		s.exitReplay()
	}

	s.mu.Lock()
	value := ""
	if s.stateStore != nil {
		value = s.stateStore[key]
	}
	s.mu.Unlock()

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeStateMutation,
		StateKey:   key,
		StateValue: value,
		StateOp:    "get",
	}
	s.recordEvent(rec)

	written, _ := s.writeResult(ctx, m, valuePtr, value, valueMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) DeleteState(ctx context.Context, m api.Module, key string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "del" || rec.StateKey != key {
				return 1
			}
			s.mu.Lock()
			if s.stateStore != nil {
				delete(s.stateStore, key)
			}
			s.mu.Unlock()
			return 0
		}
		s.exitReplay()
	}

	s.mu.Lock()
	if s.stateStore != nil {
		delete(s.stateStore, key)
	}
	s.mu.Unlock()

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeStateMutation,
		StateKey:  key,
		StateOp:   "del",
	}
	s.recordEvent(rec)
	return 0
}

// IncrState atomically increments a numeric state value.  It is NOT safe for
// concurrent access from multiple WASM modules.  The engine serialises all
// host calls within a single workflow execution, so this is never called
// concurrently in practice — speculative parallelism MUST NOT be introduced
// without adding synchronisation to IncrState.
func (s *execSession) IncrState(ctx context.Context, m api.Module, key string, delta int64) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "incr" || rec.StateKey != key {
				return 0
			}
			s.mu.Lock()
			if s.stateStore == nil {
				s.stateStore = make(map[string]string)
			}
			s.stateStore[key] = rec.StateValue
			s.mu.Unlock()
			newVal, _ := strconv.ParseInt(rec.StateValue, 10, 64)
			return newVal
		}
		s.exitReplay()
	}

	s.mu.Lock()
	if s.stateStore == nil {
		s.stateStore = make(map[string]string)
	}

	current := int64(0)
	if v, ok := s.stateStore[key]; ok {
		current, _ = strconv.ParseInt(v, 10, 64)
	}
	newVal := current + delta
	s.stateStore[key] = fmt.Sprintf("%d", newVal)
	s.mu.Unlock()

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeStateMutation,
		StateKey:   key,
		StateValue: fmt.Sprintf("%d", newVal),
		StateDelta: delta,
		StateOp:    "incr",
	}
	s.recordEvent(rec)
	return newVal
}

func (s *execSession) HasState(ctx context.Context, m api.Module, key string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "has" || rec.StateKey != key {
				return 0
			}
			if rec.StateValue == "1" {
				return 1
			}
			return 0
		}
		s.exitReplay()
	}

	s.mu.Lock()
	exists := int64(0)
	if s.stateStore != nil {
		if _, ok := s.stateStore[key]; ok {
			exists = 1
		}
	}
	s.mu.Unlock()

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeStateMutation,
		StateKey:   key,
		StateValue: fmt.Sprintf("%d", exists),
		StateOp:    "has",
	}
	s.recordEvent(rec)
	return exists
}

func (s *execSession) ListState(ctx context.Context, m api.Module, prefix string, keysPtr, keysMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "list" || rec.StateKey != prefix {
				return packSimpleResult(1, 0)
			}
			written, _ := s.writeResult(ctx, m, keysPtr, rec.StateKeys, keysMaxLen)
			return packSimpleResult(0, written)
		}
		s.exitReplay()
	}

	s.mu.Lock()
	var keys []string
	if s.stateStore != nil {
		for k := range s.stateStore {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
	}
	s.mu.Unlock()
	sort.Strings(keys)
	keysJSON, _ := json.Marshal(keys)

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeStateMutation,
		StateKey:  prefix,
		StateKeys: string(keysJSON),
		StateOp:   "list",
	}
	s.recordEvent(rec)

	written, _ := s.writeResult(ctx, m, keysPtr, string(keysJSON), keysMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) RunDetached(ctx context.Context, m api.Module, name, inputJSON string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType != EventTypeRunDetached || rec.DetachedName != name {
				return 1
			}
			return 0
		}
		s.exitReplay()
	}

	var runID string
	if s.engine.childWfStore != nil {
		rid, err := s.engine.childWfStore.StartChildWorkflow(ctx, s.workflowID, name, inputJSON, 0, "", 0)
		if err == nil {
			runID = rid
		}
	}
	if runID == "" {
		runID = fmt.Sprintf("detached-%s-%d", name, s.stepCount)
	}

	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeRunDetached,
		DetachedName:  name,
		DetachedInput: inputJSON,
		DetachedRunID: runID,
	}
	s.recordEvent(rec)
	return 0
}

func (s *execSession) Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) { return 0 }
			if rec.EventType != EventTypeFetch || rec.FetchMethod != method || rec.FetchURL != url || rec.FetchBody != body {
				replayFailuresTotal.Inc()
				errMsg := fmt.Sprintf("replay divergence at step %d: Fetch mismatch.\n  workflow: %s %s\n  history: %s %s\n  actual body: %s\n  expected body: %s\n  expected response: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
					rec.Step,
					method, url,
					rec.FetchMethod, rec.FetchURL,
					truncateWithHash(body, maxPayloadLen),
					truncateWithHash(rec.FetchBody, maxPayloadLen),
					truncateWithHash(rec.FetchResponse, maxPayloadLen))
				written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
				return packSimpleResult(1, written)
			}
			if rec.Err != "" {
				written, _ := s.writeResult(ctx, m, responsePtr, rec.Err, responseMaxLen)
				return packSimpleResult(1, written)
			}
			written, _ := s.writeResult(ctx, m, responsePtr, rec.FetchResponse, responseMaxLen)
			return packSimpleResult(0, written)
		}
		s.exitReplay()
	}

	var response string
	var fetchErr error
	if s.engine.fetcher != nil {
		response, fetchErr = s.engine.fetcher.Fetch(ctx, method, url, headersJSON, body)
	} else {
		fetchErr = errors.New("no fetcher configured")
	}

	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeFetch,
		FetchMethod:   method,
		FetchURL:      url,
		FetchHeaders:  headersJSON,
		FetchBody:     body,
		FetchResponse: response,
	}
	if fetchErr != nil {
		rec.Err = fetchErr.Error()
	}
	s.recordEvent(rec)

	if fetchErr != nil {
		written, _ := s.writeResult(ctx, m, responsePtr, fetchErr.Error(), responseMaxLen)
		return packSimpleResult(1, written)
	}

	written, _ := s.writeResult(ctx, m, responsePtr, response, responseMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) JsonParse(ctx context.Context, m api.Module, jsonPtr, jsonLen, outPtr, outMaxLen uint32) int64 {
	mem := m.Memory()
	input, ok := readWasmStringValidated(mem, jsonPtr, jsonLen, MaxWasmStringLen)
	if !ok {
		return packSimpleResult(1)
	}
	var v interface{}
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return packSimpleResult(1)
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return packSimpleResult(1)
	}
	written, err := writeWasmString(mem, outPtr, string(normalized), outMaxLen)
	if err != nil {
		return packSimpleResult(1)
	}
	return packSimpleResult(0, written)
}

func (s *execSession) JsonStringify(ctx context.Context, m api.Module, ptr, length, outPtr, outMaxLen uint32) int64 {
	mem := m.Memory()
	input, ok := readWasmStringValidated(mem, ptr, length, MaxWasmStringLen)
	if !ok {
		return packSimpleResult(1)
	}
	var v interface{}
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return packSimpleResult(1)
	}
	serialized, err := json.Marshal(v)
	if err != nil {
		return packSimpleResult(1)
	}
	written, err := writeWasmString(mem, outPtr, string(serialized), outMaxLen)
	if err != nil {
		return packSimpleResult(1)
	}
	return packSimpleResult(0, written)
}

// ---- Result packing helpers ----

func packSleepResult(status byte, durationMs int64) int64 {
	return int64(uint64(status)<<56 | uint64(durationMs)&0x00FFFFFFFFFFFFFF)
}

func packAwaitSignalsResult(sigNameLen, payloadLen uint32, timedOut bool, errCode uint32) int64 {
	toFlag := uint32(0)
	if timedOut {
		toFlag = 1
	}
	return int64(uint64(sigNameLen)<<48 | uint64(payloadLen)<<32 | uint64(toFlag)<<16 | uint64(errCode))
}

func packAwaitChildResult(written uint32, errCode uint32) int64 {
	return int64(uint64(written)<<32 | uint64(errCode))
}

func packAwaitChildResultSuspend() int64 {
	return 1 << 62
}

func packAwaitPromiseResult(resultLen uint32, timedOut bool, errCode uint16) int64 {
	toFlag := uint32(0)
	if timedOut {
		toFlag = 1
	}
	return int64(uint64(resultLen)<<32 | uint64(toFlag)<<16 | uint64(errCode))
}

func packAcquireLockResult(acquired bool, errCode uint32) int64 {
	a := uint32(0)
	if acquired {
		a = 1
	}
	return int64(uint64(a)<<8 | uint64(errCode))
}

// isDefinitelyNonRetryable checks if an error should not be retried.
// Returns true if the error's Retryable() method returns false, or if
// the error message matches any of the non-retryable patterns.
func isDefinitelyNonRetryable(err error, nonRetryablePatterns []string) bool {
	// Check if error self-reports as non-retryable via Retryable interface.
	var re RetryableError
	if errors.As(err, &re) {
		if !re.Retryable() {
			return true
		}
	}

	// Check non-retryable error substrings.
	if len(nonRetryablePatterns) > 0 {
		errMsg := err.Error()
		for _, p := range nonRetryablePatterns {
			if strings.Contains(errMsg, p) {
				return true
			}
		}
	}

	return false
}

func splitSignalNames(names string) []string {
	if names == "" {
		return nil
	}
	parts := make([]string, 0)
	start := 0
	for i := 0; i < len(names); i++ {
		if names[i] == ',' {
			parts = append(parts, names[start:i])
			start = i + 1
		}
	}
	parts = append(parts, names[start:])
	return parts
}

// stripCompactedEvents removes virtual events that were prepended from compaction
// state from the result history. This ensures the caller sees only the tail events
// plus any new events produced during this execution.
func stripCompactedEvents(history []EventRecord, compactedStep int) []EventRecord {
	if compactedStep <= 0 || compactedStep >= len(history) {
		return history
	}
	result := make([]EventRecord, len(history)-compactedStep)
	copy(result, history[compactedStep:])
	return result
}

// flushEvent writes a single event to event_history in its own transaction.
// This guarantees exactly-once: if the worker crashes before the workflow
// completes, replay will find this event and return the cached response.
// Retries with [100ms, 200ms, 400ms] backoff on transient failures.
//
// NOTE: flushEvent, flushCallIntent, and completeCallEvent use Postgres-specific
// SQL syntax ($N placeholders, ON CONFLICT). This is a known portability
// constraint -- MySQL and MSSQL workers use the batch path (appendEventsInTx
// via FinalizeWorkflowSegment) which is fully dialect-abstracted.
func (e *Engine) flushEvent(ctx context.Context, workflowID string, rec EventRecord) error {
	if e.db == nil {
		return nil
	}

	var lastErr error
	backoff := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 && attempt-1 < len(backoff) {
			select {
			case <-ctx.Done():
				return fmt.Errorf("flush event: context cancelled after %d retries: %w", attempt, ctx.Err())
			case <-time.After(backoff[attempt-1]):
			}
		}

		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			lastErr = fmt.Errorf("flush event: begin tx: %w", err)
			if attempt < len(backoff) {
				continue
			}
			break
		}

		var prevChecksum string
		if rec.Step > 1 {
			tx.QueryRowContext(ctx, `SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
				workflowID, rec.Step-1).Scan(&prevChecksum)
		}
		checksum := computeEventChecksum(rec, prevChecksum)
		payloadJSON, _ := eventRecordToPayload(rec)
		payloadArg := nullStr("")
		if len(payloadJSON) > 0 {
			payloadArg = sql.NullString{String: string(payloadJSON), Valid: true}
		}

		requestStr := tryEncodeBase64(rec.Request)
		responseStr := tryEncodeBase64(rec.Response)
		errStr := rec.Err
		sigPayload := rec.SignalPayload
		childInput := rec.ChildInput
		newInput := rec.NewInput
		pluginInput := rec.PluginInput
		pluginOutput := rec.PluginOutput
		promiseResult := rec.PromiseResult
		promiseError := rec.PromiseError

		// Encrypt sensitive payload fields when encryption is enabled.
		// On any encryption failure, abort the flush (fail-secure). Silently
		// storing plaintext when encryption is enabled would be a data leak.
		if e.encryptSensitivePayloads && e.encryption != nil {
			var encErr error

			if requestStr, encErr = e.encryption.EncryptString(rec.Request); encErr != nil {
				e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "request", "error", encErr)
				encryptionErrorsTotal.Inc()
				tx.Rollback()
				lastErr = fmt.Errorf("flush event: encrypt request: %w", encErr)
				if attempt < len(backoff) {
					continue
				}
				break
			}
			if responseStr, encErr = e.encryption.EncryptString(rec.Response); encErr != nil {
				e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "response", "error", encErr)
				encryptionErrorsTotal.Inc()
				tx.Rollback()
				lastErr = fmt.Errorf("flush event: encrypt response: %w", encErr)
				if attempt < len(backoff) {
					continue
				}
				break
			}
			if errStr, encErr = e.encryption.EncryptString(rec.Err); encErr != nil {
				e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "err", "error", encErr)
				encryptionErrorsTotal.Inc()
				tx.Rollback()
				lastErr = fmt.Errorf("flush event: encrypt err: %w", encErr)
				if attempt < len(backoff) {
					continue
				}
				break
			}
			if rec.SignalPayload != "" {
				if sigPayload, encErr = e.encryption.EncryptString(rec.SignalPayload); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "signal_payload", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt signal_payload: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.ChildInput != "" {
				if childInput, encErr = e.encryption.EncryptString(rec.ChildInput); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "child_input", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt child_input: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.NewInput != "" {
				if newInput, encErr = e.encryption.EncryptString(rec.NewInput); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "new_input", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt new_input: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.PluginInput != "" {
				if pluginInput, encErr = e.encryption.EncryptString(rec.PluginInput); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "plugin_input", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt plugin_input: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.PluginOutput != "" {
				if pluginOutput, encErr = e.encryption.EncryptString(rec.PluginOutput); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "plugin_output", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt plugin_output: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.PromiseResult != "" {
				if promiseResult, encErr = e.encryption.EncryptString(rec.PromiseResult); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "promise_result", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt promise_result: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			if rec.PromiseError != "" {
				if promiseError, encErr = e.encryption.EncryptString(rec.PromiseError); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "promise_error", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt promise_error: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
			}
			// Encrypt payload JSON when present.
			if len(payloadJSON) > 0 {
				var encrypted []byte
				if encrypted, encErr = e.encryption.EncryptJSON(payloadJSON); encErr != nil {
					e.log().ErrorContext(ctx, "encryption failed", "workflow_id", workflowID, "tenant_id", e.tenantID, "field", "payload", "error", encErr)
					encryptionErrorsTotal.Inc()
					tx.Rollback()
					lastErr = fmt.Errorf("flush event: encrypt payload: %w", encErr)
					if attempt < len(backoff) {
						continue
					}
					break
				}
				payloadArg = sql.NullString{String: string(encrypted), Valid: true}
			}
		}

		// Quota check: read current count without incrementing.
		// Increment happens in appendEventsInTx (via FinalizeWorkflowSegment)
		// to avoid double-counting with flushEvent's own increment.
		// Note: event_count is read before appendEventsInTx increments it.
		// Within a single execution segment, multiple flushEvent calls see the
		// same count, allowing the quota to be exceeded by the segment size.
		// This is intentional: the quota is a soft backstop, not an exact cap.
		// The atomic increment in appendEventsInTx determines the final count.
		if e.maxQuotaEvents > 0 && e.workflowStore != nil {
			var currentCount int
			qErr := tx.QueryRowContext(ctx, `SELECT event_count FROM workflow_instances WHERE id = $1`, workflowID).Scan(&currentCount)
			if qErr != nil {
				tx.Rollback()
				lastErr = fmt.Errorf("flush event: quota check: %w", qErr)
				if attempt < len(backoff) {
					continue
				}
				break
			}
			if currentCount >= e.maxQuotaEvents {
				tx.Rollback()
				return fmt.Errorf("flush event: event quota exceeded (max %d)", e.maxQuotaEvents)
			}
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error,
				duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
				defer_description, defer_id, child_name, child_input, run_id, new_input,
				plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
				promise_name, promise_id, promise_result, promise_error, payload,
				checksum, created_at, tenant_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
				$9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
				$20, $21, $22, $23, $24, $25, $26, $27, $28, $29,
				$30, NOW(), $31)
			ON CONFLICT (workflow_id, step) DO UPDATE SET response = EXCLUDED.response, error = EXCLUDED.error WHERE event_history.response = '' AND event_history.error IS NULL
		`, workflowID, rec.Step, rec.EventType,
			nullStr(rec.Service), nullStr(rec.Op), nullStr(requestStr), nullStr(responseStr), nullStr(errStr),
			nullInt64(rec.DurationMs), nullStr(rec.SignalNames), nullInt64(rec.TimeoutMs),
			nullStr(rec.SignalName), nullStr(sigPayload),
			nullStr(rec.DeferDescription), nullStr(rec.DeferID),
			nullStr(rec.ChildName), nullStr(childInput), nullStr(rec.RunID), nullStr(newInput),
			nullStr(rec.PluginName), nullStr(rec.PluginFunc), nullStr(pluginInput), nullStr(pluginOutput), nullStr(rec.PluginError),
			nullStr(rec.PromiseName), nullStr(rec.PromiseID), nullStr(promiseResult), nullStr(promiseError),
			payloadArg,
			checksum, e.tenantID)
		if err != nil {
			tx.Rollback()
			lastErr = fmt.Errorf("flush event: exec: %w", err)
			if attempt < len(backoff) {
				continue
			}
			break
		}

		if err := tx.Commit(); err != nil {
			tx.Rollback()
			lastErr = fmt.Errorf("flush event: commit: %w", err)
			if attempt < len(backoff) {
				continue
			}
			break
		}

		return nil
	}

	// All retries exhausted --- log a structured error that can be alerted on.
	e.log().ErrorContext(ctx, "flushEvent retries exhausted", "workflow_id", workflowID, "tenant_id", e.tenantID, "step", rec.Step, "event_type", rec.EventType, "error", lastErr)
	return lastErr

}

// pendingSentinel is stored in the error column of event_history to mark a
// DurableCall whose external call has been dispatched but whose outcome is
// not yet persisted.  On replay, a pending event means the call outcome is
// ambiguous — the external service may have processed it.
const pendingSentinel = "__CLEAT_PENDING_INTENT__"

// PendingSentinel is the exported form of pendingSentinel, provided so that
// external packages (notably the integrity test suite) can reference it
// without duplicating the sentinel value.
const PendingSentinel = pendingSentinel

// flushCallIntent inserts a pending event BEFORE the external call is
// dispatched.  This provides a durable record of intent: if the worker
// crashes after the external call succeeds but before the response is
// persisted, replay will find the pending event and return ErrAmbiguous.
func (e *Engine) flushCallIntent(ctx context.Context, workflowID string, rec EventRecord) error {
	if e.db == nil {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("flush call intent: begin tx: %w", err)
	}
	defer tx.Rollback()

	var prevChecksum string
	if rec.Step > 1 {
		tx.QueryRowContext(ctx, `SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
			workflowID, rec.Step-1).Scan(&prevChecksum)
	}
	checksum := computeEventChecksum(rec, prevChecksum)
	_, err = tx.ExecContext(ctx, `
			INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error, checksum)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (workflow_id, step) DO NOTHING
		`, workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(rec.Request), nullStr(rec.Response), pendingSentinel,
		checksum)
	if err != nil {
		return fmt.Errorf("flush call intent: exec: %w", err)
	}
	return tx.Commit()
}

// completeCallEvent updates a previously-flushed pending event with the
// actual call response (or error).  This transitions the event from the
// pending state to the completed state so that replay returns the cached
// response rather than ErrAmbiguous.  The checksum is recomputed from the
// full event record (which the caller stashes in rec) so that integrity
// verification remains consistent.
func (e *Engine) completeCallEvent(ctx context.Context, workflowID string, rec EventRecord, callErr string) error {
	if e.db == nil {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("complete call event: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Recompute the checksum with the actual response.
	completed := rec
	completed.Err = callErr
	var prevChecksum string
	if completed.Step > 1 {
		tx.QueryRowContext(ctx, `SELECT COALESCE(checksum, '') FROM event_history WHERE workflow_id = $1 AND step = $2`,
			workflowID, completed.Step-1).Scan(&prevChecksum)
	}
	checksum := computeEventChecksum(completed, prevChecksum)

	responseStr := nullStr(rec.Response)
	errorStr := nullStr(callErr)
	if e.encryptSensitivePayloads && e.encryption != nil {
		s, err := e.encryption.EncryptString(rec.Response)
		if err != nil {
			e.log().ErrorContext(ctx, "encryption failed for response", "workflow_id", workflowID, "tenant_id", e.tenantID, "step", rec.Step, "error", err)
			encryptionErrorsTotal.Inc()
			tx.Rollback()
			return fmt.Errorf("complete call event: encrypt response: %w", err)
		}
		responseStr = nullStr(s)
		s, err = e.encryption.EncryptString(callErr)
		if err != nil {
			e.log().ErrorContext(ctx, "encryption failed for error", "workflow_id", workflowID, "tenant_id", e.tenantID, "step", rec.Step, "error", err)
			encryptionErrorsTotal.Inc()
			tx.Rollback()
			return fmt.Errorf("complete call event: encrypt error: %w", err)
		}
		errorStr = nullStr(s)
	}

	result, err := tx.ExecContext(ctx, `
			UPDATE event_history
			SET response = $1, error = $2, checksum = $6
			WHERE workflow_id = $3 AND step = $4 AND error = $5
		`, responseStr, errorStr, workflowID, rec.Step, pendingSentinel, checksum)
	if err != nil {
		return fmt.Errorf("complete call event: exec: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete call event: rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("completeCallEvent: no rows updated for workflow %s step %d — the event may have been completed by another worker", workflowID, rec.Step)
	}
	return tx.Commit()
}

// runDefers invokes registered defer functions on a fresh module instance.
// Called on non-suspend errors to ensure cleanup runs even when the workflow fails.
func (e *Engine) runDefers(ctx context.Context, wasmBytes []byte, deferrals map[string]string) {
	type defEntry struct {
		id     string
		desc   string
		stepNo int // parsed from defer ID "defer-N"
	}
	var entries []defEntry
	for id, desc := range deferrals {
		stepNo := parseDeferStepNo(id)
		entries = append(entries, defEntry{id: id, desc: desc, stepNo: stepNo})
	}
	// Sort defers in LIFO order (higher stepNo first) so the most recently
	// registered defer runs first. Uses sort.Slice for clarity.
	sort.Slice(entries, func(i, j int) bool {
		return parseDeferStepNo(entries[i].id) < parseDeferStepNo(entries[j].id)
	})
	for _, entry := range entries {
		// Use the same naming convention as invokeDefersOnTrap:
		// "cleat_defer_" + deferID so both paths find the same export.
		deferName := "cleat_defer_" + entry.id
		if wasmBytes != nil {
			_, err := e.RunDefer(ctx, wasmBytes, deferName, nil)
			if err != nil {
				// Defer failures are not propagated — cleanup runs best-effort.
			}
		}
	}
}

// parseDeferStepNo extracts the numeric step from a defer ID of the form "defer-N".
// Returns -1 if the ID does not match the expected pattern.
func parseDeferStepNo(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "defer-%d", &n); err != nil {
		return -1
	}
	return n
}
