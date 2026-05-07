// Package durable defines the shared types and host interface between
// the WASM workflow module and the host runtime.
//
// This is the "durable SDK" — the only import the workflow author needs.
// Everything else comes from ordinary Go and the code transformer.
package durable

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// SuspendSentinel is a sentinel panic value used to suspend workflow execution.
// When the host returns a suspend signal (e.g., from DurableSleep), the
// WASM adapter panics with this value. The export wrapper catches it and
// returns a suspend status code to the host.
type SuspendSentinel struct{}

func (SuspendSentinel) Error() string { return "durable: workflow suspended" }

// ErrSuspend is the sentinel value panicked to suspend a workflow.
var ErrSuspend error = SuspendSentinel{}

// HostCalls is the interface workflow code programs against. It provides
// durable, deterministic access to external services, time, and randomness.
//
// Entry points receive a HostCalls as their first parameter. Helper functions
// in the durable closure can thread it through their call chains (manually or
// via the auto-threading transformer).
//
// The concrete implementation is *hostCallsImpl, created by the WASM host
// adapter at runtime. For tests, mock implementations can be provided.
type HostCalls interface {
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
	// invokes onProgress periodically with status updates from the host.
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

	// DurableSleep suspends the workflow for the given duration.
	DurableSleep(d time.Duration)

	// DurableSleepMs suspends the workflow for the given milliseconds.
	// Prefer DurableSleep(time.Duration) for readability.
	DurableSleepMs(ms int64)

	// AwaitSignals blocks until one of the named signals arrives or the
	// timeout expires. Returns a structured SignalResult.
	AwaitSignals(signalNames []string, timeout time.Duration) SignalResult

	// DurableAwaitSignals is the low-level signal wait. Prefer AwaitSignals.
	DurableAwaitSignals(signalNames []string, timeoutMs int64) (signalName, payload string, timedOut bool, err error)

	// CreatePromise creates a named durable promise and returns its ID.
	// The promise can be resolved or rejected by an external caller via the REST API.
	CreatePromise(name string) (promiseID string, err error)

	// AwaitPromise waits for a promise to be resolved by an external caller.
	// Returns the result, whether it timed out, and any error.
	// Blocks the workflow until resolved or timeout expires.
	AwaitPromise(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)

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

	// PollCancellation checks whether the workflow has been requested to
	// cancel. Returns true and the reason if so.
	PollCancellation() (cancelled bool, reason string)

	// PollSignal checks for a non-blocking signal.
	PollSignal(signalName string) (payload string, found bool, err error)

	// ContinueAsNew creates a new workflow run with fresh event history,
	// passing the current state as input.
	ContinueAsNew(newInputJSON string) error

	// ChildWorkflow starts a child workflow with its own event history.
	ChildWorkflow(name string, inputJSON string) (runID string, err error)

	// AwaitChild waits for a child workflow to complete.
	AwaitChild(runID string) (resultJSON string, err error)

	// AwaitAllChildren waits for all child workflows identified by runIDs to
	// complete. Results are returned in the same order as runIDs. Unlike
	// calling AwaitChild in a loop, all children are awaited concurrently.
	AwaitAllChildren(runIDs []string) ([]ChildResult, error)

	// ChildWorkflowTyped starts a child workflow with typed input.
	// Marshals request to JSON internally. Use AwaitChildTyped to
	// get the typed result.
	ChildWorkflowTyped(name string, request interface{}) (runID string, err error)

	// AwaitChildTyped waits for a child workflow and unmarshals its result.
	AwaitChildTyped(runID string, result interface{}) error

	// Version returns the current workflow version number for schema evolution.
	Version() int

	// MinVersion declares the minimum version this code requires. If a
	// workflow replays against newer code than it started with, the runtime
	// can detect version skew.
	MinVersion() int

	// SetQueryState records state that can be read by query handlers.
	SetQueryState(key, value string)

	// SetState stores a typed value in the workflow's state.
	// value is marshaled to JSON for persistence.
	SetState(key string, value interface{})

	// GetState retrieves a typed value from the workflow's state.
	// result must be a pointer; the stored value is unmarshaled into it.
	GetState(key string, result interface{}) error

	// DeleteState removes a key from the workflow's state.
	DeleteState(key string)

	// HasState returns true if the key exists in the workflow's state.
	HasState(key string) bool

	// IncrState atomically increments a numeric state value by delta.
	// Returns the new value after increment.
	IncrState(key string, delta int64) int64

	// ListState returns all state keys matching the given prefix.
	ListState(prefix string) []string

	// RegisterUpdateHandler registers a handler for the named update.
	// handler receives payload JSON and returns result JSON.
	// validator runs first (read-only). Called during workflow init, before durable ops.
	RegisterUpdateHandler(name string, handler func(payloadJSON string) (resultJSON string, err error), validator func(payloadJSON string) error)

	// RegisterQueryHandler registers a read-only query handler that can be
	// invoked on-demand by external callers without journaling.
	// Queries are deterministic, read-only, and do not record events.
	RegisterQueryHandler(name string, handler func(payloadJSON string) (resultJSON string, err error))

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

	// RunDetached runs fn with a fresh HostCalls that ignores cancellation.
	// fn executes immediately, is recorded in history, and survives crash/replay.
	// On replay, fn IS re-executed (not replayed from cache).
	RunDetached(fn func(h HostCalls) error) error

	// DurableFetch makes an HTTP request as a durable operation.
	// Delegates to DurableCall("http", "fetch", requestJSON) internally.
	DurableFetch(url, method string, headers map[string]string, body string) (responseJSON string, statusCode int, err error)

	// DurableFetchJSON is like DurableFetch but unmarshals the response into result.
	DurableFetchJSON(url, method string, headers map[string]string, body string, result interface{}) error

	// FetchGet is a shorthand for DurableFetch with GET method, no headers, no body.
	FetchGet(url string) (responseJSON string, statusCode int, err error)

	// FetchGetJSON is like FetchGet but unmarshals the response into result.
	FetchGetJSON(url string, result interface{}) error

	// Now returns the current wall-clock time. Use instead of time.Now()
	// for deterministic replay.
	Now() time.Time

	// NowMs returns the current wall-clock time in milliseconds since epoch.
	// Prefer Now() for readability.
	NowMs() int64

	// Random returns a deterministic random number seeded from the event
	// history. Use instead of math/rand for deterministic replay.
	Random() int64

	// SetScope sets the state key prefix for virtual object instances.
	// All subsequent SetState/GetState/etc calls are automatically prefixed
	// with "vo:<objectType>:<instanceKey>:". Returns the previous scope
	// prefix for stack-style save/restore.
	SetScope(objectType, instanceKey string) (previousScope string)

	// GetScope returns the current (objectType, instanceKey) or ("", "")
	// if no scope is set.
	GetScope() (objectType, instanceKey string)

	// ClearScope removes the current scope and returns the previous scope
	// prefix (empty string if none was set).
	ClearScope() (previousScope string)

	// UUID returns a deterministic UUID scoped to the current workflow
	// and the given seed. Same seed always produces the same UUID for
	// this workflow instance. Useful for generating predictable IDs
	// (e.g. entity IDs, correlation IDs) that are stable across replays.
	UUID(seed string) string
}

// ---- Result types ----

// SignalResult is the structured result of AwaitSignals.
type SignalResult struct {
	Name     string
	Payload  string
	TimedOut bool
	Err      error
}

// ChildResult holds the outcome of a child workflow.
type ChildResult struct {
	RunID  string `json:"run_id"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

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

// Promise is a typed durable promise that can be awaited within the workflow
// and resolved by an external caller via the REST API. Create with
// CreatePromiseTyped or NewPromiseTyped.
//
// Usage:
//
//	promise, err := durable.NewPromiseTyped[ApprovalResult](h, "manager_approval")
//	// ... pass promise.ID to an external system ...
//	result, timedOut, err := promise.Await(30 * time.Minute)
type Promise[T any] struct {
	ID   string
	Name string
	h    HostCalls
}

// Await blocks until the promise is resolved or the timeout expires.
// Returns the typed result, whether it timed out, and any error.
func (p *Promise[T]) Await(timeout time.Duration) (T, bool, error) {
	var zero T
	resultJSON, timedOut, err := p.h.AwaitPromise(p.ID, timeout)
	if err != nil {
		return zero, timedOut, err
	}
	var val T
	if err := json.Unmarshal([]byte(resultJSON), &val); err != nil {
		return zero, false, fmt.Errorf("durable: unmarshal promise result: %w", err)
	}
	return val, timedOut, nil
}

// NewPromiseTyped creates a typed durable promise and returns a Promise[T]
// that can be awaited later. The name is a human-readable label.
func NewPromiseTyped[T any](h HostCalls, name string) (*Promise[T], error) {
	id, err := h.CreatePromise(name)
	if err != nil {
		return nil, err
	}
	return &Promise[T]{ID: id, Name: name, h: h}, nil
}

// ---- Structured error types ----

// updateHandlerEntry stores a registered update handler and its validator.
type updateHandlerEntry struct {
	handler   func(payloadJSON string) (resultJSON string, err error)
	validator func(payloadJSON string) error
}

// CallErrorCode classifies durable call failures so callers can distinguish
// retryable from non-retryable errors without string-matching.
type CallErrorCode int

const (
	CallErrorUnknown           CallErrorCode = iota
	CallErrorTimeout                        // retryable
	CallErrorUnavailable                    // retryable
	CallErrorNotFound                       // non-retryable
	CallErrorInvalidRequest                 // non-retryable
	CallErrorPermissionDenied               // non-retryable
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
// global registry. Panics if a definition with the same name already
// exists or if the name is empty.
func RegisterVirtualObject(def VirtualObjectDef) {
	virtualObjectRegistry.mu.Lock()
	defer virtualObjectRegistry.mu.Unlock()
	if def.Name == "" {
		panic("durable: virtual object name must not be empty")
	}
	if _, exists := virtualObjectRegistry.defs[def.Name]; exists {
		panic(fmt.Sprintf("durable: virtual object %q already registered", def.Name))
	}
	virtualObjectRegistry.defs[def.Name] = def
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

// ---- Call options ----

// CallOptions provides per-call configuration.

type CallOptions struct {
	Retry           *RetryPolicy
	MaxResponseSize int    // 0 = use default (64KB), capped at outBufSize
	Timeout         time.Duration // 0 = no timeout
}

// RetryPolicy configures automatic retry behavior for durable calls.
// When nil, no retry is performed (backward-compatible default).
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
// Returns 0 if rp is nil.
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

// hostCallsImpl is the default concrete implementation of HostCalls.
// The WASM host adapter populates the function fields with closures that
// call into the WASM host imports.
type hostCallsImpl struct {
	durableCall               func(service, operation, requestJSON string) (string, error)
	durableCallTyped          func(service, operation string, request, result interface{}) error
	durableCallTypedWithOptions func(opts CallOptions, service, operation string, request, result interface{}) error
	durableCallWithOptions    func(opts CallOptions, service, operation, requestJSON string) (string, error)
	durableCallJSONWithOptions func(opts CallOptions, service, operation, requestJSON string, result interface{}) error
	durableCallWithHeartbeat   func(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(string)) (string, error)
	durableSleep              func(ms int64)
	durableAwaitSignals       func(signalNames []string, timeoutMs int64) (string, string, bool, error)
	createPromise    func(name string) (promiseID string, err error)
	awaitPromise     func(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)
	resolvePromise   func(id, value string) error
	rejectPromise    func(id, errMsg string) error
	durableDefer              func(description string) (string, error)
	durableDeferFunc          func(fn func()) (string, error)
	workflowID                func() string
	workflowRunID             func() string
	durableLog                func(message string)
	pollCancellation          func() (bool, string)
	pollSignal                func(signalName string) (string, bool, error)
	continueAsNew             func(newInputJSON string) error
	childWorkflow             func(name, inputJSON string) (string, error)
	awaitChild                func(runID string) (string, error)
	awaitAllChildren           func(runIDs []string) ([]ChildResult, error)
	durableCallTypedWithHeartbeat func(service, operation string, request, result interface{}, heartbeatInterval time.Duration, onProgress func(string)) error
	childWorkflowTyped        func(name string, request interface{}) (string, error)
	awaitChildTyped           func(runID string, result interface{}) error
	durableCallWithRetry       func(service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string) (string, error)
	version                   func() int
	minVersion                func() int
	setQueryState             func(key, value string)
	registerUpdateHandler     func(name string)
	registerQueryHandler     func(name string)
	handleQuery              func(name, payload string) (string, error)
	runDetached               func(fn func(h HostCalls) error) error
	now                       func() int64
	random                    func() int64

	pluginCall                func(pluginName, functionName, inputJSON string) (string, error)
	durableSend               func(service, operation, requestJSON string) error
	scheduleInvoke            func(service, operation, requestJSON string, delayMs int64) error
	sendSignalAndWait         func(targetRunID, signalName, payload string, timeout time.Duration) (string, error)
	replyToSignal             func(correlationID, response string) error
	awaitSignalsWithQuorum    func(signalNames []string, minCount int, maxRejections int, timeout time.Duration) ([]SignalResult, error)
	signalWorkflow            func(targetRunID, signalName, payload string) error

	// State map for typed K/V operations.
	stateMap       map[string]interface{}
	updateHandlers map[string]updateHandlerEntry
	queryHandlers map[string]func(payloadJSON string) (resultJSON string, err error)

	// Scope management for virtual object instances.
	scopePrefix  string // "vo:<type>:<key>:" prefix, empty if no scope
	scopeObjType string // current object type in scope
	scopeInstKey string // current instance key in scope
	scopeSet     bool   // true when scope is active
}

// NewHostCalls creates a HostCalls from a set of function implementations.
// Used by the WASM host adapter and by tests.
func NewHostCalls(opts HostCallsOptions) HostCalls {
	return &hostCallsImpl{
		durableCall:               opts.DurableCall,
		durableCallTyped:          opts.DurableCallTyped,
		durableCallTypedWithOptions: opts.DurableCallTypedWithOptions,
		durableCallWithOptions:    opts.DurableCallWithOptions,
		durableCallJSONWithOptions: opts.DurableCallJSONWithOptions,
		durableCallWithHeartbeat:   opts.DurableCallWithHeartbeat,
		durableSleep:              opts.DurableSleep,
		durableAwaitSignals:       opts.DurableAwaitSignals,
		createPromise:              opts.CreatePromise,
		awaitPromise:               opts.AwaitPromise,
		resolvePromise:             opts.ResolvePromise,
		rejectPromise:              opts.RejectPromise,
		durableDefer:              opts.DurableDefer,
		durableDeferFunc:          opts.DurableDeferFunc,
		workflowID:                opts.WorkflowID,
		workflowRunID:             opts.RunID,
		durableLog:                opts.DurableLog,
		pollCancellation:          opts.PollCancellation,
		pollSignal:                opts.PollSignal,
		continueAsNew:             opts.ContinueAsNew,
		childWorkflow:             opts.ChildWorkflow,
		awaitChild:                opts.AwaitChild,
		awaitAllChildren:           opts.AwaitAllChildren,
		durableCallTypedWithHeartbeat: opts.DurableCallTypedWithHeartbeat,
		childWorkflowTyped:        opts.ChildWorkflowTyped,
		awaitChildTyped:           opts.AwaitChildTyped,
		durableCallWithRetry:       opts.DurableCallWithRetry,
		version:                   opts.Version,
		minVersion:                opts.MinVersion,
		setQueryState:             opts.SetQueryState,
		registerUpdateHandler:     opts.RegisterUpdateHandler,
		registerQueryHandler:     opts.RegisterQueryHandler,
		handleQuery:              opts.HandleQuery,
		runDetached:               opts.RunDetached,
		now:                       opts.Now,
		random:                    opts.Random,
		pluginCall:                opts.PluginCall,
		durableSend:               opts.DurableSend,
		scheduleInvoke:            opts.ScheduleInvoke,
		sendSignalAndWait:         opts.SendSignalAndWait,
		replyToSignal:             opts.ReplyToSignal,
		awaitSignalsWithQuorum:    opts.AwaitSignalsWithQuorum,
		signalWorkflow:            opts.SignalWorkflow,
	}
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
//   DurableCallJSON              -> DurableCall
//   DurableCallTyped             -> durableCallTyped (falls back to DurableCall)
//   DurableCallWithOptions       -> durableCallWithOptions (falls back to DurableCall)
//   DurableCallJSONWithOptions   -> DurableCallWithOptions
//   DurableCallWithHeartbeat     -> durableCallWithHeartbeat (falls back to DurableCall)
//   AwaitSignals                 -> DurableAwaitSignals
//   DurableSleep                 -> DurableSleepMs
//   LogKV                        -> DurableLog
//   Now                          -> NowMs
//
// See individual method docs on hostCallsImpl for details.
type HostCallsOptions struct {
	DurableCall               func(service, operation, requestJSON string) (string, error)
	DurableCallTyped          func(service, operation string, request, result interface{}) error
	DurableCallTypedWithOptions func(opts CallOptions, service, operation string, request, result interface{}) error
	DurableCallWithOptions    func(opts CallOptions, service, operation, requestJSON string) (string, error)
	DurableCallJSONWithOptions func(opts CallOptions, service, operation, requestJSON string, result interface{}) error
	DurableCallWithHeartbeat   func(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(string)) (string, error)
	DurableSleep              func(ms int64)
	DurableAwaitSignals       func(signalNames []string, timeoutMs int64) (string, string, bool, error)
	CreatePromise func(name string) (promiseID string, err error)
	AwaitPromise  func(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)
	ResolvePromise func(id, value string) error
	RejectPromise  func(id, errMsg string) error
	DurableDefer              func(description string) (string, error)
	DurableDeferFunc          func(fn func()) (string, error)
	WorkflowID                func() string
	RunID                     func() string
	DurableLog                func(message string)
	PollCancellation          func() (bool, string)
	PollSignal                func(signalName string) (string, bool, error)
	ContinueAsNew                func(newInputJSON string) error
	ChildWorkflow                func(name, inputJSON string) (string, error)
	AwaitChild                   func(runID string) (string, error)
	AwaitAllChildren              func(runIDs []string) ([]ChildResult, error)
	DurableCallWithRetry          func(service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string) (string, error)
	DurableCallTypedWithHeartbeat func(service, operation string, request, result interface{}, heartbeatInterval time.Duration, onProgress func(string)) error
	ChildWorkflowTyped           func(name string, request interface{}) (string, error)
	AwaitChildTyped              func(runID string, result interface{}) error
	Version                   func() int
	MinVersion                func() int
	SetQueryState             func(key, value string)
	RegisterUpdateHandler     func(name string)
	RegisterQueryHandler     func(name string)
	HandleQuery              func(name, payload string) (string, error)
	RunDetached               func(fn func(h HostCalls) error) error
	Now                       func() int64
	Random                    func() int64
	PluginCall                func(pluginName, functionName, inputJSON string) (string, error)
	DurableSend               func(service, operation, requestJSON string) error
	ScheduleInvoke            func(service, operation, requestJSON string, delayMs int64) error
	SendSignalAndWait         func(targetRunID, signalName, payload string, timeout time.Duration) (string, error)
	ReplyToSignal             func(correlationID, response string) error
	AwaitSignalsWithQuorum    func(signalNames []string, minCount int, maxRejections int, timeout time.Duration) ([]SignalResult, error)
	SignalWorkflow            func(targetRunID, signalName, payload string) error
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

func (h *hostCallsImpl) DurableCall(service, operation, requestJSON string) (string, error) {
	if h.durableCall == nil {
		return "", errors.New("durable: DurableCall not initialized")
	}
	return h.durableCall(service, operation, requestJSON)
}

func (h *hostCallsImpl) DurableCallJSON(service, operation, requestJSON string, result interface{}) error {
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

func (h *hostCallsImpl) DurableCallTyped(service, operation string, request, result interface{}) error {
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

// DurableCallWithOptions provides retry at either host or SDK level.
// When the host-side durableCallWithRetry import is available, the retry
// loop runs on the host and produces ONE history event per logical call.
// Otherwise, it falls back to SDK-level retry (one event per attempt).
func (h *hostCallsImpl) DurableCallWithOptions(opts CallOptions, service, operation, requestJSON string) (string, error) {
	if h.durableCallWithOptions != nil {
		return h.durableCallWithOptions(opts, service, operation, requestJSON)
	}

	if opts.Retry == nil {
		return h.DurableCall(service, operation, requestJSON)
	}

	// When host-side retry is available, delegate to the host import.
	// This produces ONE history event per logical call instead of one per attempt.
	if h.durableCallWithRetry != nil {
		rp := opts.Retry
		nonRetryableJSON, _ := json.Marshal(rp.NonRetryableErrors)
		if nonRetryableJSON == nil {
			nonRetryableJSON = []byte("[]")
		}
		return h.durableCallWithRetry(
			service, operation, requestJSON,
			int64(rp.MaxAttempts),
			rp.InitialInterval.Milliseconds(),
			int64(rp.BackoffCoefficient*100),
			rp.MaxInterval.Milliseconds(),
			string(nonRetryableJSON),
		)
	}

	// Fall back to SDK-level retry (one event per attempt).
	rp := opts.Retry
	var lastErr error
	for attempt := 1; attempt <= rp.MaxAttempts; attempt++ {
		resp, err := h.DurableCall(service, operation, requestJSON)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if isNonRetryable(err, rp.NonRetryableErrors) {
			return "", err
		}

		if attempt < rp.MaxAttempts {
			backoff := time.Duration(float64(rp.InitialInterval) * math.Pow(rp.BackoffCoefficient, float64(attempt-1)))
			if backoff > rp.MaxInterval {
				backoff = rp.MaxInterval
			}
			h.DurableSleep(backoff)
		}
	}
	return "", fmt.Errorf("durable: call %s.%s retry exhausted after %d attempts: %w",
		service, operation, rp.MaxAttempts, lastErr)
}

func (h *hostCallsImpl) DurableCallTypedWithOptions(opts CallOptions, service, operation string, request, result interface{}) error {
	if h.durableCallTypedWithOptions != nil {
		return h.durableCallTypedWithOptions(opts, service, operation, request, result)
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

func (h *hostCallsImpl) DurableCallJSONWithOptions(opts CallOptions, service, operation, requestJSON string, result interface{}) error {
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

func (h *hostCallsImpl) DurableCallWithHeartbeat(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(string)) (string, error) {
	if h.durableCallWithHeartbeat != nil {
		return h.durableCallWithHeartbeat(service, operation, requestJSON, heartbeatInterval, onProgress)
	}
	// Fallback: regular durable call without heartbeat support.
	return h.DurableCall(service, operation, requestJSON)
}

func (h *hostCallsImpl) DurableCallTypedWithHeartbeat(service, operation string, request, result interface{}, heartbeatInterval time.Duration, onProgress func(string)) error {
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

func (h *hostCallsImpl) PluginCall(pluginName, functionName, inputJSON string) (string, error) {
	if h.pluginCall != nil {
		return h.pluginCall(pluginName, functionName, inputJSON)
	}
	return "", fmt.Errorf("durable: PluginCall not initialized")
}

func (h *hostCallsImpl) DurableSend(service, operation, requestJSON string) error {
	if h.durableSend == nil {
		return errors.New("durable: DurableSend not initialized")
	}
	return h.durableSend(service, operation, requestJSON)
}

func (h *hostCallsImpl) ScheduleInvoke(service, operation, requestJSON string, delayMs int64) error {
	if h.scheduleInvoke == nil {
		return errors.New("durable: ScheduleInvoke not initialized")
	}
	return h.scheduleInvoke(service, operation, requestJSON, delayMs)
}

func (h *hostCallsImpl) SendSignalAndWait(targetRunID, signalName, payload string, timeout time.Duration) (string, error) {
	if h.sendSignalAndWait == nil {
		return "", errors.New("durable: SendSignalAndWait not initialized")
	}
	return h.sendSignalAndWait(targetRunID, signalName, payload, timeout)
}

func (h *hostCallsImpl) ReplyToSignal(correlationID, response string) error {
	if h.replyToSignal == nil {
		return errors.New("durable: ReplyToSignal not initialized")
	}
	return h.replyToSignal(correlationID, response)
}

func (h *hostCallsImpl) AwaitSignalsWithQuorum(signalNames []string, minCount int, maxRejections int, timeout time.Duration) ([]SignalResult, error) {
	if h.awaitSignalsWithQuorum != nil {
		return h.awaitSignalsWithQuorum(signalNames, minCount, maxRejections, timeout)
	}
	// Fallback: poll-based loop using DurableAwaitSignals.
	deadline := time.Now().Add(timeout)
	var results []SignalResult
	rejectionCount := 0
	remaining := signalNames

	for len(results) < minCount {
		remainingTime := time.Until(deadline)
		if remainingTime <= 0 {
			return results, fmt.Errorf("durable: quorum timeout after %v: got %d/%d signals", timeout, len(results), minCount)
		}
		result := h.AwaitSignals(remaining, remainingTime)
		if result.TimedOut {
			return results, fmt.Errorf("durable: quorum timeout after %v: got %d/%d signals", timeout, len(results), minCount)
		}
		if result.Err != nil {
			return results, fmt.Errorf("durable: quorum signal error: %w", result.Err)
		}
		results = append(results, result)

		// Check for rejection if maxRejections >= 0.
		if maxRejections >= 0 {
			var payloadMap map[string]interface{}
			if err := json.Unmarshal([]byte(result.Payload), &payloadMap); err == nil {
				if rejected, ok := payloadMap["rejected"].(bool); ok && rejected {
					rejectionCount++
					if rejectionCount > maxRejections {
						return results, fmt.Errorf("durable: quorum exceeded max rejections (%d)", maxRejections)
					}
				}
			}
		}
	}
	return results, nil
}

func (h *hostCallsImpl) SignalWorkflow(targetRunID, signalName, payload string) error {
	if h.signalWorkflow == nil {
		return errors.New("durable: SignalWorkflow not initialized")
	}
	return h.signalWorkflow(targetRunID, signalName, payload)
}

func (h *hostCallsImpl) DurableSleep(d time.Duration) {
	h.DurableSleepMs(d.Milliseconds())
}

func (h *hostCallsImpl) DurableSleepMs(ms int64) {
	if h.durableSleep == nil {
		panic("durable: DurableSleep not initialized")
	}
	h.durableSleep(ms)
}

func (h *hostCallsImpl) AwaitSignals(signalNames []string, timeout time.Duration) SignalResult {
	name, payload, timedOut, err := h.DurableAwaitSignals(signalNames, timeout.Milliseconds())
	return SignalResult{
		Name:     name,
		Payload:  payload,
		TimedOut: timedOut,
		Err:      err,
	}
}

func (h *hostCallsImpl) DurableAwaitSignals(signalNames []string, timeoutMs int64) (string, string, bool, error) {
	if h.durableAwaitSignals == nil {
		return "", "", false, errors.New("durable: DurableAwaitSignals not initialized")
	}
	return h.durableAwaitSignals(signalNames, timeoutMs)
}

func (h *hostCallsImpl) CreatePromise(name string) (string, error) {
	if h.createPromise == nil {
		return "", errors.New("durable: CreatePromise not initialized")
	}
	return h.createPromise(name)
}

func (h *hostCallsImpl) AwaitPromise(promiseID string, timeout time.Duration) (string, bool, error) {
	if h.awaitPromise == nil {
		return "", false, errors.New("durable: AwaitPromise not initialized")
	}
	return h.awaitPromise(promiseID, timeout)
}

func (h *hostCallsImpl) AwaitPromiseMs(promiseID string, timeoutMs int64) (result string, timedOut bool, err error) {
	return h.AwaitPromise(promiseID, time.Duration(timeoutMs)*time.Millisecond)
}

func (h *hostCallsImpl) ResolvePromise(id, value string) error {
	if h.resolvePromise == nil {
		return errors.New("durable: ResolvePromise not initialized")
	}
	return h.resolvePromise(id, value)
}

func (h *hostCallsImpl) RejectPromise(id, errMsg string) error {
	if h.rejectPromise == nil {
		return errors.New("durable: RejectPromise not initialized")
	}
	return h.rejectPromise(id, errMsg)
}

func (h *hostCallsImpl) DurableDefer(description string) (string, error) {
	if h.durableDefer == nil {
		return "", errors.New("durable: DurableDefer not initialized")
	}
	return h.durableDefer(description)
}

func (h *hostCallsImpl) DurableDeferFunc(fn func()) (string, error) {
	if h.durableDeferFunc == nil {
		return "", errors.New("durable: DurableDeferFunc not initialized")
	}
	return h.durableDeferFunc(fn)
}

func (h *hostCallsImpl) WorkflowID() string {
	if h.workflowID == nil {
		return ""
	}
	return h.workflowID()
}

func (h *hostCallsImpl) RunID() string {
	if h.workflowRunID == nil {
		return ""
	}
	return h.workflowRunID()
}

func (h *hostCallsImpl) DurableLog(message string) {
	if h.durableLog != nil {
		h.durableLog(message)
	}
}

func (h *hostCallsImpl) LogKV(message string, kvs ...interface{}) {
	entry := map[string]interface{}{
		"msg": message,
	}
	if len(kvs) > 0 {
		kvMap := make(map[string]interface{}, len(kvs)/2)
		for i := 0; i+1 < len(kvs); i += 2 {
			key, ok := kvs[i].(string)
			if !ok {
				key = fmt.Sprintf("%v", kvs[i])
			}
			kvMap[key] = kvs[i+1]
		}
		if len(kvs)%2 != 0 {
			kvMap["_unpaired"] = kvs[len(kvs)-1]
		}
		entry["kvs"] = kvMap
	}
	data, _ := json.Marshal(entry)
	h.DurableLog(string(data))
}

func (h *hostCallsImpl) PollCancellation() (bool, string) {
	if h.pollCancellation == nil {
		return false, ""
	}
	return h.pollCancellation()
}

func (h *hostCallsImpl) PollSignal(signalName string) (string, bool, error) {
	if h.pollSignal == nil {
		return "", false, errors.New("durable: PollSignal not initialized")
	}
	return h.pollSignal(signalName)
}

func (h *hostCallsImpl) ContinueAsNew(newInputJSON string) error {
	if h.continueAsNew == nil {
		return errors.New("durable: ContinueAsNew not initialized")
	}
	return h.continueAsNew(newInputJSON)
}

func (h *hostCallsImpl) ChildWorkflow(name, inputJSON string) (string, error) {
	if h.childWorkflow == nil {
		return "", errors.New("durable: ChildWorkflow not initialized")
	}
	return h.childWorkflow(name, inputJSON)
}

func (h *hostCallsImpl) AwaitChild(runID string) (string, error) {
	if h.awaitChild == nil {
		return "", errors.New("durable: AwaitChild not initialized")
	}
	return h.awaitChild(runID)
}

func (h *hostCallsImpl) AwaitAllChildren(runIDs []string) ([]ChildResult, error) {
	if h.awaitAllChildren == nil {
		return nil, errors.New("durable: AwaitAllChildren not initialized")
	}
	return h.awaitAllChildren(runIDs)
}

func (h *hostCallsImpl) ChildWorkflowTyped(name string, request interface{}) (string, error) {
	if h.childWorkflowTyped != nil {
		return h.childWorkflowTyped(name, request)
	}
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("durable: marshaling child workflow input for %s: %w", name, err)
	}
	return h.ChildWorkflow(name, string(reqJSON))
}

func (h *hostCallsImpl) AwaitChildTyped(runID string, result interface{}) error {
	if h.awaitChildTyped != nil {
		return h.awaitChildTyped(runID, result)
	}
	resp, err := h.AwaitChild(runID)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(resp), result); err != nil {
		return fmt.Errorf("durable: unmarshaling child result for %s: %w", runID, err)
	}
	return nil
}

func (h *hostCallsImpl) Version() int {
	if h.version == nil {
		return 1
	}
	return h.version()
}

func (h *hostCallsImpl) MinVersion() int {
	if h.minVersion == nil {
		return 1
	}
	return h.minVersion()
}

func (h *hostCallsImpl) SetQueryState(key, value string) {
	if h.setQueryState != nil {
		h.setQueryState(key, value)
	}
	// Also store in local state map for typed access.
	if h.stateMap == nil {
		h.stateMap = make(map[string]interface{})
	}
	h.stateMap[key] = value
}

// scopedKey returns the internally-stored key, applying the current
// virtual-object scope prefix when one is active.
func (h *hostCallsImpl) scopedKey(key string) string {
	if h.scopeSet && h.scopePrefix != "" {
		return h.scopePrefix + key
	}
	return key
}

// SetScope sets the state key prefix for virtual object instances.
// All subsequent SetState/GetState/etc calls are automatically prefixed
// with "vo:<objectType>:<instanceKey>:". Returns the previous scope
// prefix for stack-style save/restore.
func (h *hostCallsImpl) SetScope(objectType, instanceKey string) (previousScope string) {
	if h.scopeSet {
		previousScope = h.scopePrefix
	}
	if objectType == "" && instanceKey == "" {
		h.scopeSet = false
		h.scopePrefix = ""
		h.scopeObjType = ""
		h.scopeInstKey = ""
	} else {
		h.scopeSet = true
		h.scopeObjType = objectType
		h.scopeInstKey = instanceKey
		h.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
	}
	return
}

// GetScope returns the current (objectType, instanceKey) or ("", "")
// if no scope is set.
func (h *hostCallsImpl) GetScope() (objectType, instanceKey string) {
	if !h.scopeSet {
		return "", ""
	}
	return h.scopeObjType, h.scopeInstKey
}

// ClearScope removes the current scope and returns the previous scope
// prefix (empty string if none was set).
func (h *hostCallsImpl) ClearScope() (previousScope string) {
	if h.scopeSet {
		previousScope = h.scopePrefix
	}
	h.scopeSet = false
	h.scopePrefix = ""
	h.scopeObjType = ""
	h.scopeInstKey = ""
	return
}

// UUID returns a deterministic UUID scoped to the current workflow
// and the given seed. Same seed always produces the same UUID for
// this workflow instance.
func (h *hostCallsImpl) UUID(seed string) string {
	wfID := h.WorkflowID()
	data := wfID + ":" + seed
	hash := sha256.Sum256([]byte(data))
	// Format as UUIDv5-like value (first 16 bytes of SHA-256, version bits set).
	hash[6] = (hash[6] & 0x0f) | 0x50 // Version 5
	hash[8] = (hash[8] & 0x3f) | 0x80 // Variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}

func (h *hostCallsImpl) SetState(key string, value interface{}) {
	sk := h.scopedKey(key)
	if h.stateMap == nil {
		h.stateMap = make(map[string]interface{})
	}
	// Store as json.RawMessage so GetState can unmarshal directly.
	data, err := json.Marshal(value)
	if err != nil {
		h.stateMap[sk] = value // fallback to raw value
	} else {
		h.stateMap[sk] = json.RawMessage(data)
	}
	// Persist via existing set_query_state mechanism.
	if h.setQueryState != nil {
		if data == nil {
			data, _ = json.Marshal(value)
		}
		h.setQueryState(sk, string(data))
	}
}

func (h *hostCallsImpl) GetState(key string, result interface{}) error {
	sk := h.scopedKey(key)
	if h.stateMap == nil {
		return errors.New("durable: state not found for key: " + sk)
	}
	val, ok := h.stateMap[sk]
	if !ok {
		return errors.New("durable: state key not found: " + sk)
	}
	// If val is already json.RawMessage, unmarshal directly.
	if raw, ok := val.(json.RawMessage); ok {
		return json.Unmarshal(raw, result)
	}
	// Otherwise marshal and unmarshal for consistent type conversion.
	data, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("durable: marshal state value: %w", err)
	}
	return json.Unmarshal(data, result)
}

func (h *hostCallsImpl) DeleteState(key string) {
	sk := h.scopedKey(key)
	if h.stateMap != nil {
		delete(h.stateMap, sk)
	}
	if h.setQueryState != nil {
		h.setQueryState(sk, "")
	}
}

func (h *hostCallsImpl) HasState(key string) bool {
	if h.stateMap == nil {
		return false
	}
	_, ok := h.stateMap[h.scopedKey(key)]
	return ok
}

func (h *hostCallsImpl) IncrState(key string, delta int64) int64 {
	sk := h.scopedKey(key)
	if h.stateMap == nil {
		h.stateMap = make(map[string]interface{})
	}
	var current int64
	if val, ok := h.stateMap[sk]; ok {
		switch v := val.(type) {
		case int64:
			current = v
		case float64:
			current = int64(v)
		case json.Number:
			current, _ = v.Int64()
		default:
			current = 0
		}
	}
	current += delta
	h.stateMap[sk] = current
	// Persist via existing set_query_state mechanism.
	if h.setQueryState != nil {
		data, err := json.Marshal(current)
		if err == nil {
			h.setQueryState(sk, string(data))
		}
	}
	return current
}

func (h *hostCallsImpl) ListState(prefix string) []string {
	if h.stateMap == nil {
		return nil
	}
	sk := h.scopedKey(prefix)
	var keys []string
	for k := range h.stateMap {
		if sk == "" || strings.HasPrefix(k, sk) {
			// Strip scope prefix from returned key names.
			if h.scopeSet && h.scopePrefix != "" && strings.HasPrefix(k, h.scopePrefix) {
				keys = append(keys, k[len(h.scopePrefix):])
			} else {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func (h *hostCallsImpl) RegisterUpdateHandler(name string, handler func(payloadJSON string) (resultJSON string, err error), validator func(payloadJSON string) error) {
	if h.updateHandlers == nil {
		h.updateHandlers = make(map[string]updateHandlerEntry)
	}
	h.updateHandlers[name] = updateHandlerEntry{handler: handler, validator: validator}
	if h.registerUpdateHandler != nil {
		h.registerUpdateHandler(name)
	}
}

// RegisterTypedUpdateHandler registers a typed update handler.
// handler receives a typed request and returns a typed response; validator
// receives the typed request. Both are called during workflow init (before
// durable ops) and replayed deterministically.
//
// Usage:
//
//	durable.RegisterTypedUpdateHandler[ApprovePayload, ApproveResult](h, "approve",
//	    func(payload ApprovePayload) (ApproveResult, error) {
//	        return ApproveResult{Approved: true}, nil
//	    },
//	    func(payload ApprovePayload) error {
//	        if payload.Amount <= 0 { return errors.New("invalid amount") }
//	        return nil
//	    },
//	)
func RegisterTypedUpdateHandler[TReq, TResp any](h HostCalls, name string, handler func(TReq) (TResp, error), validator func(TReq) error) {
	h.RegisterUpdateHandler(name,
		func(payloadJSON string) (string, error) {
			var req TReq
			if err := json.Unmarshal([]byte(payloadJSON), &req); err != nil {
				return "", fmt.Errorf("durable: unmarshal update payload for %q: %w", name, err)
			}
			resp, err := handler(req)
			if err != nil {
				return "", err
			}
			respBytes, err := json.Marshal(resp)
			if err != nil {
				return "", fmt.Errorf("durable: marshal update response for %q: %w", name, err)
			}
			return string(respBytes), nil
		},
		func(payloadJSON string) error {
			var req TReq
			if err := json.Unmarshal([]byte(payloadJSON), &req); err != nil {
				return fmt.Errorf("durable: unmarshal update payload for %q: %w", name, err)
			}
			return validator(req)
		},
	)
}

func (h *hostCallsImpl) RegisterQueryHandler(name string, handler func(payloadJSON string) (resultJSON string, err error)) {
	if h.queryHandlers == nil {
		h.queryHandlers = make(map[string]func(payloadJSON string) (resultJSON string, err error))
	}
	h.queryHandlers[name] = handler
	if h.registerQueryHandler != nil {
		h.registerQueryHandler(name)
	}
}

// HandleQuery invokes a registered query handler by name with the given payload.
func (h *hostCallsImpl) HandleQuery(name, payload string) (string, error) {
	if h.handleQuery != nil {
		return h.handleQuery(name, payload)
	}
	handler, ok := h.queryHandlers[name]
	if !ok {
		return "", fmt.Errorf("durable: no query handler registered for %q", name)
	}
	return handler(payload)
}

func (h *hostCallsImpl) RunDetached(fn func(h HostCalls) error) error {
	if h.runDetached != nil {
		return h.runDetached(fn)
	}
	return nil
}

func (h *hostCallsImpl) DurableFetch(url, method string, headers map[string]string, body string) (responseJSON string, statusCode int, err error) {
	requestMap := map[string]interface{}{
		"url":     url,
		"method":  method,
		"headers": headers,
		"body":    body,
	}
	requestJSON, marshalErr := json.Marshal(requestMap)
	if marshalErr != nil {
		return "", 0, fmt.Errorf("durable: marshal fetch request: %w", marshalErr)
	}
	resp, callErr := h.DurableCall("http", "fetch", string(requestJSON))
	if callErr != nil {
		return "", 0, callErr
	}
	var respData struct {
		Body       string `json:"body"`
		StatusCode int    `json:"status_code"`
	}
	if unmarshalErr := json.Unmarshal([]byte(resp), &respData); unmarshalErr != nil {
		return "", 0, fmt.Errorf("durable: unmarshal fetch response: %w", unmarshalErr)
	}
	return respData.Body, respData.StatusCode, nil
}

func (h *hostCallsImpl) DurableFetchJSON(url, method string, headers map[string]string, body string, result interface{}) error {
	resp, _, err := h.DurableFetch(url, method, headers, body)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(resp), result)
}

// FetchGet is a shorthand for DurableFetch with GET method, no headers, and no body.
func (h *hostCallsImpl) FetchGet(url string) (responseJSON string, statusCode int, err error) {
	return h.DurableFetch(url, "GET", nil, "")
}

// FetchGetJSON is like FetchGet but unmarshals the response into result.
func (h *hostCallsImpl) FetchGetJSON(url string, result interface{}) error {
	resp, _, err := h.FetchGet(url)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(resp), result)
}

func (h *hostCallsImpl) Now() time.Time {
	ms := h.NowMs()
	return time.Unix(ms/1000, (ms%1000)*1_000_000)
}

func (h *hostCallsImpl) NowMs() int64 {
	if h.now == nil {
		panic("durable: Now not initialized")
	}
	return h.now()
}

func (h *hostCallsImpl) Random() int64 {
	if h.random == nil {
		panic("durable: Random not initialized")
	}
	return h.random()
}

// ---- Saga: structured compensation ----

// SagaStep defines a single step in a Saga with its forward action and
// compensation function. Create instances via NewSaga().AddStep() or
// by constructing a SagaStep literal for use with AddParallel().
type SagaStep struct {
	Description string
	Forward     func(HostCalls) (string, error)
	Compensate  func(HostCalls) error
}

// Saga provides structured compensation for multi-step operations.
// Steps execute in order. If any step fails, all previously completed
// steps are compensated in reverse order.
//
// Usage:
//
//	s := durable.NewSaga()
//	s.AddStep("charge", chargeFn, refundFn)
//	s.AddStep("assign_driver", assignFn, releaseFn)
//	if err := s.Run(h); err != nil {
//	    return err
//	}
//
// Typed usage (recommended):
//
//	s := durable.NewSaga()
//	s.AddStep("book_flight",
//	    func(h HostCalls) (string, error) {
//	        var result FlightResult
//	        err := h.DurableCallTyped("flights", "Book", req, &result)
//	        return "", err
//	    },
//	    func(h HostCalls) {
//	        h.DurableCall("flights", "Cancel", cancelJSON)
//	    },
//	)
type Saga struct {
	steps []SagaStep
}

// NewSaga creates a new Saga helper.
func NewSaga() *Saga {
	return &Saga{}
}

// AddStep adds a step to the saga. forward is the main action; compensate
// is the cleanup if a later step fails. description is used for logging.
// compensate may be nil for best-effort steps that have no meaningful
// compensation (e.g., sending a notification).
//
// Typed usage example:
//
//	s.AddStep("book_flight",
//	    func(h HostCalls) (string, error) {
//	        var result FlightResult
//	        err := h.DurableCallTyped("flights", "Book", req, &result)
//	        return "", err
//	    },
//	    func(h HostCalls) {
//	        h.DurableCall("flights", "Cancel", cancelJSON)
//	    },
//	)
func (s *Saga) AddStep(description string, forward func(HostCalls) (string, error), compensate func(HostCalls) error) *Saga {
	s.steps = append(s.steps, SagaStep{
		Description: description,
		Forward:     forward,
		Compensate:  compensate,
	})
	return s
}

// Run executes all forward steps in order. If any step fails, previously
// completed steps are compensated in reverse order. Nil compensate functions
// are skipped. The first forward error encountered is returned.
func (s *Saga) Run(h HostCalls) error {
	var completed int
	for i, step := range s.steps {
		h.LogKV("saga: executing step", "step", i, "description", step.Description)
		_, err := step.Forward(h)
		if err != nil {
			// Only compensate on TerminalError (non-retryable). For retryable
			// errors, the caller should retry the entire saga.
			if !IsTerminalError(err) {
				return fmt.Errorf("saga: %w", err)
			}
			h.LogKV("saga: step failed, compensating",
				"step", i,
				"description", step.Description,
				"error", err.Error(),
				"completed_count", completed)
			var compErr error
			for j := completed - 1; j >= 0; j-- {
				cs := s.steps[j]
				if cs.Compensate == nil {
					continue
				}
				h.LogKV("saga: compensating", "step", j, "description", cs.Description)
				if cerr := cs.Compensate(h); cerr != nil {
					compErr = errors.Join(compErr, cerr)
				}
			}
			if compErr != nil {
				return fmt.Errorf("saga: %w", errors.Join(err, compErr))
			}
			return fmt.Errorf("saga: %w", err)
		}
		completed++
	}
	return nil
}

// AddParallel adds multiple steps that execute concurrently. If any step
// fails, all successfully completed parallel steps are compensated in
// LIFO order. The returned values are collected into a slice in the same
// order as the steps were added.
func (s *Saga) AddParallel(steps ...SagaStep) *Saga {
	s.steps = append(s.steps, SagaStep{
		Description: "parallel",
		Forward: func(h HostCalls) (string, error) {
			type stepResult struct {
				index  int
				result string
				err    error
			}

			results := make([]stepResult, len(steps))
			var wg sync.WaitGroup

			for i, step := range steps {
				wg.Add(1)
				go func(idx int, st SagaStep) {
					defer wg.Done()
					// Each step runs in its own goroutine.
					// All durable calls within each step are recorded
					// deterministically through the same HostCalls.
					res, err := st.Forward(h)
					results[idx] = stepResult{index: idx, result: res, err: err}
				}(i, step)
			}
			wg.Wait()

			// Check for failures in order.
			var firstErr error
			for _, r := range results {
				if r.err != nil && firstErr == nil {
					firstErr = r.err
				}
			}

			if firstErr != nil {
				// Compensate successful steps in LIFO order.
				var compErr error
				for i := len(results) - 1; i >= 0; i-- {
					if results[i].err == nil && steps[i].Compensate != nil {
						if cerr := steps[i].Compensate(h); cerr != nil {
							compErr = errors.Join(compErr, cerr)
						}
					}
				}
				if compErr != nil {
					return "", fmt.Errorf("%w (compensation failures: %v)", firstErr, compErr)
				}
				return "", firstErr
			}

			// All succeeded — collect results.
			var out []string
			for _, r := range results {
				out = append(out, r.result)
			}
			b, _ := json.Marshal(out)
			return string(b), nil
		},
		// No single description for parallel steps — the forward closure
		// handles everything internally.
	})
	return s
}

// ---- SagaTyped: typed result collection ----

// SagaStepTyped defines a single saga step with a typed result.
// Generic parameter T is the forward action's result type.
type SagaStepTyped[T any] struct {
	Description string
	Forward     func(HostCalls) (T, error)
	Compensate  func(HostCalls) error
}

// SagaTyped provides structured compensation with typed result collection.
// Generic parameter T is the result type of each step.
//
// Usage:
//
//	saga := durable.NewSagaTyped[ChargeResult]()
//	saga.AddStep("charge",
//	    func(h HostCalls) (ChargeResult, error) {
//	        var result ChargeResult
//	        err := h.DurableCallTyped("payment", "charge", req, &result)
//	        return result, err
//	    },
//	    func(h HostCalls) error {
//	        h.DurableCall("payment", "refund", refundJSON)
//	        return nil
//	    },
//	)
//	results, err := saga.Run(h)
type SagaTyped[T any] struct {
	steps []SagaStepTyped[T]
}

// NewSagaTyped creates a new SagaTyped helper.
func NewSagaTyped[T any]() *SagaTyped[T] {
	return &SagaTyped[T]{}
}

// AddStep adds a typed step to the saga. forward returns a (T, error);
// compensate runs on failure of a later step. compensate may be nil
// for best-effort steps.
func (s *SagaTyped[T]) AddStep(description string, forward func(HostCalls) (T, error), compensate func(HostCalls) error) *SagaTyped[T] {
	s.steps = append(s.steps, SagaStepTyped[T]{
		Description: description,
		Forward:     forward,
		Compensate:  compensate,
	})
	return s
}

// Run executes all typed forward steps in order, collecting results.
// If any step fails, previously completed steps are compensated in
// reverse order. Returns the collected results or the first error.
//
// Only TerminalError triggers compensation (non-retryable). Transient
// errors are returned without compensation so the caller can retry.
func (s *SagaTyped[T]) Run(h HostCalls) ([]T, error) {
	var completed int
	var results []T

	for i, step := range s.steps {
		h.LogKV("saga: executing typed step", "step", i, "description", step.Description)
		result, err := step.Forward(h)
		if err != nil {
			if !IsTerminalError(err) {
				return results, fmt.Errorf("saga: %w", err)
			}
			h.LogKV("saga: typed step failed, compensating",
				"step", i, "description", step.Description,
				"error", err.Error(), "completed_count", completed)
			var compErr error
			for j := completed - 1; j >= 0; j-- {
				cs := s.steps[j]
				if cs.Compensate == nil {
					continue
				}
				h.LogKV("saga: compensating", "step", j, "description", cs.Description)
				if cerr := cs.Compensate(h); cerr != nil {
					compErr = errors.Join(compErr, cerr)
				}
			}
			if compErr != nil {
				return results, fmt.Errorf("saga: %w", errors.Join(err, compErr))
			}
			return results, fmt.Errorf("saga: %w", err)
		}
		results = append(results, result)
		completed++
	}

	return results, nil
}

// ---- PollUntil: sleep-based polling ----

// PollUntil repeatedly calls pollFn at the given interval until pollFn
// returns done=true, or the deadline is exceeded. Returns the last value
// from pollFn and any error.
//
// Usage:
//
//	status, err := durable.PollUntil(h, 30*time.Second, 30*time.Minute,
//	    func() (string, error) {
//	        return checkPickupStatus(driverID)
//	    },
//	    func(s string) bool { return s == "picked_up" },
//	)
func PollUntil[T any](h HostCalls, interval, timeout time.Duration,
	fn func() (T, error), done func(T) bool) (T, error) {

	deadline := h.Now().Add(timeout)
	var zero T
	for {
		val, err := fn()
		if err != nil {
			return zero, err
		}
		if done(val) {
			return val, nil
		}
		if h.Now().After(deadline) {
			return zero, fmt.Errorf("poll deadline exceeded after %v", timeout)
		}
		h.DurableSleep(interval)
	}
}

// ---- Helpers ----

// isNonRetryable returns true if err matches any of the non-retryable
// substrings.
func isNonRetryable(err error, nonRetryableErrors []string) bool {
	errMsg := err.Error()
	for _, substr := range nonRetryableErrors {
		if strings.Contains(errMsg, substr) {
			return true
		}
	}
	return false
}
