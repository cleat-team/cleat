package host

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/rcownie/cleat/internal/plugin"
)

// MaxRetryAttempts is the worker-enforced ceiling for DurableCallWithRetry
// maxAttempts, preventing a misconfigured WASM module from retrying forever.
const MaxRetryAttempts = 100

// EventType classifies event history records.
type EventType string

const (
	EventTypeCall             EventType = "call"
	EventTypeAwaitSignals     EventType = "await_signals"
	EventTypeSignalReceived   EventType = "signal_received"
	EventTypeDefer            EventType = "defer"
	EventTypeChildWorkflow    EventType = "child_workflow"
	EventTypeAwaitChild       EventType = "await_child"
	EventTypeContinueAsNew    EventType = "continue_as_new"
	EventTypeHeartbeat        EventType = "heartbeat"
	EventTypeAwaitAllChildren EventType = "await_all_children"
	EventTypePluginCall       EventType = "plugin_call"
	EventTypeCreatePromise    EventType = "create_promise"
	EventTypeAwaitPromise     EventType = "await_promise"
	EventTypePromiseResolved  EventType = "promise_resolved"
	EventTypePromiseRejected  EventType = "promise_rejected"
	EventTypeUpdateHandler    EventType = "update_handler"
	EventTypeStateMutation    EventType = "state_mutation"
	EventTypeRunDetached      EventType = "run_detached"
	EventTypePluginCallStreamChunk EventType = "plugin_call_stream_chunk"
	EventTypeDurableLog       EventType = "durable_log"
	EventTypeAcquireLock      EventType = "acquire_lock"
	EventTypeReleaseLock      EventType = "release_lock"
	EventTypeSideEffect            EventType = "side_effect"
	EventTypeScopeAcquired         EventType = "scope_acquired"
	EventTypeDurableSend           EventType = "durable_send"
	EventTypeDurableScheduleInvoke EventType = "durable_schedule_invoke"
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
	ChildName        string `json:"child_name,omitempty"`
	ChildInput       string `json:"child_input,omitempty"`
	RunID            string `json:"run_id,omitempty"`
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

	// Lock fields.
	LockKey      string `json:"lock_key,omitempty"`
	LockTTLMs    int64  `json:"lock_ttl_ms,omitempty"`
	LockAcquired int    `json:"lock_acquired,omitempty"`

	// SideEffect fields.
	SideEffectResult string `json:"side_effect_result,omitempty"`
	
	// Scope / virtual object fields.
	ScopeKey     string `json:"scope_key,omitempty"`

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
	StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string) (string, error)
	GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error)
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
	rt              *Runtime
	caller          ServiceCaller
	signalStore     SignalStore
	promiseStore    PromiseStore
	state           WorkflowState
	workflowID      string
	childWfStore    ChildWorkflowStore
	concurrencyKeyStore ConcurrencyKeyStore
	compactionState *CompactionState
	pluginRegistry    *PluginRegistry
	pluginStreamRegistry *PluginStreamRegistry
	updateHandler         func(name, payload string) (string, error)
	pluginCallGuard      *PluginCallGuard
	tenantID             string
	db                   *sql.DB // tenant-scoped database connection for plugin host functions
	maxRetries           int     // worker-configured ceiling for retry attempts; 0 means use MaxRetryAttempts

	// versionValidateFn is an optional hook called at the start of replay
	// to validate version compatibility. If non-nil and the workflow is
	// replaying (history is non-nil), the engine calls this before execution.
	// If it returns an error, execution is aborted.
	versionValidateFn func() error
}

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

// WithChildWorkflowStore sets the store used to create and poll child workflows.
func WithChildWorkflowStore(cws ChildWorkflowStore) EngineOption {
	return func(e *Engine) { e.childWfStore = cws }
}

func WithConcurrencyKeyStore(cks ConcurrencyKeyStore) EngineOption {
	return func(e *Engine) { e.concurrencyKeyStore = cks }
}

// WithCompactionState sets the compaction state for replaying a compacted workflow.
func WithCompactionState(cs *CompactionState) EngineOption {
	return func(e *Engine) { e.compactionState = cs }
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

// WithPluginCallGuard sets the plugin call guard for enforcing call_plugin
// capability restrictions on WASM plugins.
func WithPluginCallGuard(g *PluginCallGuard) EngineOption {
	return func(e *Engine) { e.pluginCallGuard = g }
}

// WithDB sets a tenant-scoped database connection for plugin host functions.
func WithDB(db *sql.DB) EngineOption {
	return func(e *Engine) { e.db = db }
}

// WithMaxRetryAttempts sets a worker-configured ceiling on retry attempts
// for DurableCallWithRetry, overriding MaxRetryAttempts when set to a
// positive value less than the constant.
func WithMaxRetryAttempts(n int) EngineOption {
	return func(e *Engine) { e.maxRetries = n }
}

// WithVersionValidation sets an optional function called at the start of
// replay (when event history is non-nil) to validate version compatibility.
// If the function returns an error, execution is aborted.
func WithVersionValidation(fn func() error) EngineOption {
	return func(e *Engine) { e.versionValidateFn = fn }
}

// NewEngine creates an Engine backed by the given Runtime and ServiceCaller.
func NewEngine(rt *Runtime, caller ServiceCaller, opts ...EngineOption) *Engine {
	e := &Engine{rt: rt, caller: caller}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Execute runs a fresh execution of the workflow and returns the result
// along with the complete event history. If the workflow suspends (sleep,
// await signals), it returns a nil result with non-nil SuspendResult.
// deferrals maps deferID -> description for any defers registered during execution.
// queryState contains key-value state set via SetQueryState during execution.
func (e *Engine) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage) (result string, history []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
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
		engine:     e,
		history:    replayHistory,
		isReplay:   len(replayHistory) > 0,
		nowMs:      now,
		deferrals:  make(map[string]string),
		workflowID: e.workflowID,
		execRunID:  e.workflowID,
		tenantID:   e.tenantID,
	}

	execCtx := withHandler(ctx, session)

	// If replaying and a version validation hook is set, run it now.
	if len(replayHistory) > 0 && e.versionValidateFn != nil {
		if err := e.versionValidateFn(); err != nil {
			return "", nil, nil, nil, nil, fmt.Errorf("host: version validation failed: %w", err)
		}
	}

	result, err := e.rt.CallExport(execCtx, mod, entryPoint, input)
	if err != nil {
		if errors.Is(err, ErrSuspended) || session.suspendErr != nil {
			se := session.suspendErr
			if se == nil {
				se = &SuspendError{Reason: "workflow suspended"}
			}
			strippedHistory := stripCompactedEvents(session.history, compactedStep)
			return "", strippedHistory, &SuspendResult{
				History:      session.history,
				SuspendUntil: se.Until,
				Reason:       se.Reason,
				NewInput:     se.NewInput,
				NewVersion:   se.NewVersion,
				Deferrals:    session.deferrals,
			}, session.deferrals, session.queryState, nil
		}
		// Workflow failed with a non-suspend error. Try running defers on the
		// still-live module first, then fall back to fresh-module defers.
		if len(session.deferrals) > 0 {
			e.invokeDefersOnTrap(execCtx, mod, session.deferrals)
			e.runDefers(ctx, wasmBytes, session.deferrals)
		}
		session.releaseHeldScopes(ctx)
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
			log.Printf("[engine] defer %q (%s): export %q not found", deferID, description, exportName)
			continue
		}
		_, _, err := e.rt.CallExportWithSuspend(ctx, mod, exportName, []byte("{}"))
		if err != nil {
			log.Printf("[engine] defer %q (%s) failed: %v", deferID, description, err)
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
	engine     *Engine
	history    []EventRecord
	stepCount  int
	isReplay   bool
	replayJustEnded bool // true when replay just ended (first sleep after replay completes)
	nowMs      int64
	randomSeq  int64 // monotonic counter for deterministic Random()
	suspendErr *SuspendError
	signals    map[string]string // pending signals delivered during this session
	deferrals  map[string]string // registered defer callbacks (deferID -> description)
	workflowID  string            // parent workflow instance ID (for child workflows)
	execRunID   string            // current execution run ID
	queryState  map[string]string // key-value state set via SetQueryState
	tenantID          string            // tenant ID injected into plugin function context
	callerPluginName  string            // for WASM plugins, the calling plugin's name (for call_plugin enforcement)
	queryHandlers     []string          // registered query handler names

	// Scope management for virtual object instances.
	scopePrefix  string // "vo:<type>:<key>:" prefix, empty if no scope
	scopeObjType string // current object type in scope
	scopeInstKey string // current instance key in scope
	scopeSet     bool   // true when scope is active
	heldScopes   []string // concurrency keys held for virtual object scopes
}

var _ HostHandler = (*execSession)(nil)

// ---- HostHandler implementation ----

func (s *execSession) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
	}
	return s.freshCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
}

func (s *execSession) freshCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	mem := m.Memory()

	durableCallsTotal.Inc()
	freshStepsTotal.Inc()

	// Check cancellation before making the call.
	callCtx := ctx
	if s.engine.signalStore != nil {
		cancelled, _, err := s.engine.signalStore.PollCancellation(ctx, "")
		if err == nil && cancelled {
			written, _ := writeWasmString(mem, responsePtr, "workflow cancelled", responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}
	}

	resp, err := s.engine.caller.Call(callCtx, service, operation, requestJSON)

	var callErr string
	if err != nil {
		callErr = err.Error()
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Response:  resp,
		Err:       callErr,
	}
	s.recordEvent(rec)

	// Flush the event immediately to guarantee exactly-once: if the worker
	// crashes before the workflow completes, replay will find this event
	// and return the cached response.
	if s.engine.db != nil {
		s.engine.flushEvent(ctx, s.workflowID, rec)
	}

	if err != nil {
		written, _ := writeWasmString(mem, responsePtr, err.Error(), responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}

	written, _ := writeWasmString(mem, responsePtr, resp, responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) replayCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	mem := m.Memory()

	replayStepsTotal.Inc()

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		s.stepCount++

		if rec.EventType != EventTypeCall {
			errMsg := fmt.Sprintf("replay divergence at step %d: expected call event, got %s. Run 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).", rec.Step, rec.EventType)
			written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Service != service || rec.Op != operation {
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s.%s but history has %s.%s. Run 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, service, operation, rec.Service, rec.Op)
			written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Err != "" {
			written, _ := writeWasmString(mem, responsePtr, rec.Err, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written, _ := writeWasmString(mem, responsePtr, rec.Response, responseMaxLen)
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
	mem := m.Memory()

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		s.stepCount++

		if rec.EventType != EventTypePluginCall {
			errMsg := fmt.Sprintf("replay divergence at step %d: expected plugin_call event, got %s. Run 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).", rec.Step, rec.EventType)
			written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.PluginName != pluginName || rec.PluginFunc != functionName {
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s/%s but history has %s/%s. Run 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, pluginName, functionName, rec.PluginName, rec.PluginFunc)
			written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
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
			written, _ := writeWasmString(mem, responsePtr, rec.PluginError, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written, _ := writeWasmString(mem, responsePtr, rec.PluginOutput, responseMaxLen)
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
	mem := m.Memory()

	// Look up the plugin function.
	if s.engine.pluginRegistry == nil {
		errMsg := fmt.Sprintf("plugin function %s/%s not available: no plugin registry configured. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}
	fn, idempotent, ok := s.engine.pluginRegistry.Lookup(pluginName, functionName)
	if !ok {
		errMsg := fmt.Sprintf("plugin function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}

	// Check plugin call guard (enforces call_plugin capability for WASM plugins).
	if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
		if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
			errMsg := err.Error()
			written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}
	}

	// Inject call context (tenant ID + workflow ID) for plugin functions.
	callCtx := ctx
	cc := &plugin.CallContext{}
	if s.tenantID != "" {
		tid, err := uuid.Parse(s.tenantID)
		if err == nil {
			cc.TenantID = tid
		}
	}
	if s.workflowID != "" {
		cc.WorkflowID = s.workflowID
	}
	if s.engine.db != nil {
		cc.DB = s.engine.db
	}
	callCtx = plugin.WithCallContext(callCtx, cc)

	// Actually call the plugin.
	outputJSON, err := fn(callCtx, inputJSON)

	var errStr string
	if err != nil {
		errStr = err.Error()
	}

	// Record in event history (unless this is an idempotent re-invocation
	// where the event is already in history).
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
	}

	if err != nil {
		written, _ := writeWasmString(mem, responsePtr, errStr, responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}

	written, _ := writeWasmString(mem, responsePtr, outputJSON, responseMaxLen)
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

func (s *execSession) freshPluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	mem := m.Memory()

	// Look up the streaming plugin function.
	if s.engine.pluginStreamRegistry == nil {
		errMsg := "plugin_call_streaming: no plugin stream registry configured"
		written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	fn, ok := s.engine.pluginStreamRegistry.Lookup(pluginName, functionName)
	if !ok {
		errMsg := fmt.Sprintf("plugin stream function %s/%s not registered. Check that the plugin is deployed and its version satisfies the workflow's plugin_deps.", pluginName, functionName)
		written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	// Check plugin call guard for streaming calls too.
	if s.engine.pluginCallGuard != nil && s.callerPluginName != "" {
		if err := s.engine.pluginCallGuard.Check(s.callerPluginName, pluginName); err != nil {
			errMsg := err.Error()
			written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 0, 1)
		}
	}

	// Inject call context.
	callCtx := ctx
	cc := &plugin.CallContext{}
	if s.tenantID != "" {
		tid, err := uuid.Parse(s.tenantID)
		if err == nil {
			cc.TenantID = tid
		}
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
		written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
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
		written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	written, _ := writeWasmString(mem, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) replayPluginCallStreaming(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON string,
	responsePtr, responseMaxLen uint32) int64 {
	mem := m.Memory()

	var collected []plugin.StreamEvent
	index := 0

	// Read consecutive stream chunk events from history.
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if rec.EventType != EventTypePluginCallStreamChunk {
			break
		}
		s.stepCount++

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

	// Return collected chunks as JSON.
	outJSON, err := json.Marshal(collected)
	if err != nil {
		errMsg := fmt.Sprintf("plugin_call_streaming %s/%s: marshal chunks: %v", pluginName, functionName, err)
		written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
		return packDurableCallResult(int(written), 0, 1)
	}

	written, _ := writeWasmString(mem, responsePtr, string(outJSON), responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
	}
	return s.freshCallWithHeartbeat(ctx, m, service, operation, requestJSON, heartbeatIntervalMs, responsePtr, responseMaxLen)
}

func (s *execSession) freshCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	mem := m.Memory()

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
				written, _ := writeWasmString(mem, responsePtr, res.err.Error(), responseMaxLen)
				return packDurableCallResult(int(written), 1, 1)
			}
			written, _ := writeWasmString(mem, responsePtr, res.resp, responseMaxLen)
			return packDurableCallResult(int(written), 0, 0)
		}
	}
}

func (s *execSession) replayCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	mem := m.Memory()

	// Consume any heartbeat events that occurred during the call.
	for s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if rec.EventType == EventTypeHeartbeat {
			s.stepCount++
			continue
		}
		break
	}

	// Now find the matching call event.
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		s.stepCount++

		if rec.EventType != EventTypeCall {
			errMsg := fmt.Sprintf("replay divergence at step %d: expected call event, got %s. Run 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).", rec.Step, rec.EventType)
			written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Service != service || rec.Op != operation {
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s.%s but history has %s.%s. Run 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
				rec.Step, service, operation, rec.Service, rec.Op)
			written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Err != "" {
			written, _ := writeWasmString(mem, responsePtr, rec.Err, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written, _ := writeWasmString(mem, responsePtr, rec.Response, responseMaxLen)
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
	mem := m.Memory()

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSignalReceived {
				s.stepCount++
				written, _ := writeWasmString(mem, sigNamePtr, rec.SignalName, sigNameMaxLen)
				_, _ = writeWasmStringOrTrap(mem, payloadPtr, rec.SignalPayload, payloadMaxLen)
				return packAwaitSignalsResult(uint32(written), uint32(len(rec.SignalPayload)), false, 0)
			}
			if rec.EventType == EventTypeAwaitSignals {
				s.stepCount++
				// Check if there's a following signal_received event.
				if s.stepCount < len(s.history) {
					nextRec := s.history[s.stepCount]
					if nextRec.EventType == EventTypeSignalReceived {
						s.stepCount++
						written, _ := writeWasmString(mem, sigNamePtr, nextRec.SignalName, sigNameMaxLen)
						_, _ = writeWasmStringOrTrap(mem, payloadPtr, nextRec.SignalPayload, payloadMaxLen)
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

				written, _ := writeWasmString(mem, sigNamePtr, name, sigNameMaxLen)
				_, _ = writeWasmStringOrTrap(mem, payloadPtr, payload, payloadMaxLen)
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
				s.stepCount++
				mem := m.Memory()
				written, _ := writeWasmString(mem, deferIDPtr, rec.DeferID, deferIDMaxLen)
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

	s.deferrals[deferID] = description

	mem := m.Memory()
	written, _ := writeWasmString(mem, deferIDPtr, deferID, deferIDMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeDurableLog,
		Message:   message,
	}

	// Parse structured log fields from JSON if present.
	if len(message) > 0 && message[0] == '{' {
		var logEntry struct {
			Level   string            `json:"level"`
			Message string            `json:"message"`
			KV      map[string]string `json:"kv"`
		}
		if err := json.Unmarshal([]byte(message), &logEntry); err == nil {
			rec.LogLevel = logEntry.Level
			rec.Message = logEntry.Message
			if kvJSON, err := json.Marshal(logEntry.KV); err == nil {
				rec.LogKV = string(kvJSON)
			}
		}
	}

	if s.isReplay {
		if s.stepCount < len(s.history) {
			next := s.history[s.stepCount]
			if next.EventType == EventTypeDurableLog {
				s.stepCount++
				return 0
			}
		}
		return 0
	}

	s.recordEvent(rec)
	return 0
}

func (s *execSession) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	if s.isReplay {
		return 0 // never cancelled during replay
	}

	if s.engine.signalStore != nil {
		cancelled, reason, err := s.engine.signalStore.PollCancellation(ctx, "")
		if err == nil && cancelled {
			mem := m.Memory()
			_, _ = writeWasmStringOrTrap(mem, reasonPtr, reason, reasonMaxLen)
			return int64(uint64(len(reason))<<32 | 1) // cancelled=true
		}
	}
	return 0
}

func (s *execSession) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	if s.engine.signalStore != nil {
		payload, found, err := s.engine.signalStore.PollSignal(ctx, "", signalName)
		if err == nil && found {
			mem := m.Memory()
			written, _ := writeWasmString(mem, payloadPtr, payload, payloadMaxLen)
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
				s.stepCount++
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
				s.stepCount++
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
	// Use parent's version as default. ResolveChildVersion with opts.Version <= 0
	// will fall through to the parent's version rule.
	parentVersion := 1
	if s.engine.state != nil {
		parentVersion = s.engine.state.Version()
	}
	return s.childWorkflowWithVersion(ctx, m, name, inputJSON, parentVersion, "", runIDPtr, runIDMaxLen)
}

func (s *execSession) ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return s.childWorkflowWithVersion(ctx, m, name, inputJSON, int(version), parentClosePolicy, runIDPtr, runIDMaxLen)
}

// childWorkflowWithVersion is the shared implementation for creating child workflows.
// If version <= 0, the parent's version is used as the default.
func (s *execSession) childWorkflowWithVersion(ctx context.Context, m api.Module, name, inputJSON string, version int, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeChildWorkflow {
				s.stepCount++
				mem := m.Memory()
				written, _ := writeWasmString(mem, runIDPtr, rec.RunID, runIDMaxLen)
				return int64(uint64(written)<<32 | 0)
			}
		}
		s.exitReplay()
	}

	// Fresh execution: create child workflow via store or generate synthetic ID.
	var runID string
	if s.engine.childWfStore != nil {
		parentID := s.workflowID
		if parentID == "" {
			parentID = fmt.Sprintf("unknown-%s-%d", name, s.stepCount)
		}
		var err error
		// Resolve version: if > 0 use explicit, otherwise use parent's version.
		childVersion := version
		if childVersion <= 0 {
			if s.engine.state != nil {
				childVersion = s.engine.state.Version()
			} else {
				childVersion = 0
			}
		}
		runID, err = s.engine.childWfStore.StartChildWorkflow(ctx, parentID, name, inputJSON, childVersion, parentClosePolicy)
		if err != nil {
			runID = fmt.Sprintf("child-%s-%d", name, s.stepCount)
		}
	} else {
		runID = fmt.Sprintf("child-%s-%d", name, s.stepCount)
	}

	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypeChildWorkflow,
		ChildName:        name,
		ChildInput:       inputJSON,
		RunID:            runID,
		ParentWorkflowID: s.workflowID,
			ParentClosePolicy: parentClosePolicy,
	}
	s.recordEvent(rec)

	mem := m.Memory()
	written, _ := writeWasmString(mem, runIDPtr, runID, runIDMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	mem := m.Memory()

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeAwaitChild {
				if rec.Response != "" || rec.Err != "" {
					// Cached result available — return it.
					s.stepCount++
					if rec.Err != "" {
						written, _ := writeWasmString(mem, resultPtr, rec.Err, resultMaxLen)
						return packAwaitChildResult(uint32(written), 1)
			}
					written, _ := writeWasmString(mem, resultPtr, rec.Response, resultMaxLen)
					return packAwaitChildResult(uint32(written), 0)
				}
				// No cached result yet — fall through to fresh to re-check.
				s.stepCount++
				s.exitReplay()
			}
		} else {
			s.exitReplay()
		}
	}

	// Fresh execution: check child result via store.
	if s.engine.childWfStore != nil {
		result, completed, err := s.engine.childWfStore.GetChildResult(ctx, runID)
		if completed && err == nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeAwaitChild,
				RunID:     runID,
				Response:  result,
			}
			s.recordEvent(rec)

			written, _ := writeWasmString(mem, resultPtr, result, resultMaxLen)
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

			written, _ := writeWasmString(mem, resultPtr, err.Error(), resultMaxLen)
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

func (s *execSession) AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	if s.isReplay {
		return s.replayAwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen)
	}
	return s.freshAwaitAllChildren(ctx, m, runIDsJSON, resultsPtr, resultsMaxLen)
}

func (s *execSession) freshAwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	mem := m.Memory()

	var runIDs []string
	if err := json.Unmarshal([]byte(runIDsJSON), &runIDs); err != nil {
		written, _ := writeWasmString(mem, resultsPtr, fmt.Sprintf(`[{"error":"invalid runIDs: %v"}]`, err), resultsMaxLen)
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
				result, completed, err := s.engine.childWfStore.GetChildResult(ctx, rid)
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

	written, _ := writeWasmString(mem, resultsPtr, string(outcomesJSON), resultsMaxLen)
	return packAwaitChildResult(uint32(written), 0)
}

func (s *execSession) replayAwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	mem := m.Memory()

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		s.stepCount++

		if rec.EventType != EventTypeAwaitAllChildren {
			errMsg := fmt.Sprintf("replay divergence at step %d: expected await_all_children, got %s. Run 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).", rec.Step, rec.EventType)
			written, _ := writeWasmString(mem, resultsPtr, errMsg, resultsMaxLen)
			return packAwaitChildResult(uint32(written), 1)
		}

		written, _ := writeWasmString(mem, resultsPtr, rec.Response, resultsMaxLen)
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

	mem := m.Memory()

	// Parse non-retryable error patterns.
	var nonRetryableErrors []string
	if nonRetryableErrorsJSON != "" {
		json.Unmarshal([]byte(nonRetryableErrorsJSON), &nonRetryableErrors)
	}

	var lastErr error

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

			written, _ := writeWasmString(mem, responsePtr, resp, responseMaxLen)
			return packDurableCallResult(int(written), 0, 0)
		}

		lastErr = callErr

		// Check if error is definitively non-retryable.
		if isDefinitelyNonRetryable(callErr, nonRetryableErrors) {
			break
		}

		if attempt < maxAttempts {
			// Exponential backoff using host time (not DurableSleep).
			backoffMs := initialIntervalMs * int64(math.Pow(float64(backoffCoefficient100x)/100.0, float64(attempt-1)))
			if backoffMs > maxIntervalMs {
				backoffMs = maxIntervalMs
			}
			if backoffMs > 0 {
				select {
				case <-ctx.Done():
					return packDurableCallResult(0, 0, 0)
				case <-time.After(time.Duration(backoffMs) * time.Millisecond):
				}
			}
		}
	}

	// All retries exhausted or non-retryable error — record failure event.
	errMsg := lastErr.Error()
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Err:       errMsg,
	}
	s.recordEvent(rec)

	written, _ := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
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
	if s.queryState == nil {
		s.queryState = make(map[string]string)
	}
	s.queryState[key] = value
	return 0
}

func (s *execSession) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeUpdateHandler {
				s.stepCount++
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
				s.stepCount++
				mem := m.Memory()
				written, _ := writeWasmString(mem, promiseIDPtr, rec.PromiseID, promiseIDMaxLen)
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
			log.Printf("[engine] create_promise: %v", err)
		}
	}

	mem := m.Memory()
	written, _ := writeWasmString(mem, promiseIDPtr, promiseID, promiseIDMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	mem := m.Memory()

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypePromiseResolved {
				s.stepCount++
				written, _ := writeWasmString(mem, resultPtr, rec.PromiseResult, resultMaxLen)
				return packAwaitPromiseResult(uint32(written), false, 0)
			}
			if rec.EventType == EventTypePromiseRejected {
				s.stepCount++
				written, _ := writeWasmString(mem, resultPtr, rec.PromiseError, resultMaxLen)
				return packAwaitPromiseResult(uint32(written), false, 1)
			}
			if rec.EventType == EventTypeAwaitPromise {
				s.stepCount++
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
			written, _ := writeWasmString(mem, resultPtr, result, resultMaxLen)
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
			written, _ := writeWasmString(mem, resultPtr, errMsg, resultMaxLen)
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
				s.stepCount++
				mem := m.Memory()
				written, _ := writeWasmString(mem, responsePtr, rec.SignalPayload, responseMaxLen)
				return packSimpleResult(0, written)
			}
		}
		s.exitReplay()
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

			mem := m.Memory()
			written, _ := writeWasmString(mem, responsePtr, payload, responseMaxLen)
			return packSimpleResult(0, written)
		}
	}

	// No response yet — record event and suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitSignals,
		SignalNames: signalName,
		TimeoutMs: timeoutMs,
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
				s.stepCount++
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
				s.stepCount++
				return 0
			}
		}
		s.exitReplay()
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
			log.Printf("[engine] deliver_signal: %v", err)
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
				log.Printf("[engine] release_concurrency_key: %v", err)
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
	mem := m.Memory()

	// Save previous scope prefix to output buffer.
	prevScope := ""
	if s.scopeSet && s.scopePrefix != "" {
		prevScope = s.scopePrefix
		_, _ = writeWasmStringOrTrap(mem, prevScopePtr, prevScope, prevScopeMaxLen)
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
				log.Printf("[engine] release_concurrency_key: %v", err)
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
				Until:   time.UnixMilli(s.nowMs).Add(5 * time.Second),
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
	mem := m.Memory()

	// Save previous scope prefix to output buffer (reconstructed from replayed scope state).
	prevScope := ""
	if s.scopeSet && s.scopePrefix != "" {
		prevScope = s.scopePrefix
		_, _ = writeWasmStringOrTrap(mem, prevScopePtr, prevScope, prevScopeMaxLen)
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
		s.stepCount++

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
			log.Printf("[engine] release_concurrency_key: %v", err)
		}
	}
	s.heldScopes = nil
}

func (s *execSession) GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64 {
	mem := m.Memory()

	var objTypeLen, instKeyLen uint32
	if s.scopeSet {
		objTypeLen, _ = writeWasmString(mem, objTypePtr, s.scopeObjType, objTypeMaxLen)
		instKeyLen, _ = writeWasmString(mem, instKeyPtr, s.scopeInstKey, instKeyMaxLen)
	}

	return int64(uint64(objTypeLen)<<32 | uint64(instKeyLen))
}

func (s *execSession) UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64 {
	mem := m.Memory()

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

	written, _ := writeWasmString(mem, uuidPtr, uuidStr, uuidMaxLen)
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
		s.stepCount++

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
		s.stepCount++

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
		return s.replaySideEffect(ctx, m, respPtr, respMaxLen)
	}
	return s.freshSideEffect(ctx, m, computedResult, respPtr, respMaxLen)
}

func (s *execSession) freshSideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	mem := m.Memory()

	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypeSideEffect,
		SideEffectResult: computedResult,
	}
	s.recordEvent(rec)

	written, _ := writeWasmString(mem, respPtr, computedResult, respMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) replaySideEffect(ctx context.Context, m api.Module, respPtr, respMaxLen uint32) int64 {
	mem := m.Memory()

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		s.stepCount++

		if rec.EventType != EventTypeSideEffect {
			return packSimpleResult(1, 0)
		}

		written, _ := writeWasmString(mem, respPtr, rec.SideEffectResult, respMaxLen)
		return packSimpleResult(0, written)
	}

	s.exitReplay()
	return s.freshSideEffect(ctx, m, "", respPtr, respMaxLen)
}

func (s *execSession) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	mem := m.Memory()
	id := s.workflowID
	if id == "" {
		id = "unknown"
	}
	written, _ := writeWasmString(mem, idPtr, id, idMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	mem := m.Memory()
	runID := s.execRunID
	if runID == "" {
		runID = "unknown"
	}
	written, _ := writeWasmString(mem, idPtr, runID, idMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			s.stepCount++
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
			log.Printf("[engine] resolve_promise: %v", err)
		}
	}
	return 0
}

func (s *execSession) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			s.stepCount++
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
			log.Printf("[engine] reject_promise: %v", err)
		}
	}
	return 0
}

func (s *execSession) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	if s.isReplay {
		// On replay, skip (fire-and-forget is recorded but not re-executed).
		if s.stepCount < len(s.history) {
			s.stepCount++
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
	if s.engine.caller != nil {
		go func() {
			if ctx.Err() != nil {
				return
			}
			_, _ = s.engine.caller.Call(ctx, service, operation, requestJSON)
		}()
	}
	return 0
}

func (s *execSession) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			s.stepCount++
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
	if s.engine.caller != nil {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(delayMs) * time.Millisecond):
				_, _ = s.engine.caller.Call(ctx, service, operation, requestJSON)
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
func (e *Engine) flushEvent(ctx context.Context, workflowID string, rec EventRecord) error {
	if e.db == nil {
		return nil
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("flush event: begin tx: %w", err)
	}
	defer tx.Rollback()

	checksum := computeEventChecksum(rec)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error, checksum)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`, workflowID, rec.Step, rec.EventType,
		nullStr(rec.Service), nullStr(rec.Op), nullStr(rec.Request), nullStr(rec.Response), nullStr(rec.Err),
		checksum)
	if err != nil {
		return fmt.Errorf("flush event: exec: %w", err)
	}
	return tx.Commit()
}

// runDefers invokes registered defer functions on a fresh module instance.
// Called on non-suspend errors to ensure cleanup runs even when the workflow fails.
func (e *Engine) runDefers(ctx context.Context, wasmBytes []byte, deferrals map[string]string) {
	type defEntry struct {
		id   string
		desc string
	}
	var entries []defEntry
	for id, desc := range deferrals {
		entries = append(entries, defEntry{id: id, desc: desc})
	}
	// LIFO: higher step number (embedded in defer ID "defer-N") runs first.
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].id > entries[i].id {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	for _, entry := range entries {
		deferName := "__defer_" + entry.desc
		if wasmBytes != nil {
			_, err := e.RunDefer(ctx, wasmBytes, deferName, nil)
			if err != nil {
				// Defer failures are not propagated — cleanup runs best-effort.
			}
		}
	}
}
