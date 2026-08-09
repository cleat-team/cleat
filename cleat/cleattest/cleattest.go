// Package cleattest provides a mock HostCalls implementation for testing
// workflows without compiling to WASM or running a full engine.
//
// NOTE: This test SDK package intentionally uses Go features (channels,
// timers) that may be flagged by `cleat vet`. These are safe here because
// this is test infrastructure, not user workflow code.
package cleattest

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/engine"
)

// TestingT is the interface required by AssertCalled and AssertNotCalled.
// *testing.T satisfies this interface.
type TestingT interface {
	Fatalf(format string, args ...interface{})
}

// CallRecord records a single durable API call made through the test env.
type CallRecord struct {
	Service    string
	Operation  string
	Request    string
	Response   string
	Err        error
	RetryCount int // Number of retry attempts before this call succeeded (0 if no retry)
}

// RecordedCall captures a single HostCalls invocation for replay testing.
type RecordedCall struct {
	Function string // Name of the HostCalls function
	Args     string // Serialized arguments key for matching
	Response string // Serialized response
	Err      error  // Error returned, if any
}

// Clock is implemented by *simClock and provides a simulated After timer.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

// simClock uses the TestEnv's simulated time to fire After channels.
type simClock struct {
	env *TestEnv
}

func (c *simClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.env.mu.Lock()
	nowTime := time.UnixMilli(c.env.nowMs)
	wakeAt := nowTime.Add(d)
	wake := make(chan struct{})
	c.env.sleepRecs = append(c.env.sleepRecs, sleepRecord{wakeAt: wakeAt, wake: wake})
	c.env.mu.Unlock()

	go func() {
		<-wake
		ch <- time.Now()
	}()
	return ch
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

	// crons holds schedules created by ScheduleCron, keyed by schedule ID.
	// cronCounter makes those IDs deterministic so a test can assert on
	// them; the engine derives its own from (tenant, workflow, step), which
	// a harness with no steps and no tenant cannot reproduce.
	crons       map[string]cleat.CronSchedule
	cronCounter int

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

	ConcurrencyKeys          map[string]string
	AcquireConcurrencyKeyFn  func(key, workflowID string) (bool, error)
	ReleaseConcurrencyKeysFn func(workflowID string)

	pluginCallStubs []*pluginCallStub

	// cancellation tracking for SetCancelled / PollCancellation tests.
	cancelled    bool
	cancelReason string

	// signalReplyChannels maps correlation IDs to reply channels for
	// SendSignalAndWait / ReplyToSignal.
	signalReplyChannels map[string]chan string

	// replayMode enables call recording and replay.
	replayMode bool
	// replayHistory stores recorded calls for replay matching.
	replayHistory []RecordedCall
	// replayDivergence counts calls during replay not found in history.
	replayDivergence int
	// replayRecording is true during the recording phase (set by EnableReplay,
	// cleared by StartReplay). During recording, calls are recorded but never
	// checked against history or counted as divergent.
	replayRecording bool

	// ContinueAsNew tracking.
	continued        bool
	continuedInput   string
	continuedVersion int64

	// clock provides simulated timeouts.
	clock Clock
}

// NewTestEnv creates a new TestEnv with a clean initial state.
// The simulated clock starts at 2024-01-01T00:00:00Z.
// Optional TestEnvOption arguments can be passed to configure behavior
// (e.g., WithRetrySimulation).
func NewTestEnv(opts ...TestEnvOption) *TestEnv {
	e := &TestEnv{
		nowMs:                    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		versionVal:               1,
		minVersionVal:            1,
		queryState:               make(map[string]string),
		promises:                 make(map[string]promiseState),
		crons:                    make(map[string]cleat.CronSchedule),
		childWorkflowStubs:       make(map[string]*childWorkflowStub),
		childResults:             make(map[string]*childStubResult),
		childWorkflowHandlers:    make(map[string]func(inputJSON string) (resultJSON string, err error)),
		retryBehaviors:           make(map[string]*retryBehavior),
		childWorkflowCallHistory: make([]ChildWorkflowCallRecord, 0),
		ConcurrencyKeys:          make(map[string]string),
		signalReplyChannels:      make(map[string]chan string),
	}
	for _, opt := range opts {
		opt(e)
	}
	e.clock = &simClock{env: e}
	e.h = cleat.NewHostCalls(e.hostCallsOptions())
	return e
}

// hostCallsOptions is the harness's implementation of every host call.
//
// It is a method rather than a literal inlined into NewTestEnv so that
// TestEveryHostCallIsWired can reflect over what it returns. A field left out
// of this struct leaves the corresponding hook nil, and cleat/runtime_*.go
// answers a nil hook with "can only be called from within a workflow function
// (the HostCalls runtime was not initialized)" -- a message about workflow
// context that says nothing about the real cause. AwaitAnyChild and PollChild
// were missing here from the day they were added to HostCallsOptions.
func (e *TestEnv) hostCallsOptions() cleat.HostCallsOptions {
	return cleat.HostCallsOptions{
		DurableCall:                   e.durableCallImpl,
		DurableCallWithOptions:        e.durableCallWithOptionsImpl,
		DurableSleep:                  e.durableSleepImpl,
		DurableAwaitSignals:           e.durableAwaitSignalsImpl,
		DurableDefer:                  e.durableDeferImpl,
		DurableLog:                    e.durableLogImpl,
		PollCancellation:              e.pollCancellationImpl,
		PollSignal:                    e.pollSignalImpl,
		ContinueAsNew:                 e.continueAsNewImpl,
		ContinueAsNewWithVersion:      e.continueAsNewWithVersionImpl,
		ResolvePromise:                e.resolvePromiseImpl,
		RejectPromise:                 e.rejectPromiseImpl,
		ChildWorkflow:                 e.childWorkflowImpl,
		AwaitChild:                    e.awaitChildImpl,
		AwaitAnyChild:                 e.awaitAnyChildImpl,
		AwaitAllChildren:              e.awaitAllChildrenImpl,
		PollChild:                     e.pollChildImpl,
		ChildWorkflowTyped:            e.childWorkflowTypedImpl,
		AwaitChildTyped:               e.awaitChildTypedImpl,
		DurableCallTypedWithHeartbeat: e.durableCallTypedWithHeartbeatImpl,
		Version:                       e.versionImpl,
		MinVersion:                    e.minVersionImpl,
		SetQueryState:                 e.setQueryStateImpl,
		Now:                           e.nowImpl,
		Random:                        e.randomImpl,
		CreatePromise:                 e.createPromiseImpl,
		ScheduleCron:                  e.scheduleCronImpl,
		DeleteCron:                    e.deleteCronImpl,
		ListCrons:                     e.listCronsImpl,
		AwaitPromise:                  e.awaitPromiseImpl,
		RegisterUpdateHandler:         e.registerUpdateHandlerImpl,
		RunDetached:                   e.runDetachedImpl,
		PluginCall:                    e.pluginCallImpl,
		DurableSend:                   e.durableSendImpl,
		ScheduleInvoke:                e.durableScheduleInvokeImpl,
		SendSignalAndWait:             e.sendSignalAndWaitImpl,
		ReplyToSignal:                 e.replyToSignalImpl,
		SignalWorkflow:                e.signalWorkflowImpl,
		AcquireLock:                   e.acquireLockImpl,
		ReleaseLock:                   e.releaseLockImpl,
		AwaitCondition:                e.awaitConditionImpl,
		SideEffect:                    e.sideEffectImpl,
	}
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
// After delivering, it yields to the scheduler so that any workflow goroutine
// blocked on AwaitSignals can process the signal before the caller continues.
func (e *TestEnv) Signal(name, payload string) {
	e.mu.Lock()
	sig := scheduledSignal{
		name:    name,
		payload: payload,
		time:    time.UnixMilli(e.nowMs),
	}
	e.pendingSignals = append(e.pendingSignals, sig)
	e.deliverSignals()
	e.mu.Unlock()
	runtime.Gosched()
}

// SetCancelled configures the test environment to report cancellation.
// After calling this, PollCancellation will return (true, reason).
// This enables testing workflow cancellation behavior.
func (e *TestEnv) SetCancelled(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelled = true
	e.cancelReason = reason
}

// ClearCancelled resets the cancellation state.
func (e *TestEnv) ClearCancelled() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancelled = false
	e.cancelReason = ""
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

// AdvanceTimeAndDrain advances the clock and best-effort blocks until
// pending goroutines (DurableSleep, AwaitSignals waiters) have been
// serviced. It spins with runtime.Gosched for up to 100 iterations or
// until no sleepers/waiters remain.
//
// This is best-effort: in edge cases where a workflow immediately
// re-enters a sleep or signal loop, the count may never reach zero,
// and the function returns after the spin limit. Tests that need
// deterministic drain should use a sync.WaitGroup or other explicit
// synchronization.
func (e *TestEnv) AdvanceTimeAndDrain(d time.Duration) {
	e.AdvanceTime(d)
	for i := 0; i < 100; i++ {
		runtime.Gosched()
		e.mu.Lock()
		pending := len(e.sleepRecs) + len(e.signalWaiters)
		e.mu.Unlock()
		if pending == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
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
	e.crons = make(map[string]cleat.CronSchedule)
	e.cronCounter = 0
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
	e.replayMode = false
	e.replayHistory = nil
	e.replayDivergence = 0
	e.replayRecording = false
	e.continued = false
	e.continuedInput = ""
}

// EnableReplay enables replay recording mode.
// When enabled, all HostCalls are recorded into the replay history.
// On subsequent calls matching the same function+args key, the cached
// response is returned instead of executing fresh.
func (e *TestEnv) EnableReplay() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replayMode = true
	e.replayRecording = true
}

// StartReplay switches from recording mode to replay mode.
// After calling this, HostCalls will check the replay history and return
// cached responses. Calls not found in history will increment replayDivergence.
func (e *TestEnv) StartReplay() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replayRecording = false
	e.replayDivergence = 0
}

// AssertReplayDivergence fails the test if the actual number of divergent
// calls (calls during replay that were not found in history) does not match
// expectedDivergence.
func (e *TestEnv) AssertReplayDivergence(t TestingT, expectedDivergence int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.replayDivergence != expectedDivergence {
		t.Fatalf("cleattest: expected replay divergence count %d, got %d", expectedDivergence, e.replayDivergence)
	}
}

// AssertContinued fails the test if ContinueAsNew was not called, or if its
// input does not match expectedInput.
func (e *TestEnv) AssertContinued(t TestingT, expectedInput string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.continued {
		t.Fatalf("cleattest: expected ContinueAsNew to have been called")
	}
	if e.continuedInput != expectedInput {
		t.Fatalf("cleattest: expected ContinueAsNew input %q, got %q", expectedInput, e.continuedInput)
	}
}

// LastContinuedInput returns the input passed to the last ContinueAsNew call.
// Returns empty string if ContinueAsNew was never called.
func (e *TestEnv) LastContinuedInput() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.continuedInput
}

// LastContinuedVersion returns the version passed to the most recent
// ContinueAsNewWithVersion, or 0 if the workflow used plain ContinueAsNew.
//
// 0 is the engine's own "keep the current version" sentinel (see
// engine/lifecycle.go), so it means "no version was pinned" rather than
// "version zero" -- and it is also the zero value here, which makes the two
// indistinguishable. Pair it with AssertContinued when that matters.
func (e *TestEnv) LastContinuedVersion() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.continuedVersion
}

// ---------------------------------------------------------------------------
// Internal HostCalls function implementations
// ---------------------------------------------------------------------------

func (e *TestEnv) durableCallImpl(service, operation, requestJSON string) (resp string, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "DurableCall|" + service + "|" + operation + "|" + requestJSON
	if cachedResp, cachedErr, matched := e.replayLookup("DurableCall", replayKey); matched {
		return cachedResp, cachedErr
	}
	defer func() { e.replayRecord("DurableCall", replayKey, resp, retErr) }()

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
			resp = ""
			retErr = err
			return
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
			resp = ""
			retErr = err
			return
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
			resp = stub.response
			retErr = stub.err
			return
		}
	}

	// No matching stub.
	err := fmt.Errorf("cleattest: no stub registered for %s.%s (request: %q)", service, operation, requestJSON)
	rec.Err = err
	e.callHistory = append(e.callHistory, rec)
	resp = ""
	retErr = err
	return
}

func (e *TestEnv) durableCallWithOptionsImpl(_ cleat.CallOptions, service, operation, requestJSON string) (string, error) {
	return e.durableCallImpl(service, operation, requestJSON)
}

func (e *TestEnv) durableSleepImpl(ms int64) {
	e.mu.Lock()

	replayKey := fmt.Sprintf("DurableSleep|%d", ms)
	if _, _, matched := e.replayLookup("DurableSleep", replayKey); matched {
		e.mu.Unlock()
		return
	}

	nowTime := time.UnixMilli(e.nowMs)
	wakeAt := nowTime.Add(time.Duration(ms) * time.Millisecond)
	wake := make(chan struct{})
	e.sleepRecs = append(e.sleepRecs, sleepRecord{wakeAt: wakeAt, wake: wake})
	e.replayRecord("DurableSleep", replayKey, "", nil)
	e.mu.Unlock()

	<-wake
}

func (e *TestEnv) durableAwaitSignalsImpl(signalNames []string, timeoutMs int64) (signalName string, payload string, timedOut bool, retErr error) {
	e.mu.Lock()

	replayKey := fmt.Sprintf("DurableAwaitSignals|%v|%d", signalNames, timeoutMs)
	if cachedResp, cachedErr, matched := e.replayLookup("DurableAwaitSignals", replayKey); matched {
		var resp struct {
			Name     string `json:"name"`
			Payload  string `json:"payload"`
			TimedOut bool   `json:"timedOut"`
		}
		json.Unmarshal([]byte(cachedResp), &resp)
		signalName = resp.Name
		payload = resp.Payload
		timedOut = resp.TimedOut
		retErr = cachedErr
		e.mu.Unlock()
		return
	}

	nowTime := time.UnixMilli(e.nowMs)
	deadline := nowTime.Add(time.Duration(timeoutMs) * time.Millisecond)

	// Check for matching due signals.
	for i, sig := range e.pendingSignals {
		if !sig.time.After(nowTime) && matchesAny(sig.name, signalNames) {
			e.pendingSignals = append(e.pendingSignals[:i], e.pendingSignals[i+1:]...)
			signalName = sig.name
			payload = sig.payload
			timedOut = false
			retErr = nil
			respData, _ := json.Marshal(struct {
				Name     string `json:"name"`
				Payload  string `json:"payload"`
				TimedOut bool   `json:"timedOut"`
			}{signalName, payload, timedOut})
			e.replayRecord("DurableAwaitSignals", replayKey, string(respData), retErr)
			e.mu.Unlock()
			return
		}
	}

	// Zero timeout means "poll only".
	if timeoutMs <= 0 {
		signalName = ""
		payload = ""
		timedOut = true
		retErr = nil
		respData, _ := json.Marshal(struct {
			Name     string `json:"name"`
			Payload  string `json:"payload"`
			TimedOut bool   `json:"timedOut"`
		}{signalName, payload, timedOut})
		e.replayRecord("DurableAwaitSignals", replayKey, string(respData), retErr)
		e.mu.Unlock()
		return
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
		signalName = ""
		payload = ""
		timedOut = true
		retErr = nil
		// Note: recording skipped here (lock not held); matches original behavior.
		return
	}
	signalName = sig.name
	payload = sig.payload
	timedOut = false
	retErr = nil
	return
}

func (e *TestEnv) durableDeferImpl(description string) (resp string, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "DurableDefer|" + description
	if cachedResp, cachedErr, matched := e.replayLookup("DurableDefer", replayKey); matched {
		return cachedResp, cachedErr
	}
	defer func() { e.replayRecord("DurableDefer", replayKey, resp, retErr) }()

	e.deferCounter++
	resp = fmt.Sprintf("defer-%d", e.deferCounter)
	return
}

func (e *TestEnv) durableLogImpl(message string) {
	_ = message // mark parameter as used for coverage
}

func (e *TestEnv) pollCancellationImpl() (cancelled bool, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "PollCancellation"
	if cachedResp, _, matched := e.replayLookup("PollCancellation", replayKey); matched {
		var resp struct {
			Cancelled bool   `json:"cancelled"`
			Reason    string `json:"reason"`
		}
		json.Unmarshal([]byte(cachedResp), &resp)
		return resp.Cancelled, resp.Reason
	}
	defer func() {
		respData, _ := json.Marshal(struct {
			Cancelled bool   `json:"cancelled"`
			Reason    string `json:"reason"`
		}{cancelled, reason})
		e.replayRecord("PollCancellation", replayKey, string(respData), nil)
	}()

	cancelled = e.cancelled
	reason = e.cancelReason
	return
}

func (e *TestEnv) pollSignalImpl(signalName string) (payload string, found bool, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "PollSignal|" + signalName
	if cachedResp, cachedErr, matched := e.replayLookup("PollSignal", replayKey); matched {
		var resp struct {
			Payload string `json:"payload"`
			Found   bool   `json:"found"`
		}
		json.Unmarshal([]byte(cachedResp), &resp)
		return resp.Payload, resp.Found, cachedErr
	}
	defer func() {
		respData, _ := json.Marshal(struct {
			Payload string `json:"payload"`
			Found   bool   `json:"found"`
		}{payload, found})
		e.replayRecord("PollSignal", replayKey, string(respData), retErr)
	}()

	nowTime := time.UnixMilli(e.nowMs)
	for i, sig := range e.pendingSignals {
		if sig.name == signalName && !sig.time.After(nowTime) {
			e.pendingSignals = append(e.pendingSignals[:i], e.pendingSignals[i+1:]...)
			return sig.payload, true, nil
		}
	}
	return "", false, nil
}

func (e *TestEnv) continueAsNewImpl(newInputJSON string) (retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "ContinueAsNew|" + newInputJSON
	if _, cachedErr, matched := e.replayLookup("ContinueAsNew", replayKey); matched {
		return cachedErr
	}
	defer func() { e.replayRecord("ContinueAsNew", replayKey, "", retErr) }()

	e.continued = true
	e.continuedInput = newInputJSON
	return
}

// continueAsNewWithVersionImpl is continueAsNewImpl plus the target version.
//
// engine/lifecycle.go records EventTypeContinueAsNew with NewInput and
// NewVersion and then suspends, and documents newVersion == 0 as "keep the
// current version". This harness has no version to keep, so it records what it
// was asked for verbatim and lets LastContinuedVersion report it -- a test
// asserting 0 is asserting "the workflow did not pin a version", which is the
// distinction the engine draws.
func (e *TestEnv) continueAsNewWithVersionImpl(newInputJSON string, newVersion int64) (retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "ContinueAsNewWithVersion|" + newInputJSON + "|" + strconv.FormatInt(newVersion, 10)
	if _, cachedErr, matched := e.replayLookup("ContinueAsNewWithVersion", replayKey); matched {
		return cachedErr
	}
	defer func() { e.replayRecord("ContinueAsNewWithVersion", replayKey, "", retErr) }()

	e.continued = true
	e.continuedInput = newInputJSON
	e.continuedVersion = newVersion
	return
}

// resolvePromiseImpl is the workflow-side counterpart of the public
// TestEnv.ResolvePromise driver method.
//
// It always returns nil, matching the engine: engine/promises.go records the
// event, calls the promise store, and LOGS rather than returns a store error --
// the host function's result is unconditionally success. A mock that surfaced
// an error here would let a test assert a failure mode production cannot
// produce. Resolving an unknown promise is likewise a silent no-op, because the
// store's UPDATE matches no rows and SQL does not call that an error.
func (e *TestEnv) resolvePromiseImpl(promiseID, value string) error {
	e.ResolvePromise(promiseID, value)
	return nil
}

// rejectPromiseImpl is the workflow-side counterpart of TestEnv.RejectPromise.
// Same contract as resolvePromiseImpl above, including the nil return.
func (e *TestEnv) rejectPromiseImpl(promiseID, errMsg string) error {
	e.RejectPromise(promiseID, errMsg)
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

func (e *TestEnv) childWorkflowImpl(name, inputJSON string) (resp string, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "ChildWorkflow|" + name + "|" + inputJSON
	if cachedResp, cachedErr, matched := e.replayLookup("ChildWorkflow", replayKey); matched {
		return cachedResp, cachedErr
	}
	defer func() { e.replayRecord("ChildWorkflow", replayKey, resp, retErr) }()

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
		resp = runID
		return
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
	resp = runID
	return
}

func (e *TestEnv) awaitChildImpl(runID string) (resp string, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "AwaitChild|" + runID
	if cachedResp, cachedErr, matched := e.replayLookup("AwaitChild", replayKey); matched {
		return cachedResp, cachedErr
	}
	defer func() { e.replayRecord("AwaitChild", replayKey, resp, retErr) }()

	if result, ok := e.childResults[runID]; ok {
		if result.err != nil {
			retErr = result.err
			return
		}
		resp = result.result
		return
	}
	resp = `{"status":"completed"}`
	return
}

// awaitAnyChildImpl mirrors engine/children.go's AwaitAnyChild: it polls the
// children in SORTED runID order and returns the first one with a result.
//
// The sort matches the engine deliberately. plugins/dag/dag.go builds its
// runIDs slice by ranging over a map, so the argument order is randomised on
// every call, and engine/children.go sorts before polling so that a replay
// after a suspend picks the same winner as the original execution. A harness
// that honoured argument order instead would disagree with the engine about
// which child wins.
//
// It is fidelity, not something the current suite pins -- measured, because
// the first version of this comment claimed otherwise. Disabling the sort and
// running examples/dag 20 times: 20/20 still pass. The DAG's outputs are keyed
// by task name and its levels respect dependencies either way, so the choice
// of winner does not change its result. TestAwaitAnyChild_PicksLowestSortedRunID
// in this package is what actually holds the behaviour in place.
//
// Every child in this harness has already run to completion by the time it is
// awaited -- childWorkflowImpl resolves it synchronously -- so "first
// completed" degenerates to "first in sorted order" and there is no suspend
// path to model. The per-child replay bookkeeping is awaitChildImpl's, which
// is why this delegates to it rather than reading e.childResults directly.
func (e *TestEnv) awaitAnyChildImpl(runIDs []string) (string, string, error) {
	if len(runIDs) == 0 {
		return "", "", fmt.Errorf("cleattest: AwaitAnyChild called with no run IDs")
	}

	sorted := append([]string(nil), runIDs...)
	sort.Strings(sorted)

	e.mu.Lock()
	winner := ""
	for _, runID := range sorted {
		if _, ok := e.childResults[runID]; ok {
			winner = runID
			break
		}
	}
	e.mu.Unlock()

	if winner == "" {
		// No recorded result for any of them. awaitChildImpl treats an unknown
		// runID as a completed child with a default result rather than
		// blocking, and this follows it: a harness that hung here would turn a
		// mis-wired test into a timeout instead of a failure.
		winner = sorted[0]
	}

	result, err := e.awaitChildImpl(winner)
	return winner, result, err
}

// pollChildImpl is the non-blocking counterpart. Children resolve synchronously
// here, so a known child is always "completed"; an unknown one is "running"
// rather than an error, which is what lets a poll loop in a workflow terminate.
func (e *TestEnv) pollChildImpl(runID string) (string, string, error) {
	e.mu.Lock()
	result, ok := e.childResults[runID]
	e.mu.Unlock()

	if !ok {
		return "running", "", nil
	}
	if result.err != nil {
		return "failed", "", result.err
	}
	return "completed", result.result, nil
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

func (e *TestEnv) versionImpl() (v int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "Version"
	if cachedResp, _, matched := e.replayLookup("Version", replayKey); matched {
		v, _ = strconv.Atoi(cachedResp)
		return
	}
	defer func() { e.replayRecord("Version", replayKey, strconv.Itoa(v), nil) }()

	v = e.versionVal
	return
}

func (e *TestEnv) minVersionImpl() (v int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "MinVersion"
	if cachedResp, _, matched := e.replayLookup("MinVersion", replayKey); matched {
		v, _ = strconv.Atoi(cachedResp)
		return
	}
	defer func() { e.replayRecord("MinVersion", replayKey, strconv.Itoa(v), nil) }()

	v = e.minVersionVal
	return
}

func (e *TestEnv) setQueryStateImpl(key, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queryState[key] = value
}

func (e *TestEnv) nowImpl() (ms int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "Now"
	if cachedResp, _, matched := e.replayLookup("Now", replayKey); matched {
		ms, _ = strconv.ParseInt(cachedResp, 10, 64)
		return
	}
	defer func() { e.replayRecord("Now", replayKey, strconv.FormatInt(ms, 10), nil) }()

	ms = e.nowMs
	return
}

func (e *TestEnv) randomImpl() (val int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "Random"
	if cachedResp, _, matched := e.replayLookup("Random", replayKey); matched {
		val, _ = strconv.ParseInt(cachedResp, 10, 64)
		return
	}
	defer func() { e.replayRecord("Random", replayKey, strconv.FormatInt(val, 10), nil) }()

	if e.randomIdx < len(e.randomSeq) {
		val = e.randomSeq[e.randomIdx]
		e.randomIdx++
		return
	}
	return
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
	_ = name // no-op for testing; SDK layer stores handler+validator in a map
}

// HandleUpdate invokes a registered update handler by name with the given payload.
func (e *TestEnv) HandleUpdate(name, payload string) (string, error) {
	if e.h.HostCallsImpl != nil {
		return e.h.HandleUpdate(name, payload)
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

func (e *TestEnv) pluginCallImpl(pluginName, functionName, inputJSON string) (resp string, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "PluginCall|" + pluginName + "|" + functionName + "|" + inputJSON
	if cachedResp, cachedErr, matched := e.replayLookup("PluginCall", replayKey); matched {
		return cachedResp, cachedErr
	}
	defer func() { e.replayRecord("PluginCall", replayKey, resp, retErr) }()

	for _, stub := range e.pluginCallStubs {
		if stub.pluginName == pluginName && stub.functionName == functionName {
			return stub.result, stub.err
		}
	}
	return "", fmt.Errorf("cleattest: no stub registered for PluginCall(%q, %q)", pluginName, functionName)
}

// sendSignalAndWaitImpl sends a signal and registers a reply channel.
func (e *TestEnv) sendSignalAndWaitImpl(targetRunID, signalName, payload string, timeout time.Duration) (resp string, retErr error) {
	e.mu.Lock()

	replayKey := "SendSignalAndWait|" + targetRunID + "|" + signalName + "|" + payload + "|" + timeout.String()
	if cachedResp, cachedErr, matched := e.replayLookup("SendSignalAndWait", replayKey); matched {
		e.mu.Unlock()
		return cachedResp, cachedErr
	}

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
		resp = ""
		retErr = err
		e.mu.Lock()
		e.replayRecord("SendSignalAndWait", replayKey, resp, retErr)
		e.mu.Unlock()
		return
	}

	// Wait for the reply with a timeout.
	select {
	case response := <-replyCh: // cleat:allow E002 -- SDK test helper, not user workflow
		resp = response
		retErr = nil
	case <-e.clock.After(timeout): // cleat:allow E002,E014 -- SDK test helper; intentional timeout pattern
		resp = ""
		retErr = fmt.Errorf("cleattest: SendSignalAndWait timed out after %v", timeout)
	}

	e.mu.Lock()
	e.replayRecord("SendSignalAndWait", replayKey, resp, retErr)
	e.mu.Unlock()
	return
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
	e.mu.Lock()

	replayKey := "SignalWorkflow|" + targetRunID + "|" + signalName + "|" + payload
	if _, cachedErr, matched := e.replayLookup("SignalWorkflow", replayKey); matched {
		e.mu.Unlock()
		return cachedErr
	}

	e.mu.Unlock()
	e.Signal(signalName, payload)

	e.mu.Lock()
	e.replayRecord("SignalWorkflow", replayKey, "", nil)
	e.mu.Unlock()
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
func (e *TestEnv) acquireLockImpl(key string, ttlMs int64) (acquired bool, retErr error) {
	e.mu.Lock()

	replayKey := "AcquireLock|" + key
	if cachedResp, cachedErr, matched := e.replayLookup("AcquireLock", replayKey); matched {
		e.mu.Unlock()
		acquired, _ = strconv.ParseBool(cachedResp)
		return acquired, cachedErr
	}

	e.mu.Unlock()

	// AcquireConcurrencyKey acquires e.mu internally, so we must not hold it here.
	acquired, retErr = e.AcquireConcurrencyKey(key, "")

	e.mu.Lock()
	e.replayRecord("AcquireLock", replayKey, strconv.FormatBool(acquired), retErr)
	e.mu.Unlock()
	return
}

func (e *TestEnv) releaseLockImpl(key string) (retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "ReleaseLock|" + key
	if _, cachedErr, matched := e.replayLookup("ReleaseLock", replayKey); matched {
		return cachedErr
	}
	defer func() { e.replayRecord("ReleaseLock", replayKey, "", retErr) }()

	delete(e.ConcurrencyKeys, key)
	return
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

func (e *TestEnv) durableSendImpl(service, operation, requestJSON string) (retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "DurableSend|" + service + "|" + operation + "|" + requestJSON
	if _, cachedErr, matched := e.replayLookup("DurableSend", replayKey); matched {
		return cachedErr
	}
	defer func() { e.replayRecord("DurableSend", replayKey, "", retErr) }()

	// Fire-and-forget: no-op in tests
	return nil
}

func (e *TestEnv) durableScheduleInvokeImpl(service, operation, requestJSON string, delayMs int64) (retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "ScheduleInvoke|" + service + "|" + operation + "|" + requestJSON + "|" + strconv.FormatInt(delayMs, 10)
	if _, cachedErr, matched := e.replayLookup("ScheduleInvoke", replayKey); matched {
		return cachedErr
	}
	defer func() { e.replayRecord("ScheduleInvoke", replayKey, "", retErr) }()

	// Scheduling: no-op in tests
	return nil
}

func (e *TestEnv) sideEffectImpl(computedResult string) (resp string, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	replayKey := "SideEffect|" + computedResult
	if cachedResp, cachedErr, matched := e.replayLookup("SideEffect", replayKey); matched {
		return cachedResp, cachedErr
	}
	defer func() { e.replayRecord("SideEffect", replayKey, resp, retErr) }()

	resp = computedResult
	return
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

// replayLookup checks the replay history. Returns cached response if found.
// Only active during replay (not recording). Must be called with e.mu held.
func (e *TestEnv) replayLookup(function, replayKey string) (resp string, retErr error, matched bool) {
	if !e.replayMode || e.replayRecording {
		return "", nil, false
	}
	for _, rec := range e.replayHistory {
		if rec.Function == function && rec.Args == replayKey {
			return rec.Response, rec.Err, true
		}
	}
	e.replayDivergence++
	return "", nil, false
}

// replayRecord records a call in replay history (recording phase only).
// Must be called with e.mu held.
func (e *TestEnv) replayRecord(function, replayKey, response string, err error) {
	if !e.replayMode || !e.replayRecording {
		return
	}
	e.replayHistory = append(e.replayHistory, RecordedCall{
		Function: function,
		Args:     replayKey,
		Response: response,
		Err:      err,
	})
}

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

// Cron schedules.
//
// These validate with engine.ValidateCronExpr and engine.ValidateTimezone --
// the same functions the host calls use -- rather than with rules invented
// here. A harness that accepted an expression the engine rejects would turn a
// production failure into a green test, which is worse than not implementing
// the call at all.
//
// What is NOT reproduced: the engine derives a schedule ID from (tenant,
// workflow, step) so that a retry after a crash addresses the same row. There
// is no replay and no crash here, so these IDs are a simple counter, which
// also makes them assertable.

func (e *TestEnv) scheduleCronImpl(workflowName, cronExpr, timezone, inputJSON string) (string, error) {
	if workflowName == "" {
		return "", fmt.Errorf("schedule_cron: workflow name is empty")
	}
	if err := engine.ValidateCronExpr(cronExpr); err != nil {
		return "", fmt.Errorf("schedule_cron %q: %w", workflowName, err)
	}
	if err := engine.ValidateTimezone(timezone); err != nil {
		return "", fmt.Errorf("schedule_cron %q: %w", workflowName, err)
	}
	if inputJSON == "" {
		inputJSON = "{}"
	}
	if !json.Valid([]byte(inputJSON)) {
		return "", fmt.Errorf("schedule_cron %q: input is not valid JSON", workflowName)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.cronCounter++
	scheduleID := fmt.Sprintf("cron-%d", e.cronCounter)
	tz := timezone
	if tz == "" {
		tz = engine.DefaultScheduleTimezone
	}
	e.crons[scheduleID] = cleat.CronSchedule{
		ScheduleID:   scheduleID,
		WorkflowName: workflowName,
		CronExpr:     cronExpr,
		Timezone:     tz,
		Input:        inputJSON,
		Enabled:      true,
	}
	return scheduleID, nil
}

// deleteCronImpl removes a schedule. Deleting one that is not there is not an
// error, matching the host call -- at-least-once delivery means a workflow
// retries, and the second delete must not fail.
func (e *TestEnv) deleteCronImpl(scheduleID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.crons, scheduleID)
	return nil
}

func (e *TestEnv) listCronsImpl() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	ids := make([]string, 0, len(e.crons))
	for id := range e.crons {
		ids = append(ids, id)
	}
	// Sorted, because map iteration order is not stable and a workflow that
	// walked this list would be non-deterministic -- the one thing a durable
	// workflow must never be.
	sort.Strings(ids)

	list := make([]cleat.CronSchedule, 0, len(ids))
	for _, id := range ids {
		list = append(list, e.crons[id])
	}
	out, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("list_crons: %w", err)
	}
	return string(out), nil
}

// Crons returns the schedules currently registered, for assertions.
func (e *TestEnv) Crons() []cleat.CronSchedule {
	listJSON, err := e.listCronsImpl()
	if err != nil {
		return nil
	}
	var list []cleat.CronSchedule
	_ = json.Unmarshal([]byte(listJSON), &list)
	return list
}
