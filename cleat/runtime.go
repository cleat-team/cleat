// Package cleat defines the shared types and host interface between
// the WASM workflow module and the host runtime.
//
// This is the "durable SDK" — the only import the workflow author needs.
// Everything else comes from ordinary Go and the code transformer.
//
// NOTE: This SDK runtime intentionally uses Go features (goroutines,
// channels, interface calls, function-value calls, timers) that are
// forbidden in user workflow code. These are safe here because this
// is the runtime implementation, not a user workflow.
package cleat

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// SuspendSentinel is a sentinel panic value used to suspend workflow execution.
// When the host returns a suspend signal (e.g., from DurableSleep), the
// WASM adapter panics with this value. The export wrapper catches it and
// returns a suspend status code to the engine.
type SuspendSentinel struct{}

func (SuspendSentinel) Error() string { return "durable: workflow suspended" }

// ErrSuspend is the sentinel value panicked to suspend a workflow.
var ErrSuspend error = SuspendSentinel{}

// CronSchedule describes a scheduled workflow trigger created by ScheduleCron.
type CronSchedule struct {
	ScheduleID   string `json:"schedule_id"`
	WorkflowName string `json:"workflow_name"`
	CronExpr     string `json:"cron_expr"`
	Timezone     string `json:"timezone"`
	Input        string `json:"input"`
	Enabled      bool   `json:"enabled"`
}

// Caller is the interface for making durable calls to external services.
// It groups all methods that invoke remote operations (API calls, plugins,
// HTTP fetches, side effects).
type Caller interface {
	// Call is an alias for DurableCall. Makes or replays an API call.
	// Prefer Call over DurableCall for consistency across SDKs.
	Call(service, operation, requestJSON string) (responseJSON string, err error)

	// DurableCall makes or replays an API call (service, operation, request).
	DurableCall(service, operation, requestJSON string) (responseJSON string, err error)

	// DurableCallJSON makes a durable API call and unmarshals the response
	// JSON into result (which must be a pointer).
	DurableCallJSON(service, operation, requestJSON string, result interface{}) error

	// DurableCallTyped marshals request to JSON, makes a durable API call,
	// and unmarshals the response into result. Eliminates manual JSON from
	// call sites entirely.
	DurableCallTyped(service, operation string, request, result interface{}) error

	// DurableCallTypedWithOptions marshals request to JSON, makes a durable API call
	// with call-level options, and unmarshals the response into result.
	DurableCallTypedWithOptions(opts CallOptions, service, operation string, request, result interface{}) error

	// DurableCallWithOptions makes a durable API call with call-level options
	// such as retry policy.
	DurableCallWithOptions(opts CallOptions, service, operation, requestJSON string) (string, error)

	// DurableCallJSONWithOptions makes a durable API call with options and
	// unmarshals the response JSON into result.
	DurableCallJSONWithOptions(opts CallOptions, service, operation, requestJSON string, result interface{}) error

	// DurableCallWithHeartbeat makes a long-running durable API call and
	// invokes onProgress periodically with status updates from the engine.
	// The heartbeatInterval controls how often the host sends progress
	// events. onProgress receives a JSON string with implementation-specific
	// progress details. Falls back to a regular DurableCall if the host
	// does not support heartbeats.
	DurableCallWithHeartbeat(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(progressJSON string)) (string, error)

	// DurableCallTypedWithHeartbeat is like DurableCallWithHeartbeat but marshals
	// request to JSON and unmarshals the response into result.
	DurableCallTypedWithHeartbeat(service, operation string, request, result interface{}, heartbeatInterval time.Duration, onProgress func(progressJSON string)) error

	// PluginCall invokes a named function on a registered plugin.
	PluginCall(pluginName, functionName, inputJSON string) (string, error)

	// PluginCallStreaming calls a plugin function that returns a stream of events.
	// On first execution, calls the plugin and records each stream event in history.
	// On replay, replays events from history.
	// Returns a channel that receives StreamEvent chunks.
	PluginCallStreaming(pluginName, functionName, inputJSON string) (<-chan StreamEvent, error)

	// DurableFetch makes an HTTP request as a durable operation.
	// Delegates to DurableCall("http", "fetch", requestJSON) internally.
	DurableFetch(url, method string, headers map[string]string, body string) (responseJSON string, statusCode int, err error)

	// DurableFetchJSON is like DurableFetch but unmarshals the response into result.
	DurableFetchJSON(url, method string, headers map[string]string, body string, result interface{}) error

	// FetchGet is a shorthand for DurableFetch with GET method, no headers, no body.
	FetchGet(url string) (responseJSON string, statusCode int, err error)

	// FetchGetJSON is like FetchGet but unmarshals the response into result.
	FetchGetJSON(url string, result interface{}) error

	// DurableSend sends a one-way (fire-and-forget) message to a service.
	DurableSend(service, operation, requestJSON string) error

	// ScheduleInvoke schedules a one-way message to be delivered after a delay.
	ScheduleInvoke(service, operation, requestJSON string, delayMs int64) error

	// SideEffect executes a non-deterministic function, records its result
	// in event history on first execution, and returns the cached result on
	// replay. The fn is always called on first execution; on replay, the
	// cached result from history is returned and fn is NOT called (the
	// computed result from fn is ignored on replay).
	SideEffect(fn func() (string, error)) (string, error)
}

// Timer provides durable, deterministic time operations.
// Use instead of time.Now() and time.Sleep() in workflow code.
//
// Durable time semantics:
//
// Now() returns the timestamp of the most recent durable event in the
// workflow's execution history. On first execution, durable events are
// timestamped with wall-clock time. During replay, the recorded timestamps
// are played back identically, so Now() returns the same values it returned
// during the original execution.
//
// Between durable events (e.g., between two DurableCall operations),
// Now() returns the timestamp of the last event — virtual time does not
// advance during non-durable CPU work. Only DurableSleep advances the
// virtual clock: after DurableSleep(5*time.Second), Now() returns the
// pre-sleep time plus 5 seconds.
//
// At the replay frontier — where recorded history ends and the workflow
// resumes forward progress — Now() jumps to the current wall-clock time.
// This "time skip" is expected: the workflow was suspended waiting for
// an external event, and Now() reflects when that event arrived.
//
// Never call time.Now() or time.Sleep() in workflow code. These read the
// real system clock, which differs across replays and breaks determinism.
// The cleat vet tool (E003 check) catches this at build time. In WASM
// mode, host time functions are disabled at the WASI level.
type Timer interface {
	// DurableSleep suspends the workflow for the given duration.
	// After resuming, Now() reflects the time after the sleep.
	DurableSleep(d time.Duration)

	// DurableSleepMs suspends the workflow for the given milliseconds.
	// Prefer DurableSleep(time.Duration) for readability.
	DurableSleepMs(ms int64)

	// Now returns the deterministic current time. See Timer doc for
	// the full durable time semantics.
	Now() time.Time

	// NowMs returns Now() in milliseconds since Unix epoch.
	// Prefer Now() for readability.
	NowMs() int64
}

// Signaler provides durable signal operations for workflow communication.
// Signals allow workflows to communicate with each other and with external
// systems in a deterministic, replay-safe manner.
type Signaler interface {
	// AwaitSignals blocks until one of the named signals arrives or the
	// timeout expires. Returns a structured SignalResult.
	AwaitSignals(signalNames []string, timeout time.Duration) SignalResult

	// DurableAwaitSignals is the low-level signal wait. Prefer AwaitSignals.
	DurableAwaitSignals(signalNames []string, timeoutMs int64) (signalName, payload string, timedOut bool, err error)

	// SendSignalAndWait sends a signal to another workflow and waits for a response.
	// The signal is sent with an embedded correlation ID; the target workflow uses
	// ReplyToSignal to send a response back.
	SendSignalAndWait(targetRunID, signalName, payload string, timeout time.Duration) (response string, err error)

	// ReplyToSignal sends a response back to the sender of a signal identified by
	// the given correlation ID. Only valid inside a signal handler context where
	// the correlation ID was embedded in the received signal payload.
	ReplyToSignal(correlationID, response string) error

	// AwaitSignalsWithQuorum waits for at least minCount signals from the named
	// set, up to an optional maxRejections threshold, within the given timeout.
	// Returns the collected signals or an error if quorum was not met.
	// When maxRejections >= 0, signals whose JSON payload contains "rejected":true
	// count toward the rejection limit; exceeding it aborts the wait.
	AwaitSignalsWithQuorum(signalNames []string, minCount int, maxRejections int, timeout time.Duration) ([]SignalResult, error)

	// SignalWorkflow sends a signal to another workflow from within a workflow.
	// This is a recorded (journaled) operation that delivers a signal to the
	// target workflow's signal queue. Unlike SendSignalAndWait, this is
	// fire-and-forget -- the caller does not wait for a response.
	SignalWorkflow(targetRunID, signalName, payload string) error

	// PollSignal checks for a non-blocking signal.
	PollSignal(signalName string) (payload string, found bool, err error)

	// PollSignals checks for any of the named signals without blocking.
	// Returns immediately. If a matching signal is pending, returns it with
	// TimedOut=false. If none, returns with TimedOut=true.
	// This is the non-blocking counterpart to AwaitSignals.
	PollSignals(names []string) SignalResult
}

// Lifecycle provides workflow lifecycle management: versioning, child workflows,
// cancellation, logging, continuation, and deferred cleanup.
type Lifecycle interface {
	// ContinueAsNew creates a new workflow run with fresh event history,
	// passing the current state as input.
	ContinueAsNew(newInputJSON string) error

	// ContinueAsNewWithVersion restarts the workflow with new input and
	// optionally a new version. If newVersion is 0, uses the current version.
	ContinueAsNewWithVersion(newInputJSON string, newVersion int64) error

	// DurableDefer registers a function to run when the workflow exits
	// (LIFO order, always runs even on error). Returns a deferID.
	// Use NewSaga() for structured compensation patterns.
	DurableDefer(description string) (deferID string, err error)

	// DurableDeferFunc registers a function (as a closure) to run when the
	// workflow exits (LIFO order, always runs even on error). Unlike DurableDefer
	// which takes a description string, this takes a function that is called
	// at cleanup time. Returns a deferID.
	DurableDeferFunc(fn func()) (deferID string, err error)

	// WorkflowID returns the unique identifier for the current workflow execution.
	WorkflowID() string

	// RunID returns the run identifier for the current workflow execution.
	RunID() string

	// DurableLog emits a log message recorded in the event history.
	DurableLog(message string)

	// LogKV emits a structured key-value log message recorded in the
	// event history. Keys and values alternate: LogKV("msg", "key1", val1, "key2", val2).
	LogKV(message string, kvs ...interface{})

	// Log is an alias for LogKV. Emits a structured key-value log message.
	// Prefer Log over LogKV for consistency across SDKs.
	Log(message string, kvs ...interface{})

	// PollCancellation checks whether the workflow has been requested to
	// cancel. Returns true and the reason if so.
	PollCancellation() (cancelled bool, reason string)

	// ChildWorkflow starts a child workflow with its own event history.
	ChildWorkflow(name string, inputJSON string) (runID string, err error)

	// ChildWorkflowWithOptions starts a child workflow with version options.
	// Use ChildWorkflowOptions to pin a specific child version.
	// When opts.Version is 0 (default), the child uses the parent's version.
	ChildWorkflowWithOptions(name string, inputJSON string, opts ChildWorkflowOptions) (runID string, err error)

	// AwaitChild waits for a child workflow to complete.
	AwaitChild(runID string) (resultJSON string, err error)

	// AwaitAllChildren waits for all child workflows identified by runIDs to
	// complete. Results are returned in the same order as runIDs. Unlike
	// calling AwaitChild in a loop, all children are awaited concurrently.
	AwaitAllChildren(runIDs []string) ([]ChildResult, error)

	// AwaitAnyChild blocks until at least one child workflow from the set completes.
	// Returns the runID, result, and any error. This is "wait for any child."
	AwaitAnyChild(runIDs []string) (completedRunID string, result string, err error)

	// PollChild checks a child's status without blocking.
	// Returns status ("running", "completed", "failed"), result, and any error.
	PollChild(runID string) (status string, result string, err error)

	// ChildWorkflowTyped starts a child workflow with typed input.
	// Marshals request to JSON internally. Use AwaitChildTyped to
	// get the typed result.
	ChildWorkflowTyped(name string, request interface{}) (runID string, err error)

	// AwaitChildTyped waits for a child workflow and unmarshals its result.
	AwaitChildTyped(runID string, result interface{}) error

	// RunDetached runs fn with a fresh HostCalls that ignores cancellation.
	// fn executes immediately, is recorded in history, and survives crash/replay.
	// On replay, fn IS re-executed (not replayed from cache).
	RunDetached(fn func(h HostCalls) error) error

	// Version returns the current workflow version number for schema evolution.
	Version() int

	// MinVersion declares the minimum version this code requires. If a
	// workflow replays against newer code than it started with, the runtime
	// can detect version skew.
	MinVersion() int

	// RegisterUpdateHandler registers a handler for the named update.
	// handler receives payload JSON and returns result JSON.
	// validator runs first (read-only). Called during workflow init, before durable ops.
	RegisterUpdateHandler(name string, handler func(payloadJSON string) (resultJSON string, err error), validator func(payloadJSON string) error)

	// ScheduleCron creates a recurring workflow trigger from a cron expression.
	// cronExpr is a standard 5-field cron expression, timezone is an IANA timezone
	// name (e.g. "America/New_York"), inputJSON is the workflow input.
	// Returns the schedule ID on success.
	ScheduleCron(workflowName, cronExpr, timezone, inputJSON string) (scheduleID string, err error)

	// DeleteCron removes a recurring workflow trigger by schedule ID.
	DeleteCron(scheduleID string) error

	// ListCrons returns all recurring workflow triggers as a JSON array of
	// CronSchedule entries.
	ListCrons() (string, error)
}

// Promises provides durable promise operations for external caller interaction.
type Promises interface {
	CreatePromise(name string) (promiseID string, err error)
	AwaitPromise(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)
}

// StateManager provides durable key-value state operations scoped to the workflow.
type StateManager interface {
	SetQueryState(key, value string)
	SetState(key string, value interface{})
	GetState(key string, result interface{}) error
	DeleteState(key string)
	HasState(key string) bool
	IncrState(key string, delta int64) int64
	ListState(prefix string) []string
}

// UpdateHandlers provides workflow update-handler registration.
//
// There is no query-handler counterpart. Cleat previously exposed
// RegisterQueryHandler (removed 2026-08-09; see docs/determinism.md, "Why
// there is no RegisterQueryHandler"): it recorded a handler name but nothing
// in the worker ever routed an external query to it, so the API always did
// less than an SDK user reading its doc comment would expect. Use
// SetQueryState to publish state and GetQueryState (or
// GET /api/workflows/:id/query?key=...) to read it instead.
type UpdateHandlers interface {
	RegisterUpdateHandler(name string, handler func(payloadJSON string) (resultJSON string, err error), validator func(payloadJSON string) error)
}

// CronScheduler provides durable cron schedule operations.
type CronScheduler interface {
	ScheduleCron(workflowName, cronExpr, timezone, inputJSON string) (scheduleID string, err error)
	DeleteCron(scheduleID string) error
	ListCrons() (string, error)
}

// Scoper provides virtual object instance scoping.
type Scoper interface {
	SetScope(objectType, instanceKey string) (previousScope string)
	GetScope() (objectType, instanceKey string)
	ClearScope() (previousScope string)
}

// UUIDGenerator provides deterministic UUID generation.
type UUIDGenerator interface {
	UUID(seed string) string
	NewUUID() string
	NewUUIDv7() string
}

// Locker provides distributed concurrency lock operations.
type Locker interface {
	AcquireLock(key string, ttl time.Duration) (acquired bool, err error)
	ReleaseLock(key string) error
	AcquireLockMs(key string, ttlMs int64) (acquired bool, err error)
	AwaitCondition(predicate func() bool, pollInterval, timeout time.Duration) (met bool)
}

// RandomSource provides a deterministic random number generator.
type RandomSource interface {
	Random() int64
}

// HostCalls is the composite interface that workflow code programs against.
// It provides durable, deterministic access to external services, time, signals,
// HostCalls is the composite interface that workflow code programs against.
// It provides durable, deterministic access to external services, time, signals,
// workflow lifecycle, state, promises, randomness, and more.
//
// The interface is composed from capability-grouped sub-interfaces:
//   - Caller: durable service calls, plugins, HTTP, side effects
//   - Timer: deterministic time and sleep
//   - Signaler: signal communication between workflows
//   - Lifecycle: versioning, child workflows, cancellation, logging, defer
//   - Promises: durable promise operations
//   - StateManager: durable key-value state
//   - UpdateHandlers: workflow update-handler registration
//   - CronScheduler: durable cron schedule operations
//   - Scoper: virtual object instance scoping
//   - UUIDGenerator: deterministic UUID generation
//   - Locker: distributed concurrency lock operations
//   - RandomSource: deterministic random number generation
//
// Entry points receive a HostCalls as their first parameter. Helper functions
// in the durable closure can thread it through their call chains (manually or
// via the auto-threading transformer).
//
// The concrete implementation is *hostCallsImpl, created by the WASM host
// adapter at runtime. For tests, mock implementations of individual group
// interfaces (e.g., Caller, Timer) can be used where the full HostCalls
// HostCalls is a lightweight wrapper passed to workflows by value.
// It embeds a pointer to HostCallsImpl which holds all the function pointers.
// Passing by value avoids interface dispatch, which prevents WASM function
// table issues in Go 1.24+ WASI.
//
// Workflows use h.DurableCall, h.DurableSleep, h.Now, h.AwaitSignals,
// h.SetQueryState, h.CreatePromise, etc. to record their side effects
// in the event history.
type HostCalls struct {
	*HostCallsImpl
}

// ---- Streaming types ----

// StreamEvent represents a single chunk of a streaming response.
// Index is 0-based, Content holds the delta or accumulated text, and Finish
// is true for the last chunk.
type StreamEvent struct {
	Index   int    `json:"i"`
	Content string `json:"c"`
	Finish  bool   `json:"f"`
}

// StreamResult holds the complete stream result after all chunks are consumed.
type StreamResult struct {
	Events []StreamEvent `json:"events"`
	Final  string        `json:"final"` // reconstructed full response
}

// ---- Result types ----

// SignalResult is the structured result of AwaitSignals.

// CallResult is the serialized result of a durable API call.
type CallResult struct {
	Service   string `json:"service"`
	Operation string `json:"operation"`
	Request   string `json:"request"`
	Response  string `json:"response,omitempty"`
	Err       string `json:"err,omitempty"`
}

// Checkpoint is the persistent state of a workflow at a point in time.
type Checkpoint struct {
	WorkflowID  string       `json:"workflow_id"`
	Step        int          `json:"step"`
	CallHistory []CallResult `json:"call_history"`
	Complete    bool         `json:"complete"`
	FinalResult string       `json:"final_result,omitempty"`
	FinalErr    string       `json:"final_err,omitempty"`
}

// ---- Structured error types ----

// CallErrorCode classifies durable call failures so callers can distinguish
// retryable from non-retryable errors without string-matching.
type CallErrorCode int

const (
	CallErrorUnknown          CallErrorCode = iota
	CallErrorTimeout                        // retryable
	CallErrorUnavailable                    // retryable
	CallErrorNotFound                       // non-retryable
	CallErrorInvalidRequest                 // non-retryable
	CallErrorPermissionDenied               // non-retryable
	// CallErrorRetryPolicyTooLong means the host declined to run this retry
	// policy in one segment because its worst-case total backoff exceeds the
	// tenant's host-retry budget. The call did NOT happen and the host
	// recorded no event, so no attempt has been consumed; the caller should
	// run the policy itself, suspending between attempts.
	//
	// Non-retryable on purpose, and the default `Retryable()` arm gives that
	// for free. Re-issuing cleat_call_retry would be refused again on
	// identical grounds and loop forever -- see ABI.md, "Retry refusal".
	CallErrorRetryPolicyTooLong // non-retryable
)

// CallError is a structured error returned by DurableCall and its variants.
type CallError struct {
	Service   string
	Operation string
	Code      CallErrorCode
	Message   string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("durable call %s.%s: [%d] %s", e.Service, e.Operation, e.Code, e.Message)
}

// Retryable returns true if the error code indicates a transient failure.
func (e *CallError) Retryable() bool {
	switch e.Code {
	case CallErrorTimeout, CallErrorUnavailable:
		return true
	default:
		return false
	}
}

// ---- Virtual Object definitions ----

// VirtualObjectDef describes a virtual object type for key-scoped
// stateful services.
type VirtualObjectDef struct {
	// Name is the unique name for this virtual object type.
	Name string

	// EntryPoint is the function that handles invocations for this
	// virtual object type. It receives a HostCalls (with state scoped
	// to the instance) and the input JSON, and returns the result JSON
	// or an error.
	EntryPoint func(h HostCalls, input string) (string, error)
}

// virtualObjectRegistry is the package-level registry of virtual object
// definitions.
var virtualObjectRegistry = struct {
	mu   sync.RWMutex
	defs map[string]VirtualObjectDef
}{
	defs: make(map[string]VirtualObjectDef),
}

// RegisterVirtualObject registers a virtual object definition in the
// global registry. Returns an error if a definition with the same name
// already exists or if the name is empty.
func RegisterVirtualObject(def VirtualObjectDef) error {
	virtualObjectRegistry.mu.Lock()
	defer virtualObjectRegistry.mu.Unlock()
	if def.Name == "" {
		return fmt.Errorf("durable: virtual object name must not be empty")
	}
	if _, exists := virtualObjectRegistry.defs[def.Name]; exists {
		return fmt.Errorf("durable: virtual object %q already registered", def.Name)
	}
	virtualObjectRegistry.defs[def.Name] = def
	return nil
}

// GetVirtualObject returns a registered virtual object definition by name.
// The second return value is false if no definition with that name exists.
func GetVirtualObject(name string) (VirtualObjectDef, bool) {
	virtualObjectRegistry.mu.RLock()
	defer virtualObjectRegistry.mu.RUnlock()
	def, ok := virtualObjectRegistry.defs[name]
	return def, ok
}

// TerminalError is a sentinel error that marks a workflow error as
// non-retryable. When a Saga encounters a TerminalError, it compensates
// all completed steps. When the runtime encounters a TerminalError from
// a step or activity, it does not retry.
type TerminalError struct {
	Err error
}

func (e *TerminalError) Error() string {
	return "terminal: " + e.Err.Error()
}

func (e *TerminalError) Unwrap() error {
	return e.Err
}

// NewTerminalError wraps an error as a terminal (non-retryable) error.
func NewTerminalError(err error) error {
	return &TerminalError{Err: err}
}

// IsTerminalError returns true if err is or wraps a TerminalError.
func IsTerminalError(err error) bool {
	var te *TerminalError
	return errors.As(err, &te)
}

// DurableCallError is returned by DurableCall when the service call fails
// in a non-retryable way (e.g., service not found, invalid request).
type DurableCallError struct {
	Service   string
	Operation string
	Message   string
	Err       error
}

func (e *DurableCallError) Error() string {
	return fmt.Sprintf("durable call %s.%s: %s", e.Service, e.Operation, e.Message)
}

func (e *DurableCallError) Unwrap() error {
	return e.Err
}

// ServiceNotFoundError is returned when a service is not found or unavailable.
type ServiceNotFoundError struct {
	Service string
	Err     error
}

func (e *ServiceNotFoundError) Error() string {
	return fmt.Sprintf("service not found: %s", e.Service)
}

func (e *ServiceNotFoundError) Unwrap() error {
	return e.Err
}

// CallTimeoutError is returned when a call exceeds its timeout.
type CallTimeoutError struct {
	Service   string
	Operation string
	Timeout   time.Duration
}

func (e *CallTimeoutError) Error() string {
	return fmt.Sprintf("call %s.%s timed out after %v", e.Service, e.Operation, e.Timeout)
}

// ---- Plugin and versioning types ----

// PluginDependency declares a dependency on a named plugin with a version
// constraint. Workflows set these at compile time; the host resolves them
// at instance creation time.
type PluginDependency struct {
	Name       string `json:"name"`
	Constraint string `json:"constraint"` // e.g., ">=1.2.0", "~1.2.0", "^1.2.0", "=1.2.0"
}

// WorkflowPluginDeps is set at build time (e.g., via ldflags or init())
// to declare which plugin versions this workflow requires. The host reads
// these from the workflow_defs.plugin_deps column at instance creation.
//
// Example (set in an init function):
//
//	func init() {
//	    cleat.WorkflowPluginDeps = []cleat.PluginDependency{
//	        {Name: "llm", Constraint: ">=1.2.0"},
//	        {Name: "blobstore", Constraint: "~2.0.0"},
//	    }
//	}
var WorkflowPluginDeps []PluginDependency

// ParentClosePolicy determines what happens to child workflows when the parent completes or fails.
type ParentClosePolicy string

const (
	ParentClosePolicyAbandon       ParentClosePolicy = "ABANDON"        // Children continue running (current default)
	ParentClosePolicyTerminate     ParentClosePolicy = "TERMINATE"      // Children are terminated
	ParentClosePolicyRequestCancel ParentClosePolicy = "REQUEST_CANCEL" // Cancellation is requested on children
)

// ChildWorkflowOptions carries version resolution, parent close policy, and
// priority configuration for spawning a child workflow.
//
// Version resolution priority:
//  1. Version > 0: use that explicit version
//  2. Version <= 0 (default): child uses the same version as the parent workflow
//
// Priority (0 = highest, lower numbers are picked first):
//  Children do NOT inherit the parent's priority. An explicit priority must
//  be set via the Priority field.

// ---- Call options ----

// CallOptions provides per-call configuration.
//
// Timeout and StartToCloseTimeout are respected when the host-side
// durableCallWithOptions import is populated (the normal WASM runtime path).
// When falling back to the SDK-level retry loop these fields are advisory
// since the underlying DurableCall import has no timeout parameter.
type CallOptions struct {
	Retry           *RetryPolicy
	MaxResponseSize int           // 0 = use default (64KB), capped at outBufSize
	Timeout         time.Duration // 0 = no timeout, per-call deadline
	// Overall deadline for the call including all retries.
	// Unlike Timeout (per-attempt), this caps the total wall-clock time.
	// Temporal-compatible.
	StartToCloseTimeout time.Duration
}

// RetryPolicy configures automatic retry behavior for durable calls.
// When nil, no retry is performed (backward-compatible default).
//
// MaxAttempts is the canonical field. The method MaximumAttempts()
// provides a convenience getter for the same value (also checks a
// zero MaxAttempts for Temporal-compatible aliasing).
type RetryPolicy struct {
	MaxAttempts        int
	InitialInterval    time.Duration
	BackoffCoefficient float64
	MaxInterval        time.Duration
	NonRetryableErrors []string // error substrings that skip retry
}

// DefaultRetryPolicy returns a sensible default retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 2.0,
		MaxInterval:        30 * time.Second,
	}
}

// MaximumAttempts returns the maximum number of retry attempts.
// Returns the value of MaxAttempts, or 0 if rp is nil.
// This is a convenience getter that mirrors the Temporal API naming.
func (rp *RetryPolicy) MaximumAttempts() int {
	if rp == nil {
		return 0
	}
	return rp.MaxAttempts
}

// MaximumInterval returns the maximum backoff interval.
// Returns 0 if rp is nil.
func (rp *RetryPolicy) MaximumInterval() time.Duration {
	if rp == nil {
		return 0
	}
	return rp.MaxInterval
}

// ---- Concrete implementation ----

// HostCallsImpl holds the function pointer fields that back HostCalls.
// The WASM host adapter populates the function fields with functions that
// call into the WASM host imports.
type HostCallsImpl struct {
	durableCall                   func(service, operation, requestJSON string) (string, error)
	durableCallTyped              func(service, operation string, request, result interface{}) error
	durableCallTypedWithOptions   func(opts CallOptions, service, operation string, request, result interface{}) error
	durableCallWithOptions        func(opts CallOptions, service, operation, requestJSON string) (string, error)
	durableCallJSONWithOptions    func(opts CallOptions, service, operation, requestJSON string, result interface{}) error
	durableCallWithHeartbeat      func(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(string)) (string, error)
	durableSleep                  func(ms int64)
	durableAwaitSignals           func(signalNames []string, timeoutMs int64) (string, string, bool, error)
	createPromise                 func(name string) (promiseID string, err error)
	awaitPromise                  func(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)
	resolvePromise                func(id, value string) error
	rejectPromise                 func(id, errMsg string) error
	durableDefer                  func(description string) (string, error)
	durableDeferFunc              func(fn func()) (string, error)
	workflowID                    func() string
	workflowRunID                 func() string
	durableLog                    func(message string)
	pollCancellation              func() (bool, string)
	pollSignal                    func(signalName string) (string, bool, error)
	continueAsNew                 func(newInputJSON string) error
	continueAsNewWithVersion      func(newInputJSON string, newVersion int64) error
	childWorkflow                 func(name, inputJSON string) (string, error)
	childWorkflowWithOptions      func(name, inputJSON string, version int, parentClosePolicy string, priority int) (string, error)
	awaitChild                    func(runID string) (string, error)
	awaitAllChildren              func(runIDs []string) ([]ChildResult, error)
	awaitAnyChild                 func(runIDs []string) (completedRunID string, result string, err error)
	pollChild                     func(runID string) (status string, result string, err error)
	durableCallTypedWithHeartbeat func(service, operation string, request, result interface{}, heartbeatInterval time.Duration, onProgress func(string)) error
	childWorkflowTyped            func(name string, request interface{}) (string, error)
	awaitChildTyped               func(runID string, result interface{}) error
	durableCallWithRetry          func(service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string) (string, error)
	version                       func() int
	minVersion                    func() int
	setQueryState                 func(key, value string)
	registerUpdateHandler         func(name string)
	handleUpdate                  func(name, payload string) (string, error)
	runDetached                   func(fn func(h HostCalls) error) error
	now                           func() int64
	random                        func() int64
	newUUID                       func() string

	pluginCall             func(pluginName, functionName, inputJSON string) (string, error)
	pluginCallStreaming    func(pluginName, functionName, inputJSON string) (<-chan StreamEvent, error)
	durableSend            func(service, operation, requestJSON string) error
	scheduleInvoke         func(service, operation, requestJSON string, delayMs int64) error
	sendSignalAndWait      func(targetRunID, signalName, payload string, timeout time.Duration) (string, error)
	replyToSignal          func(correlationID, response string) error
	awaitSignalsWithQuorum func(signalNames []string, minCount int, maxRejections int, timeout time.Duration) ([]SignalResult, error)
	signalWorkflow         func(targetRunID, signalName, payload string) error
	scheduleCron           func(workflowName, cronExpr, timezone, inputJSON string) (string, error)
	deleteCron             func(scheduleID string) error
	listCrons              func() (string, error)

	acquireLock    func(key string, ttlMs int64) (bool, error)
	releaseLock    func(key string) error
	awaitCondition func(predicate func() bool, pollInterval, timeout time.Duration) (bool, error)

	sideEffect func(computedResult string) (string, error)

	// State map for typed K/V operations.
	stateMap       map[string]interface{}
	updateHandlers map[string]updateHandlerEntry

	// Scope management for virtual object instances.
	scopePrefix  string // "vo:<type>:<key>:" prefix, empty if no scope
	scopeObjType string // current object type in scope
	scopeInstKey string // current instance key in scope
	scopeSet     bool   // true when scope is active
}

// NewHostCalls creates a HostCalls from a set of function implementations.
// Used by the WASM host adapter and by tests.
func NewHostCalls(opts HostCallsOptions) HostCalls {
	return HostCalls{&HostCallsImpl{
		durableCall:                   opts.DurableCall,
		durableCallTyped:              opts.DurableCallTyped,
		durableCallTypedWithOptions:   opts.DurableCallTypedWithOptions,
		durableCallWithOptions:        opts.DurableCallWithOptions,
		durableCallJSONWithOptions:    opts.DurableCallJSONWithOptions,
		durableCallWithHeartbeat:      opts.DurableCallWithHeartbeat,
		durableSleep:                  opts.DurableSleep,
		durableAwaitSignals:           opts.DurableAwaitSignals,
		createPromise:                 opts.CreatePromise,
		awaitPromise:                  opts.AwaitPromise,
		resolvePromise:                opts.ResolvePromise,
		rejectPromise:                 opts.RejectPromise,
		durableDefer:                  opts.DurableDefer,
		durableDeferFunc:              opts.DurableDeferFunc,
		workflowID:                    opts.WorkflowID,
		workflowRunID:                 opts.RunID,
		durableLog:                    opts.DurableLog,
		pollCancellation:              opts.PollCancellation,
		pollSignal:                    opts.PollSignal,
		continueAsNew:                 opts.ContinueAsNew,
		continueAsNewWithVersion:      opts.ContinueAsNewWithVersion,
		childWorkflow:                 opts.ChildWorkflow,
		childWorkflowWithOptions:      opts.ChildWorkflowWithOptions,
		awaitChild:                    opts.AwaitChild,
		awaitAllChildren:              opts.AwaitAllChildren,
		awaitAnyChild:                 opts.AwaitAnyChild,
		pollChild:                     opts.PollChild,
		durableCallTypedWithHeartbeat: opts.DurableCallTypedWithHeartbeat,
		childWorkflowTyped:            opts.ChildWorkflowTyped,
		awaitChildTyped:               opts.AwaitChildTyped,
		durableCallWithRetry:          opts.DurableCallWithRetry,
		version:                       opts.Version,
		minVersion:                    opts.MinVersion,
		setQueryState:                 opts.SetQueryState,
		registerUpdateHandler:         opts.RegisterUpdateHandler,
		handleUpdate:                  opts.HandleUpdate,
		runDetached:                   opts.RunDetached,
		now:                           opts.Now,
		random:                        opts.Random,
		newUUID:                       opts.NewUUID,
		pluginCall:                    opts.PluginCall,
		pluginCallStreaming:           opts.PluginCallStreaming,
		durableSend:                   opts.DurableSend,
		scheduleInvoke:                opts.ScheduleInvoke,
		sendSignalAndWait:             opts.SendSignalAndWait,
		replyToSignal:                 opts.ReplyToSignal,
		awaitSignalsWithQuorum:        opts.AwaitSignalsWithQuorum,
		signalWorkflow:                opts.SignalWorkflow,
		scheduleCron:                  opts.ScheduleCron,
		deleteCron:                    opts.DeleteCron,
		listCrons:                     opts.ListCrons,
		acquireLock:                   opts.AcquireLock,
		releaseLock:                   opts.ReleaseLock,
		awaitCondition:                opts.AwaitCondition,
		sideEffect:                    opts.SideEffect,
	}}
}

// HostCallsOptions holds the function implementations for NewHostCalls.
// All fields are optional. Nil fields produce one of four behaviors depending
// on the method's role:
//
//   - Panic: core primitives (DurableSleep, Now, Random) — nil indicates
//     programmer error.
//   - Error: callable operations (DurableCall, DurableAwaitSignals, etc.) —
//     nil prevents the caller from proceeding safely.
//   - No-op: diagnostic/logging (DurableLog, SetQueryState, PollCancellation).
//   - Default: Version/MinVersion return 1 when nil.
//
// Composite wrappers (not individually nullable — behavior derives from
// the underlying field noted below):
//
//	DurableCallJSON              -> DurableCall
//	DurableCallTyped             -> durableCallTyped (falls back to DurableCall)
//	DurableCallWithOptions       -> durableCallWithOptions (falls back to DurableCall)
//	DurableCallJSONWithOptions   -> DurableCallWithOptions
//	DurableCallWithHeartbeat     -> durableCallWithHeartbeat (falls back to DurableCall)
//	AwaitSignals                 -> DurableAwaitSignals
//	DurableSleep                 -> DurableSleepMs
//	LogKV                        -> DurableLog
//	Now                          -> NowMs
//
// See individual method docs on hostCallsImpl for details.
type HostCallsOptions struct {
	DurableCall                   func(service, operation, requestJSON string) (string, error)
	DurableCallTyped              func(service, operation string, request, result interface{}) error
	DurableCallTypedWithOptions   func(opts CallOptions, service, operation string, request, result interface{}) error
	DurableCallWithOptions        func(opts CallOptions, service, operation, requestJSON string) (string, error)
	DurableCallJSONWithOptions    func(opts CallOptions, service, operation, requestJSON string, result interface{}) error
	DurableCallWithHeartbeat      func(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(string)) (string, error)
	DurableSleep                  func(ms int64)
	DurableSleepMs                func(ms int64)
	DurableAwaitSignals           func(signalNames []string, timeoutMs int64) (string, string, bool, error)
	CreatePromise                 func(name string) (promiseID string, err error)
	AwaitPromise                  func(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)
	ResolvePromise                func(id, value string) error
	RejectPromise                 func(id, errMsg string) error
	DurableDefer                  func(description string) (string, error)
	DurableDeferFunc              func(fn func()) (string, error)
	WorkflowID                    func() string
	RunID                         func() string
	DurableLog                    func(message string)
	PollCancellation              func() (bool, string)
	PollSignal                    func(signalName string) (string, bool, error)
	ContinueAsNew                 func(newInputJSON string) error
	ContinueAsNewWithVersion      func(newInputJSON string, newVersion int64) error
	ChildWorkflow                 func(name, inputJSON string) (string, error)
	ChildWorkflowWithOptions      func(name, inputJSON string, version int, parentClosePolicy string, priority int) (string, error)
	AwaitChild                    func(runID string) (string, error)
	AwaitAllChildren              func(runIDs []string) ([]ChildResult, error)
	AwaitAnyChild                 func(runIDs []string) (completedRunID string, result string, err error)
	PollChild                     func(runID string) (status string, result string, err error)
	DurableCallWithRetry          func(service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string) (string, error)
	DurableCallTypedWithHeartbeat func(service, operation string, request, result interface{}, heartbeatInterval time.Duration, onProgress func(string)) error
	ChildWorkflowTyped            func(name string, request interface{}) (string, error)
	AwaitChildTyped               func(runID string, result interface{}) error
	Version                       func() int
	MinVersion                    func() int
	SetQueryState                 func(key, value string)
	RegisterUpdateHandler         func(name string)
	HandleUpdate                  func(name, payload string) (string, error)
	RunDetached                   func(fn func(h HostCalls) error) error
	Now                           func() int64
	Random                        func() int64
	NewUUID                       func() string
	PluginCall                    func(pluginName, functionName, inputJSON string) (string, error)
	PluginCallStreaming           func(pluginName, functionName, inputJSON string) (<-chan StreamEvent, error)
	DurableSend                   func(service, operation, requestJSON string) error
	ScheduleInvoke                func(service, operation, requestJSON string, delayMs int64) error
	SendSignalAndWait             func(targetRunID, signalName, payload string, timeout time.Duration) (string, error)
	ReplyToSignal                 func(correlationID, response string) error
	AwaitSignalsWithQuorum        func(signalNames []string, minCount int, maxRejections int, timeout time.Duration) ([]SignalResult, error)
	SignalWorkflow                func(targetRunID, signalName, payload string) error
	ScheduleCron                  func(workflowName, cronExpr, timezone, inputJSON string) (string, error)
	DeleteCron                    func(scheduleID string) error
	ListCrons                     func() (string, error)

	AcquireLock func(key string, ttlMs int64) (acquired bool, err error)
	ReleaseLock func(key string) error

	// AwaitCondition blocks until the predicate returns true or the timeout expires.
	AwaitCondition func(predicate func() bool, pollInterval, timeout time.Duration) (bool, error)

	// SideEffect records the result of a non-deterministic function in event
	// history. On first execution, computedResult comes from calling fn; on
	// replay the host returns the cached result.
	SideEffect func(computedResult string) (string, error)
}

// ---- Interface method implementations ----
//
// Nil-guard contract for hostCallsImpl methods:
//   Panic:   DurableSleepMs, NowMs, Random — core primitives, nil = programmer error
//   Error:   DurableCall, DurableCallJSON, DurableCallTyped, DurableCallWithOptions,
//            DurableCallJSONWithOptions, DurableCallWithHeartbeat, DurableAwaitSignals,
//            DurableDefer, PollSignal, ContinueAsNew, ChildWorkflow, AwaitChild, AwaitAllChildren
//   No-op:   DurableLog, LogKV, PollCancellation, SetQueryState — diagnostic/optional
//   Default: Version, MinVersion — return 1 when nil
// ----

func (h *HostCallsImpl) Call(service, operation, requestJSON string) (string, error) {
	return h.DurableCall(service, operation, requestJSON)
}

func (h *HostCallsImpl) DurableCall(service, operation, requestJSON string) (string, error) {
	if h.durableCall == nil {
		return "", errors.New("durable: DurableCall can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.durableCall(service, operation, requestJSON)
}

func (h *HostCallsImpl) DurableCallJSON(service, operation, requestJSON string, result interface{}) error {
	resp, err := h.DurableCall(service, operation, requestJSON)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(resp), result); err != nil {
		return fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
	}
	return nil
}

func (h *HostCallsImpl) DurableCallTyped(service, operation string, request, result interface{}) error {
	if h.durableCallTyped != nil {
		return h.durableCallTyped(service, operation, request, result)
	}

	reqBytes, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("durable: marshaling request for %s.%s: %w", service, operation, err)
	}

	resp, err := h.DurableCall(service, operation, string(reqBytes))
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(resp), result); err != nil {
		return fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
	}
	return nil
}

// DurableCallWithRetry runs a retry policy on the HOST, in one segment.
//
// This is the explicit form of what DurableCallWithOptions picks automatically
// for a short policy. It exists for two reasons: it is the symbol
// wasm/usage.go keys the cleat_call_retry import on, and it is the form the
// Rust SDK has always had (HostCalls::cleat_call_with_retry), so the two SDKs
// now describe the same capability with the same shape.
//
// This SDK applies no threshold of its own: a caller naming this function has
// asked for the host loop specifically. But the HOST still applies the tenant's
// budget, and it can refuse.
//
// That is a deliberate narrowing, and it is what moving the threshold host-side
// means (§3.94 step 4). This function used to be an escape hatch -- a long
// policy would hold a worker and could blow through --wasm-wall-clock-ceiling,
// "which is the caller's choice to make". It is not the caller's choice on a
// shared deployment: the budget bounds how long one tenant may hold a slot, so
// a guest that could opt out of it would make the limit advisory.
//
// A refused policy comes back as a *CallError with Code
// CallErrorRetryPolicyTooLong, and unlike DurableCallWithOptions this function
// does NOT fall back for you -- it has no SDK-level loop to fall back to. The
// call was not made and no attempt was consumed, so a caller that wants the
// suspending behaviour should handle that code by calling
// DurableCallWithOptions, which does it automatically.
//
// Returns an error rather than falling back when the import is unavailable,
// because silently doing something with different durability semantics is how
// this whole area went wrong -- see IMPROVEMENT-PLAN 3.88.
func (h *HostCallsImpl) DurableCallWithRetry(service, operation, requestJSON string, policy RetryPolicy) (string, error) {
	if h.durableCallWithRetry == nil {
		return "", fmt.Errorf("durable: DurableCallWithRetry: the cleat_call_retry import is not wired into this module")
	}
	nonRetryableJSON, _ := json.Marshal(policy.NonRetryableErrors)
	if nonRetryableJSON == nil {
		nonRetryableJSON = []byte("[]")
	}
	return h.durableCallWithRetry(
		service, operation, requestJSON,
		int64(policy.MaxAttempts),
		policy.InitialInterval.Milliseconds(),
		int64(policy.BackoffCoefficient*100),
		policy.MaxInterval.Milliseconds(),
		string(nonRetryableJSON),
	)
}

// DurableCallWithOptions provides retry at either host or SDK level.
// When the host-side durableCallWithRetry import is available, the retry
// loop runs on the host and produces ONE history event per logical call.
// Otherwise, it falls back to SDK-level retry (one event per attempt).
func (h *HostCallsImpl) DurableCallWithOptions(opts CallOptions, service, operation, requestJSON string) (string, error) {
	if h.durableCallWithOptions != nil {
		return h.durableCallWithOptions(opts, service, operation, requestJSON)
	}

	// Per-call timeout enforcement.
	// When opts.Timeout > 0, the call is wrapped in a goroutine and must
	// complete within the deadline or a CallTimeoutError is returned.
	if opts.Timeout > 0 {
		type callResult struct {
			resp string
			err  error
		}
		ch := make(chan callResult, 1)
		go func() {
			resp, err := h.DurableCall(service, operation, requestJSON)
			ch <- callResult{resp, err}
		}()
		select {
		case r := <-ch:
			return r.resp, r.err
		case <-time.After(opts.Timeout):
			return "", &CallTimeoutError{
				Service:   service,
				Operation: operation,
				Timeout:   opts.Timeout,
			}
		}
	}

	// StartToCloseTimeout: overall deadline across all retry attempts.
	// Unlike Timeout (per-attempt), this caps the total wall-clock time.
	var overallDeadline time.Time
	if opts.StartToCloseTimeout > 0 {
		overallDeadline = time.Now().Add(opts.StartToCloseTimeout)

		if opts.Retry == nil {
			// No retry: use StartToCloseTimeout as the per-call timeout.
			type callResult struct {
				resp string
				err  error
			}
			ch := make(chan callResult, 1)
			go func() {
				resp, err := h.DurableCall(service, operation, requestJSON)
				ch <- callResult{resp, err}
			}()
			select {
			case r := <-ch:
				return r.resp, r.err
			case <-time.After(opts.StartToCloseTimeout):
				return "", &CallTimeoutError{
					Service:   service,
					Operation: operation,
					Timeout:   opts.StartToCloseTimeout,
				}
			}
		}
	}

	if opts.Retry == nil {
		return h.DurableCall(service, operation, requestJSON)
	}

	// When host-side retry is available AND the policy is short enough to be
	// worth holding a worker for, delegate to the host import: ONE history
	// event for the whole logical call, one segment, no replay per attempt.
	//
	// The threshold is the whole point, not a safety valve. IMPROVEMENT-PLAN
	// 3.88: a retry loop finishing within a few minutes should keep its worker,
	// the way non-durable code would, because that is frequent and ordinary. A
	// policy that backs off for an hour should NOT -- it should suspend and let
	// the worker do something else, which is what the SDK-level loop below does
	// via DurableSleep.
	//
	// Getting this wrong in the generous direction is worse than not wiring the
	// import at all: the host loop backs off inside a host call, so a policy
	// exceeding the wall-clock ceiling (--wasm-wall-clock-ceiling, 5m by
	// default, see 3.90) does not merely waste a worker -- it gets the
	// invocation killed, where the SDK-level path would have suspended and
	// completed.
	// The HOST decides whether this policy fits in one segment. It knows the
	// tenant's budget; this SDK does not, and a constant compiled in here could
	// only ever be one operator's answer for every tenant (§3.94 step 4).
	//
	// A refusal costs nothing: the host makes no call, records no event, and
	// consumes no attempt, so falling through to the loop below starts the
	// policy from attempt 1 with an untouched history.
	// opts.Retry != nil is load-bearing, not defensive: retryFitsInOneSegment,
	// which this condition replaces, returned false for a nil policy and so
	// sent it down the loop below. Dropping the check here would dereference
	// nil instead.
	if h.durableCallWithRetry != nil && opts.Retry != nil {
		rp := opts.Retry
		nonRetryableJSON, _ := json.Marshal(rp.NonRetryableErrors)
		if nonRetryableJSON == nil {
			nonRetryableJSON = []byte("[]")
		}
		resp, err := h.durableCallWithRetry(
			service, operation, requestJSON,
			int64(rp.MaxAttempts),
			rp.InitialInterval.Milliseconds(),
			int64(rp.BackoffCoefficient*100),
			rp.MaxInterval.Milliseconds(),
			string(nonRetryableJSON),
		)
		var ce *CallError
		if !errors.As(err, &ce) || ce.Code != CallErrorRetryPolicyTooLong {
			return resp, err
		}
		// Refused as too long. Fall through to the SDK-level loop, which
		// suspends between attempts via DurableSleep.
	}

	// Fall back to SDK-level retry (one event per attempt).
	rp := opts.Retry
	var lastErr error
	for attempt := 1; attempt <= rp.MaxAttempts; attempt++ {
		if !overallDeadline.IsZero() && time.Now().After(overallDeadline) {
			return "", &CallTimeoutError{
				Service:   service,
				Operation: operation,
				Timeout:   opts.StartToCloseTimeout,
			}
		}
		resp, err := h.DurableCall(service, operation, requestJSON)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if !overallDeadline.IsZero() && time.Now().After(overallDeadline) {
			return "", &CallTimeoutError{
				Service:   service,
				Operation: operation,
				Timeout:   opts.StartToCloseTimeout,
			}
		}

		if isNonRetryable(err, rp.NonRetryableErrors) {
			return "", err
		}

		if attempt < rp.MaxAttempts {
			backoff := time.Duration(float64(rp.InitialInterval) * math.Pow(rp.BackoffCoefficient, float64(attempt-1)))
			if backoff > rp.MaxInterval {
				backoff = rp.MaxInterval
			}
			if !overallDeadline.IsZero() {
				remaining := time.Until(overallDeadline)
				if backoff > remaining {
					return "", &CallTimeoutError{
						Service:   service,
						Operation: operation,
						Timeout:   opts.StartToCloseTimeout,
					}
				}
			}
			h.DurableSleep(backoff)
		}
	}
	return "", fmt.Errorf("durable: call %s.%s retry exhausted after %d attempts: %w",
		service, operation, rp.MaxAttempts, lastErr)
}

func (h *HostCallsImpl) DurableCallTypedWithOptions(opts CallOptions, service, operation string, request, result interface{}) error {
	if h.durableCallTypedWithOptions != nil {
		return h.durableCallTypedWithOptions(opts, service, operation, request, result)
	}

	// Per-call timeout enforcement for the typed variant.
	if opts.Timeout > 0 {
		type callResult struct {
			resp string
			err  error
		}
		ch := make(chan callResult, 1)
		go func() {
			reqBytes, marshalErr := json.Marshal(request)
			if marshalErr != nil {
				ch <- callResult{"", marshalErr}
				return
			}
			resp, callErr := h.DurableCallWithOptions(opts, service, operation, string(reqBytes))
			ch <- callResult{resp, callErr}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				return r.err
			}
			if result != nil {
				if err := json.Unmarshal([]byte(r.resp), result); err != nil {
					return fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
				}
			}
			return nil
		case <-time.After(opts.Timeout):
			return &CallTimeoutError{
				Service:   service,
				Operation: operation,
				Timeout:   opts.Timeout,
			}
		}
	}

	// StartToCloseTimeout: overall deadline across all retry attempts.
	// When there is a retry policy, DurableCallWithOptions handles the deadline.
	if opts.StartToCloseTimeout > 0 && opts.Retry == nil {
		// No retry: use StartToCloseTimeout as the per-call timeout.
		type callResult struct {
			resp string
			err  error
		}
		ch := make(chan callResult, 1)
		go func() {
			reqBytes, marshalErr := json.Marshal(request)
			if marshalErr != nil {
				ch <- callResult{"", marshalErr}
				return
			}
			resp, callErr := h.DurableCall(service, operation, string(reqBytes))
			ch <- callResult{resp, callErr}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				return r.err
			}
			if result != nil {
				if err := json.Unmarshal([]byte(r.resp), result); err != nil {
					return fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
				}
			}
			return nil
		case <-time.After(opts.StartToCloseTimeout):
			return &CallTimeoutError{
				Service:   service,
				Operation: operation,
				Timeout:   opts.StartToCloseTimeout,
			}
		}
	}

	reqBytes, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("durable: marshaling request for %s.%s: %w", service, operation, err)
	}

	resp, err := h.DurableCallWithOptions(opts, service, operation, string(reqBytes))
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(resp), result); err != nil {
		return fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
	}
	return nil
}

func (h *HostCallsImpl) DurableCallJSONWithOptions(opts CallOptions, service, operation, requestJSON string, result interface{}) error {
	resp, err := h.DurableCallWithOptions(opts, service, operation, requestJSON)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(resp), result); err != nil {
		return fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
	}
	return nil
}

func (h *HostCallsImpl) DurableCallWithHeartbeat(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(string)) (string, error) {
	if h.durableCallWithHeartbeat != nil {
		return h.durableCallWithHeartbeat(service, operation, requestJSON, heartbeatInterval, onProgress)
	}
	// Fallback: regular durable call without heartbeat support.
	return h.DurableCall(service, operation, requestJSON)
}

func (h *HostCallsImpl) DurableCallTypedWithHeartbeat(service, operation string, request, result interface{}, heartbeatInterval time.Duration, onProgress func(string)) error {
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("durable: marshaling request for %s.%s: %w", service, operation, err)
	}
	resp, err := h.DurableCallWithHeartbeat(service, operation, string(reqJSON), heartbeatInterval, onProgress)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(resp), result); err != nil {
		return fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
	}
	return nil
}

func (h *HostCallsImpl) PluginCall(pluginName, functionName, inputJSON string) (string, error) {
	if h.pluginCall != nil {
		return h.pluginCall(pluginName, functionName, inputJSON)
	}
	return "", fmt.Errorf("durable: PluginCall can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
}

func (h *HostCallsImpl) PluginCallStreaming(pluginName, functionName, inputJSON string) (<-chan StreamEvent, error) {
	if h.pluginCallStreaming != nil {
		return h.pluginCallStreaming(pluginName, functionName, inputJSON)
	}
	return nil, fmt.Errorf("durable: PluginCallStreaming can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
}

func (h *HostCallsImpl) DurableSend(service, operation, requestJSON string) error {
	if h.durableSend == nil {
		return errors.New("durable: DurableSend can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.durableSend(service, operation, requestJSON)
}

func (h *HostCallsImpl) ScheduleInvoke(service, operation, requestJSON string, delayMs int64) error {
	if h.scheduleInvoke == nil {
		return errors.New("durable: ScheduleInvoke can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.scheduleInvoke(service, operation, requestJSON, delayMs)
}

func (h *HostCallsImpl) ScheduleCron(workflowName, cronExpr, timezone, inputJSON string) (string, error) {
	if h.scheduleCron == nil {
		return "", errors.New("durable: ScheduleCron can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.scheduleCron(workflowName, cronExpr, timezone, inputJSON)
}

func (h *HostCallsImpl) DeleteCron(scheduleID string) error {
	if h.deleteCron == nil {
		return errors.New("durable: DeleteCron can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.deleteCron(scheduleID)
}

func (h *HostCallsImpl) ListCrons() (string, error) {
	if h.listCrons == nil {
		return "", errors.New("durable: ListCrons can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.listCrons()
}

func (h *HostCallsImpl) AwaitCondition(predicate func() bool, pollInterval, timeout time.Duration) (met bool) {
	if h.awaitCondition != nil {
		met, err := h.awaitCondition(predicate, pollInterval, timeout)
		if err != nil {
			return false
		}
		return met
	}
	deadline := h.Now().Add(timeout)
	for {
		if predicate() {
			return true
		}
		if h.Now().After(deadline) {
			return false
		}
		h.AwaitSignals([]string{"__condition_poll"}, pollInterval)
	}
}

func (h *HostCallsImpl) SideEffect(fn func() (string, error)) (string, error) {
	if h.sideEffect == nil {
		return "", errors.New("durable: SideEffect can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	computedResult, err := fn()
	if err != nil {
		return "", err
	}
	return h.sideEffect(computedResult)
}

func (h *HostCallsImpl) AcquireLock(key string, ttl time.Duration) (bool, error) {
	return h.AcquireLockMs(key, ttl.Milliseconds())
}

func (h *HostCallsImpl) AcquireLockMs(key string, ttlMs int64) (bool, error) {
	if h.acquireLock == nil {
		return false, errors.New("durable: AcquireLock can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.acquireLock(key, ttlMs)
}

func (h *HostCallsImpl) ReleaseLock(key string) error {
	if h.releaseLock == nil {
		return errors.New("durable: ReleaseLock can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.releaseLock(key)
}

func (h *HostCallsImpl) DurableDefer(description string) (string, error) {
	if h.durableDefer == nil {
		return "", errors.New("durable: DurableDefer can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.durableDefer(description)
}

func (h *HostCallsImpl) DurableDeferFunc(fn func()) (string, error) {
	if h.durableDeferFunc == nil {
		return "", errors.New("durable: DurableDeferFunc can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.durableDeferFunc(fn)
}
