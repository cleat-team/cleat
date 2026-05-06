// Package durable defines the shared types and host interface between
// the WASM workflow module and the host runtime.
//
// This is the "durable SDK" — the only import the workflow author needs.
// Everything else comes from ordinary Go and the code transformer.
package durable

import (
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

	// RunDetached runs fn with a fresh HostCalls that ignores cancellation.
	// fn executes immediately, is recorded in history, and survives crash/replay.
	// On replay, fn IS re-executed (not replayed from cache).
	RunDetached(fn func(h HostCalls) error) error

	// DurableFetch makes an HTTP request as a durable operation.
	// Delegates to DurableCall("http", "fetch", requestJSON) internally.
	DurableFetch(url, method string, headers map[string]string, body string) (responseJSON string, statusCode int, err error)

	// DurableFetchJSON is like DurableFetch but unmarshals the response into result.
	DurableFetchJSON(url, method string, headers map[string]string, body string, result interface{}) error

	// Now returns the current wall-clock time. Use instead of time.Now()
	// for deterministic replay.
	Now() time.Time

	// NowMs returns the current wall-clock time in milliseconds since epoch.
	// Prefer Now() for readability.
	NowMs() int64

	// Random returns a deterministic random number seeded from the event
	// history. Use instead of math/rand for deterministic replay.
	Random() int64
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

// ---- Call options ----

// CallOptions provides per-call configuration.
type CallOptions struct {
	Retry           *RetryPolicy
	MaxResponseSize int // 0 = use default (64KB), capped at outBufSize
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

// ---- Concrete implementation ----

// hostCallsImpl is the default concrete implementation of HostCalls.
// The WASM host adapter populates the function fields with closures that
// call into the WASM host imports.
type hostCallsImpl struct {
	durableCall               func(service, operation, requestJSON string) (string, error)
	durableCallTyped          func(service, operation string, request, result interface{}) error
	durableCallWithOptions    func(opts CallOptions, service, operation, requestJSON string) (string, error)
	durableCallJSONWithOptions func(opts CallOptions, service, operation, requestJSON string, result interface{}) error
	durableCallWithHeartbeat   func(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(string)) (string, error)
	durableSleep              func(ms int64)
	durableAwaitSignals       func(signalNames []string, timeoutMs int64) (string, string, bool, error)
	createPromise    func(name string) (promiseID string, err error)
	awaitPromise     func(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)
	durableDefer              func(description string) (string, error)
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
	runDetached               func(fn func(h HostCalls) error) error
	now                       func() int64
	random                    func() int64

	// State map for typed K/V operations.
	stateMap       map[string]interface{}
	updateHandlers map[string]updateHandlerEntry
}

// NewHostCalls creates a HostCalls from a set of function implementations.
// Used by the WASM host adapter and by tests.
func NewHostCalls(opts HostCallsOptions) HostCalls {
	return &hostCallsImpl{
		durableCall:               opts.DurableCall,
		durableCallTyped:          opts.DurableCallTyped,
		durableCallWithOptions:    opts.DurableCallWithOptions,
		durableCallJSONWithOptions: opts.DurableCallJSONWithOptions,
		durableCallWithHeartbeat:   opts.DurableCallWithHeartbeat,
		durableSleep:              opts.DurableSleep,
		durableAwaitSignals:       opts.DurableAwaitSignals,
		createPromise:              opts.CreatePromise,
		awaitPromise:               opts.AwaitPromise,
		durableDefer:              opts.DurableDefer,
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
		runDetached:               opts.RunDetached,
		now:                       opts.Now,
		random:                    opts.Random,
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
	DurableCallWithOptions    func(opts CallOptions, service, operation, requestJSON string) (string, error)
	DurableCallJSONWithOptions func(opts CallOptions, service, operation, requestJSON string, result interface{}) error
	DurableCallWithHeartbeat   func(service, operation, requestJSON string, heartbeatInterval time.Duration, onProgress func(string)) (string, error)
	DurableSleep              func(ms int64)
	DurableAwaitSignals       func(signalNames []string, timeoutMs int64) (string, string, bool, error)
	CreatePromise func(name string) (promiseID string, err error)
	AwaitPromise  func(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)
	DurableDefer              func(description string) (string, error)
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
	RunDetached               func(fn func(h HostCalls) error) error
	Now                       func() int64
	Random                    func() int64
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

func (h *hostCallsImpl) DurableCallJSONWithOptions(opts CallOptions, service, operation, requestJSON string, result interface{}) error {
	resp, err := h.DurableCallWithOptions(opts, service, operation, requestJSON)
	if err != nil {
		return err
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
	if err := json.Unmarshal([]byte(resp), result); err != nil {
		return fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
	}
	return nil
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

func (h *hostCallsImpl) DurableDefer(description string) (string, error) {
	if h.durableDefer == nil {
		return "", errors.New("durable: DurableDefer not initialized")
	}
	return h.durableDefer(description)
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

func (h *hostCallsImpl) SetState(key string, value interface{}) {
	if h.stateMap == nil {
		h.stateMap = make(map[string]interface{})
	}
	h.stateMap[key] = value
	// Persist via existing set_query_state mechanism.
	if h.setQueryState != nil {
		data, err := json.Marshal(value)
		if err == nil {
			h.setQueryState(key, string(data))
		}
	}
}

func (h *hostCallsImpl) GetState(key string, result interface{}) error {
	if h.stateMap == nil {
		return errors.New("durable: state not found for key: " + key)
	}
	val, ok := h.stateMap[key]
	if !ok {
		return errors.New("durable: state key not found: " + key)
	}
	data, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("durable: marshal state value: %w", err)
	}
	return json.Unmarshal(data, result)
}

func (h *hostCallsImpl) DeleteState(key string) {
	if h.stateMap != nil {
		delete(h.stateMap, key)
	}
	if h.setQueryState != nil {
		h.setQueryState(key, "")
	}
}

func (h *hostCallsImpl) HasState(key string) bool {
	if h.stateMap == nil {
		return false
	}
	_, ok := h.stateMap[key]
	return ok
}

func (h *hostCallsImpl) IncrState(key string, delta int64) int64 {
	if h.stateMap == nil {
		h.stateMap = make(map[string]interface{})
	}
	var current int64
	if val, ok := h.stateMap[key]; ok {
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
	h.stateMap[key] = current
	// Persist via existing set_query_state mechanism.
	if h.setQueryState != nil {
		data, err := json.Marshal(current)
		if err == nil {
			h.setQueryState(key, string(data))
		}
	}
	return current
}

func (h *hostCallsImpl) ListState(prefix string) []string {
	if h.stateMap == nil {
		return nil
	}
	var keys []string
	for k := range h.stateMap {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
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
	Compensate  func(HostCalls)
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
func (s *Saga) AddStep(description string, forward func(HostCalls) (string, error), compensate func(HostCalls)) *Saga {
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
			h.LogKV("saga: step failed, compensating",
				"step", i,
				"description", step.Description,
				"error", err.Error(),
				"completed_count", completed)
			for j := completed - 1; j >= 0; j-- {
				cs := s.steps[j]
				if cs.Compensate == nil {
					continue
				}
				h.LogKV("saga: compensating", "step", j, "description", cs.Description)
				cs.Compensate(h)
			}
			return fmt.Errorf("saga step %q failed: %w", step.Description, err)
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
				for i := len(results) - 1; i >= 0; i-- {
					if results[i].err == nil && steps[i].Compensate != nil {
						steps[i].Compensate(h)
					}
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
