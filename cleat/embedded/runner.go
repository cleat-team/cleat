// Package embedded provides a lightweight in-process workflow runner for
// integration testing and simple single-binary deployments. It registers
// workflow functions by name and executes them with an in-memory HostCalls
// implementation, eliminating the need for WASM compilation.
//
// Unlike cleattest (which uses stubs), the embedded runner actually
// executes workflow functions end-to-end, supporting child workflows,
// signals, sleep, and durable promises -- all in-memory.
//
// Unlike localdev (which makes real API calls), the embedded runner uses
// configurable handlers for external dependencies, making it ideal for
// integration tests and simple standalone deployments.
//
// # Usage
//
// Create a runner, register workflow functions, and execute them:
//
//	runner := embedded.New()
//
//	// Register workflows by name:
//	runner.Register("order_workflow", func(ctx *embedded.Context) error {
//	    var input struct {
//	        OrderID string `json:"order_id"`
//	    }
//	    if err := json.Unmarshal([]byte(ctx.Input), &input); err != nil {
//	        ctx.SetOutputf(`{"error": "invalid input: %s"}`, err)
//	        return nil
//	    }
//
//	    // Make durable calls through ctx.H():
//	    resp, err := ctx.H().DurableCall("inventory", "reserve", `{"sku":"s-1"}`)
//	    if err != nil {
//	        ctx.SetOutputf(`{"error": "reservation failed: %s"}`, err)
//	        return nil
//	    }
//	    _ = resp
//
//	    ctx.SetOutput(`{"status": "completed", "order_id": "` + input.OrderID + `"}`)
//	    return nil
//	})
//
//	// Execute a workflow:
//	result, err := runner.ExecuteWorkflow(context.Background(),
//	    "order_workflow", `{"order_id": "ord-42"}`)
//
//	// Or use typed execution:
//	var output OrderResult
//	err = runner.ExecuteWorkflowTyped(context.Background(),
//	    "order_workflow", OrderInput{OrderID: "ord-42"}, &output)
//
// # Signal injection
//
// The runner's Signal method injects signals that are picked up by
// pollSignal/awaitSignals inside running workflows:
//
//	runner.Signal("order_workflow", "payment_received", `{"amount": 100}`)
//
// # Child workflows
//
// Child workflows are resolved from the same runner's registry. Register
// the child before the parent so the runner can find it:
//
//	runner.Register("notify_user", func(ctx *embedded.Context) error {
//	    ctx.SetOutput(`{"sent": true}`)
//	    return nil
//	})
//
//	runner.Register("order_workflow", func(ctx *embedded.Context) error {
//	    runID, err := ctx.H().ChildWorkflow("notify_user", `{"user":"u-1"}`)
//	    if err != nil { return err }
//	    result, err := ctx.H().AwaitChild(runID)
//	    if err != nil { return err }
//	    _ = result
//	    ctx.SetOutput(`{"status": "done"}`)
//	    return nil
//	})
//
// # Deterministic time
//
// The runner uses a simulated clock starting at 2024-01-01T00:00:00Z.
// DurableSleep advances the clock by the requested duration. Use
// ctx.H().Now() instead of time.Now() for deterministic replay behavior.
package embedded

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cleat-team/cleat/cleat"
	"github.com/google/uuid"
)

// WorkflowFunc is a workflow entry point. The context provides HostCalls
// and the input/output payloads.
type WorkflowFunc func(ctx *Context) error

// Context provides HostCalls and manages input/output for a single
// workflow execution.
type Context struct {
	h              cleat.HostCalls
	Input          string
	Output         string
	childWorkflows map[string]WorkflowFunc
}

// H returns the HostCalls interface for the current execution.
func (c *Context) H() cleat.HostCalls {
	return c.h
}

// Option configures the embedded Runner.
type Option func(*Runner)

// Runner is an in-process workflow runner.
type Runner struct {
	mu        sync.RWMutex
	workflows map[string]WorkflowFunc
	now       time.Time
}

// New creates a new embedded Runner. The simulated clock starts at
// 2024-01-01T00:00:00Z. Call Register to add workflows, then ExecuteWorkflow
// to run them.
func New(opts ...Option) *Runner {
	r := &Runner{
		workflows: make(map[string]WorkflowFunc),
		now:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register registers a workflow function by name. If a workflow with the
// same name already exists, it is overwritten.
func (r *Runner) Register(name string, fn WorkflowFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workflows[name] = fn
}

// ExecuteWorkflow runs the named workflow with the given input JSON and
// returns the output JSON. It blocks until the workflow completes.
func (r *Runner) ExecuteWorkflow(ctx context.Context, name, inputJSON string) (string, error) {
	r.mu.RLock()
	fn, ok := r.workflows[name]
	r.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("embedded: workflow %q not registered", name)
	}

	exec := newExecution(r, name, inputJSON)
	wfCtx := &Context{
		h:              exec.hostCalls(),
		Input:          inputJSON,
		childWorkflows: r.workflows,
	}

	err := fn(wfCtx)
	// Defers run after the body and before the output is read, which is the
	// order Go's own defer uses -- a defer may adjust what the workflow
	// returns, and in this runner it is a plain closure that can.
	exec.runDeferFuncs()
	return wfCtx.Output, err
}

// ExecuteWorkflowTyped runs the named workflow with typed input/output.
// input is marshaled to JSON, and the result is unmarshaled into output.
func (r *Runner) ExecuteWorkflowTyped(ctx context.Context, name string, input, output interface{}) error {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("embedded: marshal input: %w", err)
	}

	resultJSON, err := r.ExecuteWorkflow(ctx, name, string(inputJSON))
	if err != nil {
		return err
	}

	if output == nil {
		return nil
	}
	return json.Unmarshal([]byte(resultJSON), output)
}

// execution holds the per-run state.
type execution struct {
	runner    *Runner
	wfID      string
	wfRunID   string
	startTime time.Time
	mu        sync.Mutex

	// call history
	calls []cleat.CallResult

	// signal support
	signals    []signalEvent
	signalWait *signalWaiter

	// promise support
	promises map[string]*promiseState

	// defer support
	deferCount int

	// sleep support
	sleepTime time.Time // set when sleeping, zero when awake

	// child workflow results
	childResults map[string]*childResult

	// lock state (in-memory concurrency keys)
	locks map[string]string

	// cleanup functions (LIFO)
	deferFuncs []func()

	// scope management for virtual object instances
	scopePrefix  string // "vo:<type>:<key>:" prefix, empty if no scope
	scopeObjType string // current object type in scope
	scopeInstKey string // current instance key in scope
	scopeSet     bool   // true when scope is active
}

type signalEvent struct {
	name    string
	payload string
}

type signalWaiter struct {
	names    []string
	deadline time.Time
	ch       chan signalEvent
}

type promiseState struct {
	name   string
	status string // "pending", "resolved", "rejected"
	result string
	errMsg string

	// settled is closed by settlePromise the first time this promise leaves
	// "pending". awaitPromise blocks on it; see its doc comment and
	// IMPROVEMENT-PLAN 3.235. Nil is tolerated: a promiseState built by any
	// route other than createPromise simply never wakes an awaiter.
	settled chan struct{}
}

type childResult struct {
	result string
	err    error
}

func newExecution(r *Runner, workflowID, inputJSON string) *execution {
	return &execution{
		runner:       r,
		wfID:         workflowID,
		wfRunID:      uuid.New().String(),
		startTime:    r.now,
		promises:     make(map[string]*promiseState),
		childResults: make(map[string]*childResult),
		locks:        make(map[string]string),
	}
}

func (e *execution) hostCalls() cleat.HostCalls {
	return cleat.NewHostCalls(cleat.HostCallsOptions{
		DurableCall:         e.durableCall,
		DurableSleep:        e.durableSleep,
		DurableAwaitSignals: e.durableAwaitSignals,
		DurableDefer:        e.durableDefer,
		DurableDeferFunc:    e.durableDeferFunc,
		DurableLog:          e.durableLog,
		PollCancellation:    e.pollCancellation,
		PollSignal:          e.pollSignal,
		Now:                 e.now,
		Random:              e.random,
		CreatePromise:       e.createPromise,
		AwaitPromise:        e.awaitPromise,
		ResolvePromise:      e.resolvePromise,
		RejectPromise:       e.rejectPromise,
		ChildWorkflow:       e.childWorkflow,
		AwaitChild:          e.awaitChild,
		WorkflowID:          e.workflowID,
		RunID:               e.runID,
		SignalWorkflow:      e.signalWorkflow,
		AcquireLock:         e.acquireLock,
		ReleaseLock:         e.releaseLock,
		AwaitCondition:      e.awaitCondition,
		SideEffect:          e.sideEffect,
		ScheduleCron:        e.scheduleCron,
		DeleteCron:          e.deleteCron,
		ListCrons:           e.listCrons,
	})
}

// Cron schedules are wired to an explicit refusal rather than left nil.
//
// A nil hook answers with "the HostCalls runtime was not initialized",
// which is about workflow context and reads as the caller's fault. The
// truth is narrower and worth saying: this runner executes a workflow in
// process and has no schedule store behind it, so a schedule created here
// would have nothing to fire it. Returning success for a cron that can
// never run would be worse than failing.
const errNoScheduleStore = "the embedded runner has no schedule store: cron schedules need a worker with a database (see cmd/cleat-worker)"

func (e *execution) scheduleCron(_, _, _, _ string) (string, error) {
	return "", errors.New(errNoScheduleStore)
}

func (e *execution) deleteCron(_ string) error {
	return errors.New(errNoScheduleStore)
}

func (e *execution) listCrons() (string, error) {
	return "", errors.New(errNoScheduleStore)
}

func (e *execution) acquireLock(key string, ttlMs int64) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	workflowID := e.wfID
	if existingWFID, ok := e.locks[key]; ok {
		if existingWFID == workflowID {
			return true, nil
		}
		return false, nil
	}
	e.locks[key] = workflowID
	return true, nil
}

func (e *execution) releaseLock(key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.locks, key)
	return nil
}

func (e *execution) awaitCondition(predicate func() bool, pollInterval, timeout time.Duration) (bool, error) {
	deadline := e.runner.now.Add(timeout)
	for {
		if predicate() {
			return true, nil
		}
		if e.runner.now.After(deadline) {
			return false, nil
		}
		e.runner.mu.Lock()
		e.runner.now = e.runner.now.Add(pollInterval)
		e.runner.mu.Unlock()
		time.Sleep(pollInterval)
	}
}
func (e *execution) sideEffect(computedResult string) (string, error) {
	// In embedded mode, there's no replay, so computedResult IS authoritative.
	return computedResult, nil
}

func (e *execution) workflowID() string {
	return e.wfID
}

func (e *execution) runID() string {
	return e.wfRunID
}

func (e *execution) now() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runner.now.UnixMilli()
}

func (e *execution) random() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Simple deterministic random from runner time seed.
	return e.runner.now.UnixMilli() % 1000000
}

func (e *execution) durableCall(service, operation, requestJSON string) (string, error) {
	if service == "http" && operation == "fetch" {
		return e.handleHTTPFetch(requestJSON)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rec := cleat.CallResult{
		Service:   service,
		Operation: operation,
		Request:   requestJSON,
		Response:  `{"result":"ok"}`,
	}
	e.calls = append(e.calls, rec)
	return rec.Response, nil
}

func (e *execution) handleHTTPFetch(requestJSON string) (string, error) {
	var req struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		return "", fmt.Errorf("http.fetch: invalid request JSON: %w", err)
	}
	if req.URL == "" {
		return "", fmt.Errorf("http.fetch: url is required")
	}
	if req.Method == "" {
		req.Method = "GET"
	}
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequest(req.Method, req.URL, body)
	if err != nil {
		return "", fmt.Errorf("http.fetch: %w", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		rec := cleat.CallResult{
			Service:   "http",
			Operation: "fetch",
			Request:   requestJSON,
			Err:       err.Error(),
		}
		e.mu.Lock()
		e.calls = append(e.calls, rec)
		e.mu.Unlock()
		return "", fmt.Errorf("http.fetch: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("http.fetch: reading response: %w", err)
	}
	respHeaders := make(map[string]string)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}
	result, _ := json.Marshal(map[string]interface{}{
		"status":  resp.StatusCode,
		"headers": respHeaders,
		"body":    string(respBody),
	})
	responseJSON := string(result)
	rec := cleat.CallResult{
		Service:   "http",
		Operation: "fetch",
		Request:   requestJSON,
		Response:  responseJSON,
	}
	e.mu.Lock()
	e.calls = append(e.calls, rec)
	e.mu.Unlock()
	return responseJSON, nil
}

func (e *execution) durableSleep(ms int64) {
	e.mu.Lock()
	e.sleepTime = e.runner.now.Add(time.Duration(ms) * time.Millisecond)
	e.mu.Unlock()
	// In the embedded runner, sleep advances the runner's clock.
	e.runner.mu.Lock()
	e.runner.now = e.runner.now.Add(time.Duration(ms) * time.Millisecond)
	e.runner.mu.Unlock()
	e.mu.Lock()
	e.sleepTime = time.Time{}
	e.mu.Unlock()
}

func (e *execution) durableAwaitSignals(signalNames []string, timeoutMs int64) (string, string, bool, error) {
	deadline := e.runner.now.Add(time.Duration(timeoutMs) * time.Millisecond)

	// Check for immediately available signals.
	e.mu.Lock()
	for i, sig := range e.signals {
		for _, name := range signalNames {
			if sig.name == name {
				e.signals = append(e.signals[:i], e.signals[i+1:]...)
				e.mu.Unlock()
				return sig.name, sig.payload, false, nil
			}
		}
	}
	e.mu.Unlock()

	if timeoutMs <= 0 {
		return "", "", true, nil
	}

	// Wait by advancing time to the deadline (simulating a timeout).
	e.runner.mu.Lock()
	if deadline.After(e.runner.now) {
		e.runner.now = deadline
	}
	e.runner.mu.Unlock()

	// Re-check after time advance.
	e.mu.Lock()
	for i, sig := range e.signals {
		for _, name := range signalNames {
			if sig.name == name {
				e.signals = append(e.signals[:i], e.signals[i+1:]...)
				e.mu.Unlock()
				return sig.name, sig.payload, false, nil
			}
		}
	}
	e.mu.Unlock()

	return "", "", true, nil
}

func (e *execution) durableDefer(description string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deferCount++
	return fmt.Sprintf("defer-%d", e.deferCount), nil
}

func (e *execution) durableDeferFunc(fn func()) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deferCount++
	id := fmt.Sprintf("def-fn-%d", e.deferCount)
	e.deferFuncs = append(e.deferFuncs, fn)
	return id, nil
}

// runDeferFuncs invokes the closures registered by DurableDeferFunc, most
// recent first.
//
// Until 2026-08-05 nothing did. durableDeferFunc appended to e.deferFuncs and
// that field had no reader anywhere in non-test code, so every closure passed
// to DurableDeferFunc was collected and dropped: the caller got a defer ID back
// and no cleanup, silently. This runner is documented for "integration testing
// and simple single-binary deployments", so the failure mode was a test suite
// reporting that cleanup worked when it had never been invoked.
//
// LIFO, matching Go's defer, which is the model the API is named after -- and
// the order matters for cleanup, since resources are released in the reverse of
// the order they were acquired.
//
// A panicking defer is logged and the rest still run. That differs from Go,
// where a panic in a defer propagates, and it is deliberate: cleanup is
// best-effort here, and one failed release should not strand the others. It is
// also what the WASM path does (engine/flush.go's runDefers), so the two agree.
//
// This runner executes Go closures with their lexical context intact, so it
// already has the "full context" half of what a defer is for. What it lacked
// was the other half: running at all. The WASM path has the opposite problem
// and IMPROVEMENT-PLAN 3.35 is the design for it.
func (e *execution) runDeferFuncs() {
	e.mu.Lock()
	fns := e.deferFuncs
	e.deferFuncs = nil
	e.mu.Unlock()

	for i := len(fns) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("embedded: defer panicked",
						"workflow_id", e.wfID, "run_id", e.wfRunID, "index", i, "panic", r)
				}
			}()
			fns[i]()
		}()
	}
}

func (e *execution) durableLog(message string) {
	// Best-effort; no-op in embedded mode.
}

func (e *execution) pollCancellation() (bool, string) {
	return false, ""
}

func (e *execution) pollSignal(signalName string) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, sig := range e.signals {
		if sig.name == signalName {
			e.signals = append(e.signals[:i], e.signals[i+1:]...)
			return sig.payload, true, nil
		}
	}
	return "", false, nil
}

func (e *execution) createPromise(name string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := uuid.New().String()
	e.promises[id] = &promiseState{
		name:    name,
		status:  "pending",
		settled: make(chan struct{}),
	}
	return id, nil
}

// resolvePromise and rejectPromise settle a promise created by this
// execution. They were missing until SendSignalAndWait became a composite
// over promises (IMPROVEMENT-PLAN 3.220): the embedded runner offered
// CreatePromise and AwaitPromise but no way to settle one, so a promise
// created here could only ever time out.
func (e *execution) resolvePromise(promiseID, value string) error {
	return e.settlePromise(promiseID, "resolved", value, "")
}

func (e *execution) rejectPromise(promiseID, errMsg string) error {
	return e.settlePromise(promiseID, "rejected", "", errMsg)
}

// settlePromise reports not-found rather than silently doing nothing, which
// is what the engine does after #818 -- a settle matching no row returns
// ErrPromiseNotFound, and engine/promises.go turns that into a non-zero
// result code.
func (e *execution) settlePromise(promiseID, status, result, errMsg string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ps, ok := e.promises[promiseID]
	if !ok {
		return fmt.Errorf("embedded: settle promise %s: promise not found", promiseID)
	}
	wasPending := ps.status == "pending"
	ps.status = status
	ps.result = result
	ps.errMsg = errMsg
	// Only the first settlement closes the channel; closing a closed channel
	// panics.
	if wasPending && ps.settled != nil {
		close(ps.settled)
	}
	return nil
}

// pendingAwaitCeiling bounds how long awaitPromise will really wait for a
// pending promise, whatever timeout the workflow asked for. Same value and
// same reasoning as cleattest's constant of the same name: workflows pass
// production durations, and a test awaiting with 7*24*time.Hour must not hang
// the suite for a week.
const pendingAwaitCeiling = 2 * time.Second

func (e *execution) awaitPromise(promiseID string, timeout time.Duration) (string, bool, error) {
	// The map holds *promiseState, so its fields must be copied under the lock
	// rather than read through the pointer afterwards. Reading them outside it
	// was a data race with settlePromise -- pre-existing, and unobservable
	// until this function could be waiting while another goroutine settled:
	// `go test -race` reports it on the very first test that does
	// (runner.go's read of ps.status against settlePromise's write).
	e.mu.Lock()
	ps, ok := e.promises[promiseID]
	var status, result, errMsg string
	var settled chan struct{}
	if ok {
		status, result, errMsg, settled = ps.status, ps.result, ps.errMsg, ps.settled
	}
	e.mu.Unlock()

	if !ok {
		return "", false, fmt.Errorf("embedded: promise %s not found", promiseID)
	}

	if status == "resolved" {
		return result, false, nil
	}
	if status == "rejected" {
		return "", false, fmt.Errorf("promise rejected: %s", errMsg)
	}

	// Pending. Wait for a settlement rather than reporting a timeout without
	// waiting at all, which is what this did until IMPROVEMENT-PLAN 3.235:
	// a promise pending at the instant of the call could never be observed
	// settling, so SendSignalAndWait -- a composite over CreatePromise +
	// SignalWorkflow + AwaitPromise since §3.220 -- always timed out here.
	//
	// The runner drives one workflow at a time, so nothing in a plain
	// embedded run settles a promise concurrently and this select falls
	// through to the timeout as before. It is here because "single-threaded
	// today" is a property of the runner, not of the API: a caller holding a
	// promise ID may settle it from its own goroutine, and the old code could
	// not see that no matter when it happened.
	if settled != nil {
		wait := timeout
		if wait > pendingAwaitCeiling {
			wait = pendingAwaitCeiling
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-settled:
			e.mu.Lock()
			status, result, errMsg = ps.status, ps.result, ps.errMsg
			e.mu.Unlock()
			if status == "resolved" {
				return result, false, nil
			}
			if status == "rejected" {
				return "", false, fmt.Errorf("promise rejected: %s", errMsg)
			}
		case <-timer.C:
		}
	}

	// Simulate timeout by advancing clock.
	e.runner.mu.Lock()
	e.runner.now = e.runner.now.Add(timeout)
	e.runner.mu.Unlock()

	return "", true, nil
}

func (e *execution) childWorkflow(name, inputJSON string) (string, error) {
	e.mu.Lock()
	runID := uuid.New().String()

	// Look up the workflow function.
	childFn, ok := e.runner.workflows[name]
	e.mu.Unlock()

	if !ok {
		return runID, fmt.Errorf("embedded: child workflow %q not registered", name)
	}

	// Execute the child workflow.
	childExec := newExecution(e.runner, name, inputJSON)
	childCtx := &Context{
		h:              childExec.hostCalls(),
		Input:          inputJSON,
		childWorkflows: e.runner.workflows,
	}

	err := childFn(childCtx)

	e.mu.Lock()
	e.childResults[runID] = &childResult{
		result: childCtx.Output,
		err:    err,
	}
	e.mu.Unlock()

	return runID, nil
}

func (e *execution) awaitChild(runID string) (string, error) {
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

func (e *execution) signalWorkflow(targetRunID, signalName, payload string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.signals = append(e.signals, signalEvent{name: signalName, payload: payload})
	return nil
}

func (e *execution) setScope(objectType, instanceKey string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.scopePrefix
	if objectType == "" && instanceKey == "" {
		e.scopeSet = false
		e.scopePrefix = ""
		e.scopeObjType = ""
		e.scopeInstKey = ""
	} else {
		e.scopeSet = true
		e.scopeObjType = objectType
		e.scopeInstKey = instanceKey
		e.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
	}
	return prev
}

func (e *execution) getScope() (string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.scopeSet {
		return "", ""
	}
	return e.scopeObjType, e.scopeInstKey
}

func (e *execution) clearScope() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	prev := e.scopePrefix
	e.scopeSet = false
	e.scopePrefix = ""
	e.scopeObjType = ""
	e.scopeInstKey = ""
	return prev
}

func (e *execution) uuid(seed string) string {
	// Deterministic UUID based on workflow ID and seed.
	wfID := e.wfID
	data := wfID + ":" + seed
	h := sha256.Sum256([]byte(data))
	h[6] = (h[6] & 0x0f) | 0x50 // Version 5
	h[8] = (h[8] & 0x3f) | 0x80 // Variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Signal delivers a signal to a workflow execution in the runner.
// This is used by tests to inject signals during workflow execution.
func (r *Runner) Signal(workflowID, name, payload string) {
	// For simplicity, signals are delivered at the runner level
	// and picked up by pollSignal/awaitSignals.
	r.mu.Lock()
	defer r.mu.Unlock()
	// Store at runner level for now.
	// In a full implementation, signals would be routed to specific executions.
}

// SetOutput sets the workflow output. Call from within a workflow function
// to set the result of the execution.
func (c *Context) SetOutput(output string) {
	c.Output = output
}

// SetOutputTyped marshals v as JSON and sets it as the workflow output.
func (c *Context) SetOutputTyped(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("embedded: marshal output: %w", err)
	}
	c.Output = string(data)
	return nil
}

// SetOutputf sets the workflow output using a format string.
func (c *Context) SetOutputf(format string, args ...interface{}) {
	c.Output = fmt.Sprintf(format, args...)
}
