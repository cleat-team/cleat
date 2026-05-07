// Package durabletest provides a mock HostCalls implementation for testing
// workflows without compiling to WASM or running a full host.
package durabletest

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rcownie/durable/durable"
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
		panic(fmt.Sprintf("durabletest: ReturnJSON marshal error: %v", marshalErr))
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

// TestEnv is a mock environment for testing workflows.
// Use NewTestEnv to create one, then wire up stubs with OnCall
// and drive the workflow via the HostCalls returned by H().
type TestEnv struct {
	mu             sync.Mutex
	h              durable.HostCalls
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

	ConcurrencyKeys           map[string]string
	AcquireConcurrencyKeyFn   func(key, workflowID string) (bool, error)
	ReleaseConcurrencyKeysFn  func(workflowID string)
}

// NewTestEnv creates a new TestEnv with a clean initial state.
// The simulated clock starts at 2024-01-01T00:00:00Z.
func NewTestEnv() *TestEnv {
	e := &TestEnv{
		nowMs:         time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		versionVal:    1,
		minVersionVal: 1,
		queryState:    make(map[string]string),
		promises:      make(map[string]promiseState),
		ConcurrencyKeys: make(map[string]string),
	}
	e.h = durable.NewHostCalls(durable.HostCallsOptions{
		DurableCall:               e.durableCallImpl,
		DurableCallWithOptions:    e.durableCallWithOptionsImpl,
		DurableSleep:              e.durableSleepImpl,
		DurableAwaitSignals:       e.durableAwaitSignalsImpl,
		DurableDefer:              e.durableDeferImpl,
		DurableLog:                e.durableLogImpl,
		PollCancellation:          e.pollCancellationImpl,
		PollSignal:                e.pollSignalImpl,
		ContinueAsNew:             e.continueAsNewImpl,
		ChildWorkflow:             e.childWorkflowImpl,
		AwaitChild:                e.awaitChildImpl,
		Version:                   e.versionImpl,
		MinVersion:                e.minVersionImpl,
		SetQueryState:             e.setQueryStateImpl,
		Now:                       e.nowImpl,
		Random:                    e.randomImpl,
		CreatePromise:             e.createPromiseImpl,
		AwaitPromise:              e.awaitPromiseImpl,
		RegisterUpdateHandler:     e.registerUpdateHandlerImpl,
		RunDetached:               e.runDetachedImpl,
	})
	return e
}

// H returns the HostCalls interface for workflow code to use.
func (e *TestEnv) H() durable.HostCalls {
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
		panic(fmt.Sprintf("durabletest: unsupported requestMatcher type %T", requestMatcher))
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
	t.Fatalf("durabletest: expected call to %s.%s was not made", service, operation)
}

// AssertNotCalled fails the test if a call to the given service+operation
// appears in the call history.
func (e *TestEnv) AssertNotCalled(t TestingT, service, operation string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range e.callHistory {
		if rec.Service == service && rec.Operation == operation {
			t.Fatalf("durabletest: unexpected call to %s.%s was made (request: %s)", service, operation, rec.Request)
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
	e.ConcurrencyKeys = make(map[string]string)
}

// ---------------------------------------------------------------------------
// Internal HostCalls function implementations
// ---------------------------------------------------------------------------

func (e *TestEnv) durableCallImpl(service, operation, requestJSON string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec := CallRecord{
		Service:   service,
		Operation: operation,
		Request:   requestJSON,
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
	err := fmt.Errorf("durabletest: no stub registered for %s.%s (request: %q)", service, operation, requestJSON)
	rec.Err = err
	e.callHistory = append(e.callHistory, rec)
	return "", err
}

func (e *TestEnv) durableCallWithOptionsImpl(_ durable.CallOptions, service, operation, requestJSON string) (string, error) {
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

func (e *TestEnv) childWorkflowImpl(name, inputJSON string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return fmt.Sprintf("child-%s-%d", name, e.deferCounter), nil
}

func (e *TestEnv) awaitChildImpl(runID string) (string, error) {
	return `{"status":"completed"}`, nil
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

func (e *TestEnv) runDetachedImpl(fn func(h durable.HostCalls) error) error {
	// Run the function directly. In test mode there is no cancellation anyway.
	return fn(e.h)
}

func (e *TestEnv) awaitPromiseImpl(promiseID string, timeout time.Duration) (string, bool, error) {
	e.mu.Lock()
	ps, ok := e.promises[promiseID]
	e.mu.Unlock()

	if !ok {
		return "", false, fmt.Errorf("durabletest: promise %s not found", promiseID)
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
