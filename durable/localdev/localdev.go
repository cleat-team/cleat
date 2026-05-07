// Package localdev provides a pure-Go implementation of durable.HostCalls
// for local development and testing, eliminating the WASM build step.
//
// LocalRunner makes real API calls via a configurable ServiceCaller, records
// event history in memory, and supports signal delivery via Go channels.
// It implements the full HostCalls interface so any HostCalls-based workflow
// code can run locally without modification.
package localdev

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/durable/durable"
)

// ConcurrencyKeyTTL is the default TTL for concurrency key acquisitions in localdev.
// This is used by generated dev code when --concurrency-key is specified.
const ConcurrencyKeyTTL = 30 * time.Minute

// ServiceCaller makes external API calls on behalf of durable workflows.
// This mirrors host.ServiceCaller so that users of the localdev package
// do not need to import internal packages.
type ServiceCaller interface {
	Call(ctx context.Context, service, operation, requestJSON string) (responseJSON string, err error)
}

// ChildWorkflowRunner executes a child workflow by name.
// Implementations should run the child synchronously and return its result.
type ChildWorkflowRunner interface {
	RunChild(ctx context.Context, name string, inputJSON string) (resultJSON string, err error)
}

// childResult holds the result of a completed child workflow.
type childResult struct {
	result string
	err    error
}

// Event records a single event during workflow execution.
type Event struct {
	Type      string `json:"type"`                // "call", "sleep", "signal", "log", etc.
	Service   string `json:"service,omitempty"`   // target service name
	Operation string `json:"operation,omitempty"` // target operation name
	Request   string `json:"request,omitempty"`   // request payload
	Response  string `json:"response,omitempty"`  // response payload
	Err       string `json:"err,omitempty"`       // error string
	Message   string `json:"message,omitempty"`   // log message or description
	Elapsed   string `json:"elapsed,omitempty"`   // time since start
}

// Signal carries a signal name and payload.
type Signal struct {
	Name    string
	Payload string
}

// Option configures a LocalRunner.
type Option func(*LocalRunner)

// WithServiceCaller sets the ServiceCaller for making real API calls.
// If not set, DurableCall returns an error.
func WithServiceCaller(caller ServiceCaller) Option {
	return func(r *LocalRunner) { r.caller = caller }
}

// WithLogWriter sets the writer for DurableLog and event logging.
// Default is os.Stderr.
func WithLogWriter(w io.Writer) Option {
	return func(r *LocalRunner) { r.logWriter = w }
}

// WithSignalChannel sets the channel for external signal delivery.
// Default is a buffered channel of capacity 100.
func WithSignalChannel(ch chan Signal) Option {
	return func(r *LocalRunner) { r.signalCh = ch }
}

// WithWorkflowID sets the workflow ID for this runner.
func WithWorkflowID(id string) Option {
	return func(r *LocalRunner) { r.workflowID = id }
}

// WithVersion sets the version returned by Version() / MinVersion().
func WithVersion(v, minV int) Option {
	return func(r *LocalRunner) {
		r.versionVal = v
		r.minVersionVal = minV
	}
}

// WithChildWorkflowRunner configures a runner for executing child workflows.
// Without this, child workflows return stub results.
func WithChildWorkflowRunner(runner ChildWorkflowRunner) Option {
	return func(r *LocalRunner) {
		r.childRunner = runner
	}
}

// WithConcurrencyKey sets a concurrency key to be acquired before workflow execution.
// The key ensures only one workflow with this key runs at a time.
func WithConcurrencyKey(key string) Option {
	return func(r *LocalRunner) { r.concurrencyKey = key }
}

// localPromise holds the in-memory state of a durable promise.
type localPromise struct {
	name     string
	status   string
	result   string
	errorMsg string
	ch       chan struct{} // closed when resolved/rejected
}

// LocalRunner is a pure-Go implementation of durable.HostCalls for local
// development. It makes real API calls via a ServiceCaller, uses wall-clock
// time, and records events in memory.
//
// Use NewLocalRunner to create one, then pass H() to workflow entry points.
//
// Example:
//
//	runner := localdev.NewLocalRunner(
//	    localdev.WithServiceCaller(myCaller),
//	)
//	result, err := myworkflow.PlaceOrder(runner.H(), userID, cart)
//	for _, evt := range runner.Events() {
//	    log.Printf("[%s] %s", evt.Elapsed, evt.Type)
//	}
type LocalRunner struct {
	mu sync.RWMutex

	h         durable.HostCalls
	caller    ServiceCaller
	logWriter io.Writer
	signalCh  chan Signal
	events    []Event

	childRunner  ChildWorkflowRunner
	childResults map[string]childResult

	workflowID    string
	versionVal    int
	minVersionVal int
	queryState    map[string]string
	deferCounter  int

	continueInput string
	cancelled     bool
	cancelReason  string

	startTime   time.Time
	pendingSigs []Signal // signals buffered while no one is listening
	promises    map[string]*localPromise // durable promises keyed by promiseID

	concurrencyKey  string
	concurrencyKeys map[string]string // key -> workflowID
}

// NewLocalRunner creates a new LocalRunner with the given options.
func NewLocalRunner(opts ...Option) *LocalRunner {
	r := &LocalRunner{
		logWriter:     os.Stderr,
		signalCh:      make(chan Signal, 100),
		startTime:     time.Now(),
		versionVal:    1,
		minVersionVal: 1,
		queryState:    make(map[string]string),
		childResults:  make(map[string]childResult),
		promises:      make(map[string]*localPromise),
		concurrencyKeys: make(map[string]string),
	}
	for _, o := range opts {
		o(r)
	}
	r.h = durable.NewHostCalls(durable.HostCallsOptions{
		DurableCall:            r.durableCall,
		DurableCallWithOptions: r.durableCallWithOptions,
		DurableSleep:           r.durableSleepMs,
		DurableAwaitSignals:    r.durableAwaitSignals,
		DurableDefer:           r.durableDefer,
		DurableLog:             r.durableLog,
		PollCancellation:       r.pollCancellation,
		PollSignal:             r.pollSignal,
		ContinueAsNew:          r.continueAsNew,
		ChildWorkflow:          r.childWorkflow,
		AwaitChild:             r.awaitChild,
		Version:                r.version,
		MinVersion:             r.minVersion,
		SetQueryState:          r.setQueryState,
		Now:                    r.nowMs,
		Random:                 r.random,
		CreatePromise:          r.createPromiseImpl,
		AwaitPromise:           r.awaitPromiseImpl,
		RegisterUpdateHandler:  r.registerUpdateHandler,
		RunDetached:            r.runDetached,
	})
	return r
}

// H returns the HostCalls interface for use by workflow code.
func (r *LocalRunner) H() durable.HostCalls {
	return r.h
}

// Events returns a copy of all recorded events.
func (r *LocalRunner) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Event, len(r.events))
	copy(result, r.events)
	return result
}

// ContinueAsNewInput returns the input passed to ContinueAsNew, if any.
// After ContinueAsNew, the caller should create a new execution with this input.
func (r *LocalRunner) ContinueAsNewInput() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.continueInput, r.continueInput != ""
}

// SendSignal delivers a signal to a waiting workflow. If no one is currently
// awaiting signals, the signal is buffered and delivered on the next wait.
func (r *LocalRunner) SendSignal(name, payload string) {
	sig := Signal{Name: name, Payload: payload}
	select {
	case r.signalCh <- sig:
	default:
		r.mu.Lock()
		r.pendingSigs = append(r.pendingSigs, sig)
		r.mu.Unlock()
	}
}

// Cancel marks the workflow as cancelled with the given reason.
func (r *LocalRunner) Cancel(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelled = true
	r.cancelReason = reason
}

// elapsed returns the time since the runner was created.
func (r *LocalRunner) elapsed() time.Duration {
	return time.Since(r.startTime)
}

// ---------------------------------------------------------------------------
// HostCalls implementation methods (called via durable.NewHostCalls closures)
// ---------------------------------------------------------------------------

func (r *LocalRunner) durableCall(service, operation, requestJSON string) (string, error) {
	// Grab the caller reference outside the lock to avoid holding the lock
	// during an external HTTP call.
	r.mu.Lock()
	caller := r.caller
	r.mu.Unlock()

	if caller == nil {
		return "", fmt.Errorf("durable dev: no ServiceCaller configured, cannot call %s.%s", service, operation)
	}

	ctx := context.Background()
	callStart := time.Now()
	resp, err := caller.Call(ctx, service, operation, requestJSON)
	callElapsed := time.Since(callStart)

	evt := Event{
		Type:      "call",
		Service:   service,
		Operation: operation,
		Request:   requestJSON,
		Elapsed:   callElapsed.String(),
	}
	if err != nil {
		evt.Err = err.Error()
	} else {
		evt.Response = resp
	}

	r.mu.Lock()
	r.events = append(r.events, evt)
	r.mu.Unlock()

	r.logEvent("[%.3fs] %s.%s", r.elapsed().Seconds(), service, operation)
	if err != nil {
		r.logEvent("  -> error: %s", err)
	} else {
		r.logEvent("  -> %s", truncate(resp, 200))
	}

	return resp, err
}

func (r *LocalRunner) durableCallWithOptions(opts durable.CallOptions, service, operation, requestJSON string) (string, error) {
	if opts.Retry == nil {
		return r.durableCall(service, operation, requestJSON)
	}

	rp := opts.Retry
	var lastErr error
	for attempt := 1; attempt <= rp.MaxAttempts; attempt++ {
		resp, err := r.durableCall(service, operation, requestJSON)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if attempt < rp.MaxAttempts {
			backoff := time.Duration(float64(rp.InitialInterval) * pow(rp.BackoffCoefficient, float64(attempt-1)))
			if backoff > rp.MaxInterval {
				backoff = rp.MaxInterval
			}
			r.logEvent("[%.3fs] retry %s.%s in %s (attempt %d/%d)",
				r.elapsed().Seconds(), service, operation, backoff, attempt+1, rp.MaxAttempts)
			time.Sleep(backoff)
		}
	}
	return "", fmt.Errorf("durable dev: call %s.%s retry exhausted after %d attempts: %w",
		service, operation, rp.MaxAttempts, lastErr)
}

func (r *LocalRunner) durableSleepMs(ms int64) {
	r.mu.Lock()
	r.events = append(r.events, Event{
		Type:    "sleep",
		Message: fmt.Sprintf("%dms", ms),
		Elapsed: r.elapsed().String(),
	})
	r.mu.Unlock()

	r.logEvent("[%.3fs] sleep %dms", r.elapsed().Seconds(), ms)
	time.Sleep(time.Duration(ms) * time.Millisecond)
	r.logEvent("[%.3fs] wake", r.elapsed().Seconds())
}

func (r *LocalRunner) durableAwaitSignals(signalNames []string, timeoutMs int64) (string, string, bool, error) {
	r.mu.Lock()
	// Drain any pending signals that were buffered while no one was listening.
	for _, sig := range r.pendingSigs {
		select {
		case r.signalCh <- sig:
		default:
			// channel full, continue (shouldn't happen with buffered channel)
		}
	}
	r.pendingSigs = nil
	r.mu.Unlock()

	r.logEvent("[%.3fs] await signals [%s] timeout=%dms", r.elapsed().Seconds(), joinStrings(signalNames), timeoutMs)

	// Record the wait event.
	r.mu.Lock()
	r.events = append(r.events, Event{
		Type:    "await_signals",
		Message: fmt.Sprintf("signals=%v timeout=%dms", signalNames, timeoutMs),
	})
	r.mu.Unlock()

	if timeoutMs <= 0 {
		// Poll mode: try to receive a signal non-blocking.
		select {
		case sig := <-r.signalCh:
			if matchesAny(sig.Name, signalNames) {
				r.logEvent("[%.3fs] got signal %s", r.elapsed().Seconds(), sig.Name)
				return sig.Name, sig.Payload, false, nil
			}
			// Non-matching signal: discard in poll mode.
			r.logEvent("[%.3fs] discarding non-matching signal %s", r.elapsed().Seconds(), sig.Name)
		default:
		}
		return "", "", true, nil
	}

	// Wait for a matching signal or timeout.
	timeout := time.Duration(timeoutMs) * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case sig := <-r.signalCh:
			if matchesAny(sig.Name, signalNames) {
				r.logEvent("[%.3fs] got signal %s", r.elapsed().Seconds(), sig.Name)
				r.mu.Lock()
				r.events = append(r.events, Event{
					Type:    "signal",
					Message: fmt.Sprintf("name=%s payload=%s", sig.Name, sig.Payload),
				})
				r.mu.Unlock()
				return sig.Name, sig.Payload, false, nil
			}
			r.logEvent("[%.3fs] discarding non-matching signal %s (waiting for %v)",
				r.elapsed().Seconds(), sig.Name, signalNames)
		case <-timer.C:
			r.logEvent("[%.3fs] signal wait timeout", r.elapsed().Seconds())
			return "", "", true, nil
		}
	}
}

func (r *LocalRunner) durableDefer(description string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferCounter++
	deferID := fmt.Sprintf("defer-%d", r.deferCounter)
	r.events = append(r.events, Event{
		Type:    "defer",
		Message: fmt.Sprintf("id=%s desc=%s", deferID, description),
	})
	r.logEvent("[%.3fs] defer %s: %s", r.elapsed().Seconds(), deferID, description)
	return deferID, nil
}

func (r *LocalRunner) durableLog(message string) {
	r.logEvent("[%.3fs] log: %s", r.elapsed().Seconds(), message)
}

func (r *LocalRunner) pollCancellation() (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled, r.cancelReason
}

func (r *LocalRunner) pollSignal(signalName string) (string, bool, error) {
	// Try to read a signal with the given name without blocking.
	select {
	case sig := <-r.signalCh:
		if sig.Name == signalName {
			return sig.Payload, true, nil
		}
		// Non-matching signal: re-queue.
		r.mu.Lock()
		r.pendingSigs = append(r.pendingSigs, sig)
		r.mu.Unlock()
	default:
	}
	return "", false, nil
}

func (r *LocalRunner) continueAsNew(newInputJSON string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.continueInput = newInputJSON
	r.events = append(r.events, Event{
		Type:    "continue_as_new",
		Message: newInputJSON,
	})
	r.logEvent("[%.3fs] continue_as_new: %s", r.elapsed().Seconds(), newInputJSON)
	return nil
}

func (r *LocalRunner) childWorkflow(name, inputJSON string) (string, error) {
	runID := fmt.Sprintf("child-%s-%d", name, r.deferCounter)
	if r.childRunner != nil {
		ctx := context.Background()
		result, err := r.childRunner.RunChild(ctx, name, inputJSON)
		r.mu.Lock()
		r.childResults[runID] = childResult{result: result, err: err}
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.events = append(r.events, Event{
		Type:      "child_workflow",
		Service:   name,
		Operation: "start",
		Request:   inputJSON,
		Message:   runID,
	})
	r.mu.Unlock()
	r.logEvent("[%.3fs] child_workflow %s -> %s", r.elapsed().Seconds(), name, runID)
	return runID, nil
}

func (r *LocalRunner) awaitChild(runID string) (string, error) {
	r.mu.RLock()
	cr, ok := r.childResults[runID]
	r.mu.RUnlock()
	if ok {
		if cr.err != nil {
			return "", cr.err
		}
		return cr.result, nil
	}
	r.mu.Lock()
	r.events = append(r.events, Event{
		Type:    "await_child",
		Message: runID,
	})
	r.mu.Unlock()
	r.logEvent("[%.3fs] await_child %s", r.elapsed().Seconds(), runID)
	return `{"status":"completed"}`, nil
}

func (r *LocalRunner) version() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.versionVal
}

func (r *LocalRunner) minVersion() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.minVersionVal
}

func (r *LocalRunner) setQueryState(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queryState[key] = value
}

func (r *LocalRunner) nowMs() int64 {
	return time.Now().UnixMilli()
}

func (r *LocalRunner) random() int64 {
	// Use crypto/rand for a non-deterministic random number in dev mode.
	n, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return 42 // fallback
	}
	return n.Int64()
}

func (r *LocalRunner) createPromiseImpl(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Generate a UUID-based promise ID.
	id, err := uuid.NewRandom()
	var promiseID string
	if err != nil {
		promiseID = fmt.Sprintf("prom-%s-%d", r.workflowID, len(r.promises))
	} else {
		promiseID = id.String()
	}
	r.promises[promiseID] = &localPromise{
		name:   name,
		status: "pending",
		ch:     make(chan struct{}),
	}
	r.events = append(r.events, Event{
		Type:    "create_promise",
		Message: fmt.Sprintf("name=%s id=%s", name, promiseID),
	})
	r.logEvent("[%.3fs] create_promise %s -> %s", r.elapsed().Seconds(), name, promiseID)
	return promiseID, nil
}

func (r *LocalRunner) registerUpdateHandler(name string) {
	r.mu.Lock()
	r.events = append(r.events, Event{
		Type:    "register_update_handler",
		Message: name,
	})
	r.mu.Unlock()
	r.logEvent("[%.3fs] register_update_handler %s", r.elapsed().Seconds(), name)
}

func (r *LocalRunner) runDetached(fn func(h durable.HostCalls) error) error {
	r.mu.Lock()
	r.events = append(r.events, Event{
		Type:    "run_detached",
		Message: "starting detached execution",
	})
	r.mu.Unlock()
	r.logEvent("[%.3fs] run_detached", r.elapsed().Seconds())
	return fn(r.h)
}

func (r *LocalRunner) awaitPromiseImpl(promiseID string, timeout time.Duration) (string, bool, error) {
	r.mu.RLock()
	lp, ok := r.promises[promiseID]
	r.mu.RUnlock()

	if !ok {
		return "", false, fmt.Errorf("localdev: promise %s not found", promiseID)
	}

	if lp.status == "resolved" {
		return lp.result, false, nil
	}
	if lp.status == "rejected" {
		return lp.errorMsg, false, fmt.Errorf("promise rejected: %s", lp.errorMsg)
	}

	// Pending -- wait for resolution via channel or timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-lp.ch:
		// Promise was resolved/rejected.
		r.mu.RLock()
		defer r.mu.RUnlock()
		if lp.status == "resolved" {
			return lp.result, false, nil
		}
		return lp.errorMsg, false, fmt.Errorf("promise rejected: %s", lp.errorMsg)
	case <-timer.C:
		return "", true, nil
	}
}

// ---------------------------------------------------------------------------
// Concurrency Key methods
// ---------------------------------------------------------------------------

// AcquireConcurrencyKey attempts to acquire a concurrency key for a workflow.
// Returns true if acquired, false if already held by a different workflow.
// The ttl parameter is accepted for API compatibility but not enforced in memory.
func (r *LocalRunner) AcquireConcurrencyKey(key, workflowID string, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existingWFID, ok := r.concurrencyKeys[key]; ok {
		if existingWFID == workflowID {
			return true, nil // re-acquire by same workflow is OK
		}
		return false, nil // held by different workflow
	}
	r.concurrencyKeys[key] = workflowID
	return true, nil
}

// ReleaseConcurrencyKeys releases all concurrency keys held by a workflow.
func (r *LocalRunner) ReleaseConcurrencyKeys(workflowID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.concurrencyKeys {
		if v == workflowID {
			delete(r.concurrencyKeys, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (r *LocalRunner) logEvent(format string, args ...interface{}) {
	if r.logWriter != nil {
		fmt.Fprintf(r.logWriter, format+"\n", args...)
	}
}

func matchesAny(name string, names []string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func joinStrings(s []string) string {
	if len(s) == 0 {
		return ""
	}
	if len(s) == 1 {
		return s[0]
	}
	b := make([]byte, 0, len(s)*10)
	for i, v := range s {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, v...)
	}
	return string(b)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func pow(base, exp float64) float64 {
	result := 1.0
	for exp > 0 {
		result *= base
		exp--
	}
	return result
}
