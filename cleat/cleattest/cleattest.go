// Package cleattest provides a mock HostCalls implementation for testing
// workflows without compiling to WASM or running a full host.
package cleattest

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rcownie/cleat/cleat"
)

// TestingT is the interface required by AssertCalled and AssertNotCalled.
// *testing.T satisfies this interface.
type TestingT interface {
	Fatalf(format string, args ...interface{})
}

// CallRecord records a single durable API call made through the test env.
type CallRecord struct {
	Service   string
	Operation string
	Request   string
	Response  string
	Err       error
	RetryCount int // Number of retry attempts before this call succeeded (0 if no retry)
}

// ChildWorkflowCallRecord records a child workflow invocation made through the test env.
type ChildWorkflowCallRecord struct {
	Name      string
	InputJSON string
	RunID     string
	Err       error
}

// CallStubBuilder builds and registers a call stub.
// Created by TestEnv.OnCall.
type CallStubBuilder struct {
	env       *TestEnv
	service   string
	operation string
	matcher   func(string) bool
}

// Return registers a stub that returns the given response string and error.
func (b *CallStubBuilder) Return(response string, err error) *CallStubBuilder {
	b.env.mu.Lock()
	defer b.env.mu.Unlock()
	b.env.callStubs = append(b.env.callStubs, &callStub{
		service:   b.service,
		operation: b.operation,
		matcher:   b.matcher,
		response:  response,
		err:       err,
	})
	return b
}

// ReturnJSON marshals v as JSON and registers a stub returning that JSON.
func (b *CallStubBuilder) ReturnJSON(v interface{}, err error) *CallStubBuilder {
	data, marshalErr := json.Marshal(v)
	if marshalErr != nil {
		panic(fmt.Sprintf("cleattest: ReturnJSON marshal error: %v", marshalErr))
	}
	return b.Return(string(data), err)
}

// callStub is an internal stub record.
type callStub struct {
	service   string
	operation string
	matcher   func(string) bool
	response  string
	err       error
}

// childStubResult holds the resolved result of a child workflow stub.
type childStubResult struct {
	result string
	err    error
}

// childWorkflowStub stores a registered stub for a child workflow.
type childWorkflowStub struct {
	name   string
	result string
	err    error
}

// retryBehavior stores the per-service/operation retry configuration.
type retryBehavior struct {
	service       string
	operation     string
	failCount     int
	finalResponse string
	attempts      int // current attempt count
}

// ChildWorkflowStubBuilder builds and registers a child workflow stub.
// Created by TestEnv.OnChildWorkflow.
type ChildWorkflowStubBuilder struct {
	env  *TestEnv
	name string
}

// Return registers a stub that returns the given result and error for a child workflow.
func (b *ChildWorkflowStubBuilder) Return(result string, err error) *ChildWorkflowStubBuilder {
	b.env.mu.Lock()
	defer b.env.mu.Unlock()
	b.env.childWorkflowStubs[b.name] = &childWorkflowStub{
		name:   b.name,
		result: result,
		err:    err,
	}
	return b
}

// scheduledSignal is a signal waiting to be delivered at a specific time.
type scheduledSignal struct {
	name    string
	payload string
	time    time.Time
}

// signalWaiter is a goroutine waiting for a matching signal.
type signalWaiter struct {
	names    []string
	deadline time.Time
	ch       chan scheduledSignal
}

// sleepRecord tracks a goroutine sleeping until a specific time.
type sleepRecord struct {
	wakeAt time.Time
	wake   chan struct{}
}

// promiseState holds the state of a durable promise in the test environment.
type promiseState struct {
	name     string
	status   string // "pending", "resolved", "rejected"
	result   string
	errorMsg string
}

type pluginCallStub struct {
	pluginName   string
	functionName string
	result       string
	err          error
}

type PluginCallStubBuilder struct {
	env          *TestEnv
	pluginName   string
	functionName string
}

func (b *PluginCallStubBuilder) Return(result string, err error) {
	b.env.pluginCallStubs = append(b.env.pluginCallStubs, &pluginCallStub{
		pluginName:   b.pluginName,
		functionName: b.functionName,
		result:       result,
		err:          err,
	})
}

func (e *TestEnv) OnPluginCall(pluginName, functionName string) *PluginCallStubBuilder {
	return &PluginCallStubBuilder{env: e, pluginName: pluginName, functionName: functionName}
}

// TestEnvOption configures a TestEnv.
type TestEnvOption func(*TestEnv)

// WithRetrySimulation configures the test env to simulate retries by
// failing the first n calls to any service+operation before succeeding.
// When n <= 0 (default), no retry simulation is applied.
// Calls matching the simulated retry pattern will fail the first n times
// with a transient error, then succeed on the (n+1)th attempt.
func WithRetrySimulation(n int) TestEnvOption {
	return func(e *TestEnv) {
		e.retrySimCount = n
	}
}

// SetRetryBehavior configures a per-service/operation retry behavior.
// Calls to the given service+operation will fail the first failCount times
// with a transient error, then succeed with the given finalResponse on
// call (failCount+1). This takes priority over the global WithRetrySimulation.
func (e *TestEnv) SetRetryBehavior(service, operation string, failCount int, finalResponse string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := service + "/" + operation
	e.retryBehaviors[key] = &retryBehavior{
		service:       service,
		operation:     operation,
		failCount:     failCount,
		finalResponse: finalResponse,
		attempts:      0,
	}
}

// RegisterChildStub registers a stub for a child workflow with the given name.
// When a child workflow with this name is started, it returns the pre-configured
// response string and no error. Supports multiple different child workflow names.
func (e *TestEnv) RegisterChildStub(name, response string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.childWorkflowStubs[name] = &childWorkflowStub{
		name:   name,
		result: response,
		err:    nil,
	}
}

// ChildWorkflowCallHistory returns a copy of all recorded child workflow calls.
func (e *TestEnv) ChildWorkflowCallHistory() []ChildWorkflowCallRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]ChildWorkflowCallRecord, len(e.childWorkflowCallHistory))
	copy(result, e.childWorkflowCallHistory)
	return result
}

// TestEnv is a mock environment for testing workflows.
// Use NewTestEnv to create one, then wire up stubs with OnCall
// and drive the workflow via the HostCalls returned by H().
type TestEnv struct {
	mu             sync.Mutex
	h              cleat.HostCalls
	nowMs          int64
	versionVal     int
	minVersionVal  int
	queryState     map[string]string
	callHistory    []CallRecord
	callStubs      []*callStub
	pendingSignals []scheduledSignal
	sleepRecs      []sleepRecord
	signalWaiters  []signalWaiter
	randomSeq      []int64
	randomIdx      int
	deferCounter   int
	promises       map[string]promiseState // keyed by promiseID

	childWorkflowStubs map[string]*childWorkflowStub
	childResults       map[string]*childStubResult

	// childWorkflowHandlers maps child workflow names to handler functions.
	// These take priority over stub results when set.
	childWorkflowHandlers map[string]func(inputJSON string) (resultJSON string, err error)

	// retrySimCount is the number of times a DurableCall is failed with a
	// transient error before succeeding. 0 means no simulation.
	retrySimCount    int
	retrySimAttempts map[string]int // key: "service/operation" -> attempt count

	// retryBehaviors stores per-service/operation retry configurations
	// set via SetRetryBehavior. These take priority over the global retrySimCount.
	retryBehaviors map[string]*retryBehavior // key: "service/operation" -> behavior

	// childWorkflowCallHistory records all child workflow invocations.
	childWorkflowCallHistory []ChildWorkflowCallRecord

	ConcurrencyKeys           map[string]string
	AcquireConcurrencyKeyFn   func(key, workflowID string) (bool, error)
	ReleaseConcurrencyKeysFn  func(workflowID string)

	pluginCallStubs []*pluginCallStub

	// signalReplyChannels maps correlation IDs to reply channels for
	// SendSignalAndWait / ReplyToSignal.
	signalReplyChannels map[string]chan string
}

// NewTestEnv creates a new TestEnv with a clean initial state.
// The simulated clock starts at 2024-01-01T00:00:00Z.
// Optional TestEnvOption arguments can be passed to configure behavior
// (e.g., WithRetrySimulation).
func NewTestEnv(opts ...TestEnvOption) *TestEnv {
	e := &TestEnv{
		nowMs:         time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		versionVal:    1,
		minVersionVal: 1,
		queryState:    make(map[string]string),
		promises:      make(map[string]promiseState),
		childWorkflowStubs: make(map[string]*childWorkflowStub),
		childResults:       make(map[string]*childStubResult),
		childWorkflowHandlers: make(map[string]func(inputJSON string) (resultJSON string, err error)),
		retryBehaviors:       make(map[string]*retryBehavior),
		childWorkflowCallHistory: make([]ChildWorkflowCallRecord, 0),
		ConcurrencyKeys: make(map[string]string),
			signalReplyChannels: make(map[string]chan string),
	}
	for _, opt := range opts {
		opt(e)
	}
	e.h = cleat.NewHostCalls(cleat.HostCallsOptions{
		DurableCall:                  e.durableCallImpl,
		DurableCallWithOptions:       e.durableCallWithOptionsImpl,
		DurableSleep:                 e.durableSleepImpl,
		DurableAwaitSignals:          e.durableAwaitSignalsImpl,
		DurableDefer:                 e.durableDeferImpl,
		DurableLog:                   e.durableLogImpl,
		PollCancellation:             e.pollCancellationImpl,
		PollSignal:                   e.pollSignalImpl,
		ContinueAsNew:                e.continueAsNewImpl,
		ChildWorkflow:                e.childWorkflowImpl,
		AwaitChild:                   e.awaitChildImpl,
		AwaitAllChildren:             e.awaitAllChildrenImpl,
		ChildWorkflowTyped:           e.childWorkflowTypedImpl,
		AwaitChildTyped:              e.awaitChildTypedImpl,
		DurableCallTypedWithHeartbeat: e.durableCallTypedWithHeartbeatImpl,
		Version:                      e.versionImpl,
		MinVersion:                   e.minVersionImpl,
		SetQueryState:                e.setQueryStateImpl,
		Now:                          e.nowImpl,
		Random:                       e.randomImpl,
		CreatePromise:                e.createPromiseImpl,
		AwaitPromise:                 e.awaitPromiseImpl,
		RegisterUpdateHandler:        e.registerUpdateHandlerImpl,
		RegisterQueryHandler:        e.registerQueryHandlerImpl,
		RunDetached:                  e.runDetachedImpl,
		PluginCall: e.pluginCallImpl,
			SendSignalAndWait:            e.sendSignalAndWaitImpl,
			ReplyToSignal:                e.replyToSignalImpl,
			SignalWorkflow:               e.signalWorkflowImpl,
			AcquireLock:                   e.acquireLockImpl,
			ReleaseLock:                   e.releaseLockImpl,
			AwaitCondition:               e.awaitConditionImpl,
			SideEffect:                    e.sideEffectImpl,
	})
	return e
}

// H returns the HostCalls interface for workflow code to use.
func (e *TestEnv) H() cleat.HostCalls {
	return e.h
}

// OnCall registers a call stub and returns a builder for setting the response.
//   - nil requestMatcher matches any request string.
//   - string requestMatcher matches only that exact string.
//   - func(string) bool requestMatcher calls the function to match.
func (e *TestEnv) OnCall(service, operation string, requestMatcher interface{}) *CallStubBuilder {
	var matcher func(string) bool
	if requestMatcher == nil {
		matcher = func(_ string) bool { return true }
	} else if m, ok := requestMatcher.(string); ok {
		matcher = func(s string) bool { return s == m }
	} else if m, ok := requestMatcher.(func(string) bool); ok {
		matcher = m
	} else {
		panic(fmt.Sprintf("cleattest: unsupported requestMatcher type %T", requestMatcher))
	}
	return &CallStubBuilder{
		env:       e,
		service:   service,
		operation: operation,
		matcher:   matcher,
	}
}

// AfterSignal schedules a signal to be delivered after the given delay
// elapses (from the current simulated time).
func (e *TestEnv) AfterSignal(delay time.Duration, name, payload string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	deliverAt := time.UnixMilli(e.nowMs).Add(delay)
	e.pendingSignals = append(e.pendingSignals, scheduledSignal{
		name:    name,
		payload: payload,
		time:    deliverAt,
	})
}

// Signal delivers a signal immediately (at the current simulated time).
func (e *TestEnv) Signal(name, payload string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sig := scheduledSignal{
		name:    name,
		payload: payload,
		time:    time.UnixMilli(e.nowMs),
	}
	e.pendingSignals = append(e.pendingSignals, sig)
	e.deliverSignals()
}

// AdvanceTime advances the simulated clock by the given duration.
// It wakes any sleeping goroutines whose sleep duration has elapsed
// and delivers any signals that become due.
func (e *TestEnv) AdvanceTime(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.nowMs += d.Milliseconds()
	nowTime := time.UnixMilli(e.nowMs)

	// Wake sleepers whose wakeAt <= now
	var remainingRecs []sleepRecord
	for _, s := range e.sleepRecs {
		if !s.wakeAt.After(nowTime) {
			close(s.wake)
		} else {
			remainingRecs = append(remainingRecs, s)
		}
	}
	e.sleepRecs = remainingRecs

	// Deliver signals that become due or whose waiters have timed out
	e.deliverSignals()
}

// SetTime sets the simulated clock to the given time.
func (e *TestEnv) SetTime(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nowMs = t.UnixMilli()
}

// Now returns the current simulated wall-clock time.
func (e *TestEnv) Now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.UnixMilli(e.nowMs)
}

// SetVersion sets the value returned by H().Version().
func (e *TestEnv) SetVersion(v int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.versionVal = v
}

// SetMinVersion sets the value returned by H().MinVersion().
func (e *TestEnv) SetMinVersion(v int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.minVersionVal = v
}

// SetRandomSeq configures the sequence of values returned by H().Random().
// After the sequence is exhausted, H().Random() returns 0.
func (e *TestEnv) SetRandomSeq(seq []int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.randomSeq = make([]int64, len(seq))
	copy(e.randomSeq, seq)
	e.randomIdx = 0
}

// QueryState reads back a value previously set via H().SetQueryState.
func (e *TestEnv) QueryState(key string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	val, ok := e.queryState[key]
	return val, ok
}

// AssertCalled fails the test if no call to the given service+operation
// appears in the call history.
func (e *TestEnv) AssertCalled(t TestingT, service, operation string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range e.callHistory {
		if rec.Service == service && rec.Operation == operation {
			return
		}
	}
	t.Fatalf("cleattest: expected call to %s.%s was not made", service, operation)
}

// AssertNotCalled fails the test if a call to the given service+operation
// appears in the call history.
func (e *TestEnv) AssertNotCalled(t TestingT, service, operation string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range e.callHistory {
		if rec.Service == service && rec.Operation == operation {
			t.Fatalf("cleattest: unexpected call to %s.%s was made (request: %s)", service, operation, rec.Request)
			return
		}
	}
}

// CallHistory returns a copy of all recorded calls.
func (e *TestEnv) CallHistory() []CallRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]CallRecord, len(e.callHistory))
	copy(result, e.callHistory)
	return result
}

// Reset clears all state: call history, stubs, signals, version, time,
// query state, random sequence, and sleepers.
func (e *TestEnv) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Wake any blocked goroutines
	for _, s := range e.sleepRecs {
		close(s.wake)
	}
	for _, w := range e.signalWaiters {
		w.ch <- scheduledSignal{} // empty signal signals timeout
	}

	e.nowMs = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	e.versionVal = 1
	e.minVersionVal = 1
	e.queryState = make(map[string]string)
	e.callHistory = nil
	e.callStubs = nil
	e.pendingSignals = nil
	e.sleepRecs = nil
	e.signalWaiters = nil
	e.randomSeq = nil
	e.randomIdx = 0
	e.deferCounter = 0
	e.childWorkflowStubs = make(map[string]*childWorkflowStub)
	e.childResults = make(map[string]*childStubResult)
	e.childWorkflowHandlers = make(map[string]func(inputJSON string) (resultJSON string, err error))
	e.retrySimCount = 0
	e.retrySimAttempts = nil
	e.retryBehaviors = make(map[string]*retryBehavior)
	e.childWorkflowCallHistory = nil
	e.ConcurrencyKeys = make(map[string]string)
	e.pluginCallStubs = nil
		e.signalReplyChannels = make(map[string]chan string)
}

// ---------------------------------------------------------------------------
// Internal HostCalls function implementations
// ---------------------------------------------------------------------------

func (e *TestEnv) durableCallImpl(service, operation, requestJSON string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec := CallRecord{
		Service:    service,
		Operation:  operation,
		Request:    requestJSON,
		RetryCount: 0,
	}

	// Retry simulation: first check per-service/operation retry behaviors,
	// then fall back to the global WithRetrySimulation.
	key := service + "/" + operation
	if rb, ok := e.retryBehaviors[key]; ok && rb.failCount > 0 {
		if rb.attempts < rb.failCount {
			rb.attempts++
			err := fmt.Errorf("cleattest: simulated transient failure for %s.%s (attempt %d/%d)", service, operation, rb.attempts, rb.failCount)
			rec.Err = err
			rec.RetryCount = rb.attempts
			e.callHistory = append(e.callHistory, rec)
			return "", err
		}
		// Success on final attempt — track the retry count but proceed to stub.
		rec.RetryCount = rb.attempts
	} else if e.retrySimCount > 0 {
		if e.retrySimAttempts == nil {
			e.retrySimAttempts = make(map[string]int)
		}
		attempt := e.retrySimAttempts[key]
		if attempt < e.retrySimCount {
			e.retrySimAttempts[key] = attempt + 1
			err := fmt.Errorf("cleattest: simulated transient failure for %s.%s (attempt %d/%d)", service, operation, attempt+1, e.retrySimCount)
			rec.Err = err
			rec.RetryCount = attempt + 1
			e.callHistory = append(e.callHistory, rec)
			return "", err
		}
		rec.RetryCount = attempt
	}

	// Find the first matching stub and consume it.
	for i, stub := range e.callStubs {
		if stub.service == service && stub.operation == operation && stub.matcher(requestJSON) {
			e.callStubs = append(e.callStubs[:i], e.callStubs[i+1:]...)
			rec.Response = stub.response
			if stub.err != nil {
				rec.Err = stub.err
			}
			e.callHistory = append(e.callHistory, rec)
			return stub.response, stub.err
		}
	}

	// No matching stub.
	err := fmt.Errorf("cleattest: no stub registered for %s.%s (request: %q)", service, operation, requestJSON)
	rec.Err = err
	e.callHistory = append(e.callHistory, rec)
	return "", err
}

func (e *TestEnv) durableCallWithOptionsImpl(_ cleat.CallOptions, service, operation, requestJSON string) (string, error) {
	return e.durableCallImpl(service, operation, requestJSON)
}

func (e *TestEnv) durableSleepImpl(ms int64) {
	e.mu.Lock()
	nowTime := time.UnixMilli(e.nowMs)
	wakeAt := nowTime.Add(time.Duration(ms) * time.Millisecond)
	wake := make(chan struct{})
	e.sleepRecs = append(e.sleepRecs, sleepRecord{wakeAt: wakeAt, wake: wake})
	e.mu.Unlock()

	<-wake
}

func (e *TestEnv) durableAwaitSignalsImpl(signalNames []string, timeoutMs int64) (string, string, bool, error) {
	e.mu.Lock()

	nowTime := time.UnixMilli(e.nowMs)
	deadline := nowTime.Add(time.Duration(timeoutMs) * time.Millisecond)

	// Check for matching due signals.
	for i, sig := range e.pendingSignals {
		if !sig.time.After(nowTime) && matchesAny(sig.name, signalNames) {
			e.pendingSignals = append(e.pendingSignals[:i], e.pendingSignals[i+1:]...)
			e.mu.Unlock()
			return sig.name, sig.payload, false, nil
		}
	}

	// Zero timeout means "poll only".
	if timeoutMs <= 0 {
		e.mu.Unlock()
		return "", "", true, nil
	}

	// No matching signal yet -- register a waiter.
	ch := make(chan scheduledSignal, 1)
	e.signalWaiters = append(e.signalWaiters, signalWaiter{
		names:    signalNames,
		deadline: deadline,
		ch:       ch,
	})
	e.mu.Unlock()

	sig := <-ch
	if sig.name == "" {
		return "", "", true, nil // timeout sentinel
	}
	return sig.name, sig.payload, false, nil
}

func (e *TestEnv) durableDeferImpl(description string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deferCounter++
	return fmt.Sprintf("defer-%d", e.deferCounter), nil
}

func (e *TestEnv) durableLogImpl(message string) {
	// Best-effort; recorded in the event history in real runs.
}

func (e *TestEnv) pollCancellationImpl() (bool, string) {
	return false, ""
}

func (e *TestEnv) pollSignalImpl(signalName string) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	nowTime := time.UnixMilli(e.nowMs)
	for i, sig := range e.pendingSignals {
		if sig.name == signalName && !sig.time.After(nowTime) {
			e.pendingSignals = append(e.pendingSignals[:i], e.pendingSignals[i+1:]...)
			return sig.payload, true, nil
		}
	}
	return "", false, nil
}

func (e *TestEnv) continueAsNewImpl(newInputJSON string) error {
	// No-op for testing; the workflow code called ContinueAsNew but we
	// simply accept it rather than restarting.
	return nil
}

// RegisterChildWorkflow registers a handler function for a child workflow with the
// given name. The handler receives the input JSON and returns the result JSON and an
// error. This takes priority over stub results set via OnChildWorkflow.
func (e *TestEnv) RegisterChildWorkflow(name string, handler func(inputJSON string) (resultJSON string, err error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.childWorkflowHandlers[name] = handler
}

// OnChildWorkflow registers a stub builder for a child workflow with the given name.
func (e *TestEnv) OnChildWorkflow(name string) *ChildWorkflowStubBuilder {
	return &ChildWorkflowStubBuilder{
		env:  e,
		name: name,
	}
}

func (e *TestEnv) childWorkflowImpl(name, inputJSON string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Record the child workflow invocation.
	childRec := ChildWorkflowCallRecord{
		Name:      name,
		InputJSON: inputJSON,
	}

	// Check for a registered handler first.
	if handler, ok := e.childWorkflowHandlers[name]; ok {
		e.deferCounter++
		runID := fmt.Sprintf("child-%s-%d", name, e.deferCounter)
		result, err := handler(inputJSON)
		e.childResults[runID] = &childStubResult{result: result, err: err}
		childRec.RunID = runID
		if err != nil {
			childRec.Err = err
		}
		e.childWorkflowCallHistory = append(e.childWorkflowCallHistory, childRec)
		return runID, nil
	}

	e.deferCounter++
	runID := fmt.Sprintf("child-%s-%d", name, e.deferCounter)
	if stub, ok := e.childWorkflowStubs[name]; ok {
		e.childResults[runID] = &childStubResult{result: stub.result, err: stub.err}
		childRec.RunID = runID
		if stub.err != nil {
			childRec.Err = stub.err
		}
		e.childWorkflowCallHistory = append(e.childWorkflowCallHistory, childRec)
	} else {
		e.childResults[runID] = &childStubResult{result: `{"status":"completed"}`, err: nil}
		childRec.RunID = runID
		e.childWorkflowCallHistory = append(e.childWorkflowCallHistory, childRec)
	}
	return runID, nil
}

func (e *TestEnv) awaitChildImpl(runID string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if result, ok := e.childResults[runID]; ok {
		if result.err != nil {
			return "", result.err
		}
		return result.result, nil
	}
	return `{"status":"completed"}`, nil
}

func (e *TestEnv) awaitAllChildrenImpl(runIDs []string) ([]cleat.ChildResult, error) {
	results := make([]cleat.ChildResult, len(runIDs))
	for i, runID := range runIDs {
		result, err := e.awaitChildImpl(runID)
		if err != nil {
			results[i] = cleat.ChildResult{RunID: runID, Error: err.Error()}
		} else {
			results[i] = cleat.ChildResult{RunID: runID, Result: result}
		}
	}
	return results, nil
}

func (e *TestEnv) childWorkflowTypedImpl(name string, request interface{}) (string, error) {
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("cleattest: marshaling child workflow input for %s: %w", name, err)
	}
	return e.childWorkflowImpl(name, string(reqJSON))
}

func (e *TestEnv) awaitChildTypedImpl(runID string, result interface{}) error {
	resp, err := e.awaitChildImpl(runID)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(resp), result)
}

func (e *TestEnv) durableCallTypedWithHeartbeatImpl(service, operation string, request, result interface{}, heartbeatInterval time.Duration, onProgress func(string)) error {
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("cleattest: marshaling request for %s.%s: %w", service, operation, err)
	}
	resp, err := e.durableCallImpl(service, operation, string(reqJSON))
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal([]byte(resp), result)
}

func (e *TestEnv) versionImpl() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.versionVal
}

func (e *TestEnv) minVersionImpl() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.minVersionVal
}

func (e *TestEnv) setQueryStateImpl(key, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queryState[key] = value
}

func (e *TestEnv) nowImpl() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nowMs
}

func (e *TestEnv) randomImpl() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.randomIdx < len(e.randomSeq) {
		val := e.randomSeq[e.randomIdx]
		e.randomIdx++
		return val
	}
	return 0
}

func (e *TestEnv) createPromiseImpl(name string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deferCounter++
	promiseID := fmt.Sprintf("prom-%s-%d", name, e.deferCounter)
	e.promises[promiseID] = promiseState{
		name:   name,
		status: "pending",
	}
	return promiseID, nil
}

func (e *TestEnv) registerUpdateHandlerImpl(name string) {
	// No-op for testing. The SDK layer stores the handler+validator in a map.
}

func (e *TestEnv) registerQueryHandlerImpl(name string) {
	// No-op for testing. The hostCallsImpl stores the handler in its
	// queryHandlers map; HandleQuery falls back to that map when
	// the host-provided handler function is nil.
}


// HandleQuery invokes a registered query handler by name with the given payload.
// If the underlying HostCalls supports it, the host-provided handler is used.
// Otherwise, falls back to the local queryHandlers map in hostCallsImpl.
func (e *TestEnv) HandleQuery(name, payload string) (string, error) {
	// The h field is a *hostCallsImpl which has a HandleQuery method.
	// Type-assert to access it; if the assertion fails, try a simpler fallback.
	if h, ok := e.h.(interface{ HandleQuery(string, string) (string, error) }); ok {
		return h.HandleQuery(name, payload)
	}
	return "", fmt.Errorf("cleattest: HandleQuery not available")
}

// HandleUpdate invokes a registered update handler by name with the given payload.
func (e *TestEnv) HandleUpdate(name, payload string) (string, error) {
	if h, ok := e.h.(interface{ HandleUpdate(string, string) (string, error) }); ok {
		return h.HandleUpdate(name, payload)
	}
	return "", fmt.Errorf("cleattest: HandleUpdate not available")
}

func (e *TestEnv) runDetachedImpl(fn func(h cleat.HostCalls) error) error {
	// Run the function directly. In test mode there is no cancellation anyway.
	return fn(e.h)
}

func (e *TestEnv) awaitPromiseImpl(promiseID string, timeout time.Duration) (string, bool, error) {
	e.mu.Lock()
	ps, ok := e.promises[promiseID]
	e.mu.Unlock()

	if !ok {
		return "", false, fmt.Errorf("cleattest: promise %s not found", promiseID)
	}

	if ps.status == "resolved" {
		return ps.result, false, nil
	}
	if ps.status == "rejected" {
		return ps.errorMsg, false, fmt.Errorf("promise rejected: %s", ps.errorMsg)
	}

	// Pending -- advance time to simulate timeout.
	e.mu.Lock()
	e.nowMs += timeout.Milliseconds()
	e.mu.Unlock()

	return "", true, nil
}

func (e *TestEnv) pluginCallImpl(pluginName, functionName, inputJSON string) (string, error) {
	for _, stub := range e.pluginCallStubs {
		if stub.pluginName == pluginName && stub.functionName == functionName {
			return stub.result, stub.err
		}
	}
	return "", fmt.Errorf("cleattest: no stub registered for PluginCall(%q, %q)", pluginName, functionName)
}

// sendSignalAndWaitImpl sends a signal and registers a reply channel.
func (e *TestEnv) sendSignalAndWaitImpl(targetRunID, signalName, payload string, timeout time.Duration) (string, error) {
	e.mu.Lock()

	// Generate a correlation ID and embed it in the payload.
	correlationID := fmt.Sprintf("corr-%s-%s-%d", targetRunID, signalName, e.deferCounter)
	e.deferCounter++

	// Register a reply channel.
	replyCh := make(chan string, 1)
	e.signalReplyChannels[correlationID] = replyCh

	// Create the enriched payload with correlation ID.
	enrichedPayload := payload
	if payload != "" && payload != "{}" {
		// Try to merge correlation ID into the existing JSON payload.
		var payloadMap map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &payloadMap); err == nil {
			payloadMap["_correlation_id"] = correlationID
			if data, err := json.Marshal(payloadMap); err == nil {
				enrichedPayload = string(data)
			}
		}
	} else {
		enrichedPayload = fmt.Sprintf(`{"_correlation_id":%q}`, correlationID)
	}

	e.mu.Unlock()

	// Send the signal.
	err := e.H().SignalWorkflow(targetRunID, signalName, enrichedPayload)
	if err != nil {
		return "", err
	}

	// Wait for the reply with a timeout.
	select {
	case response := <-replyCh:
		return response, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("cleattest: SendSignalAndWait timed out after %v", timeout)
	}
}

// replyToSignalImpl sends a response back via the correlation ID.
func (e *TestEnv) replyToSignalImpl(correlationID, response string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.signalReplyChannels[correlationID]
	if !ok {
		return fmt.Errorf("cleattest: no pending signal for correlation ID %q", correlationID)
	}
	delete(e.signalReplyChannels, correlationID)
	ch <- response
	return nil
}


// signalWorkflowImpl delivers a signal to a target workflow.
// In the test env, the target workflow is the current workflow itself.
func (e *TestEnv) signalWorkflowImpl(targetRunID, signalName, payload string) error {
	e.Signal(signalName, payload)
	return nil
}


// ResolvePromise resolves a promise with the given result.
func (e *TestEnv) ResolvePromise(promiseID, result string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ps, ok := e.promises[promiseID]; ok {
		ps.status = "resolved"
		ps.result = result
		e.promises[promiseID] = ps
	}
}

// RejectPromise rejects a promise with the given error message.
func (e *TestEnv) RejectPromise(promiseID, errMsg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ps, ok := e.promises[promiseID]; ok {
		ps.status = "rejected"
		ps.errorMsg = errMsg
		e.promises[promiseID] = ps
	}
}

// ---------------------------------------------------------------------------
// Concurrency Key methods
// ---------------------------------------------------------------------------

// AcquireConcurrencyKey attempts to acquire a concurrency key for a workflow.
// Uses the mock function if set, otherwise uses the default in-memory map behavior.
func (e *TestEnv) acquireLockImpl(key string, ttlMs int64) (bool, error) {
	return e.AcquireConcurrencyKey(key, "")
}

func (e *TestEnv) releaseLockImpl(key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.ConcurrencyKeys, key)
	return nil
}

func (e *TestEnv) awaitConditionImpl(predicate func() bool, pollInterval, timeout time.Duration) (bool, error) {
	deadline := e.Now().Add(timeout)
	for {
		if predicate() {
			return true, nil
		}
		if e.Now().After(deadline) {
			return false, nil
		}
		e.durableSleepImpl(pollInterval.Milliseconds())
	}
}
	func (e *TestEnv) sideEffectImpl(computedResult string) (string, error) {
	// In tests, there's no replay, so computedResult IS authoritative.
	return computedResult, nil
}

func (e *TestEnv) AcquireConcurrencyKey(key, workflowID string) (bool, error) {
	if e.AcquireConcurrencyKeyFn != nil {
		return e.AcquireConcurrencyKeyFn(key, workflowID)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if existingWFID, ok := e.ConcurrencyKeys[key]; ok {
		if existingWFID == workflowID {
			return true, nil
		}
		return false, nil
	}
	e.ConcurrencyKeys[key] = workflowID
	return true, nil
}

// ReleaseConcurrencyKeys releases all concurrency keys held by a workflow.
// Uses the mock function if set, otherwise uses the default in-memory map behavior.
func (e *TestEnv) ReleaseConcurrencyKeys(workflowID string) {
	if e.ReleaseConcurrencyKeysFn != nil {
		e.ReleaseConcurrencyKeysFn(workflowID)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range e.ConcurrencyKeys {
		if v == workflowID {
			delete(e.ConcurrencyKeys, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// deliverSignals checks pending signals against registered waiters.
// Must be called with e.mu held.
func (e *TestEnv) deliverSignals() {
	nowTime := time.UnixMilli(e.nowMs)

	// Remove timed-out and delivered waiters.
	var remaining []signalWaiter
	for _, w := range e.signalWaiters {
		// Check for a matching signal that is now due.
		delivered := false
		for i, sig := range e.pendingSignals {
			if !sig.time.After(nowTime) && matchesAny(sig.name, w.names) {
				e.pendingSignals = append(e.pendingSignals[:i], e.pendingSignals[i+1:]...)
				w.ch <- sig
				delivered = true
				break
			}
		}
		if delivered {
			continue
		}

		// Check for timeout.
		if !w.deadline.After(nowTime) {
			w.ch <- scheduledSignal{} // empty sentinel = timeout
			continue
		}

		remaining = append(remaining, w)
	}
	e.signalWaiters = remaining
}

// matchesAny reports whether name is in the list.
func matchesAny(name string, names []string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
