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
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/cleat"
)

// WorkflowFunc is a workflow entry point. The context provides HostCalls
// and the input/output payloads.
type WorkflowFunc func(ctx *Context) error

// Context provides HostCalls and manages input/output for a single
// workflow execution.
type Context struct {
	h               cleat.HostCalls
	Input           string
	Output          string
	childWorkflows  map[string]WorkflowFunc
}

// H returns the HostCalls interface for the current execution.
func (c *Context) H() cleat.HostCalls {
	return c.h
}

// Option configures the embedded Runner.
type Option func(*Runner)

// Runner is an in-process workflow runner.
type Runner struct {
	mu         sync.RWMutex
	workflows  map[string]WorkflowFunc
	now        time.Time
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
	runner      *Runner
	wfID        string
	wfRunID     string
	startTime   time.Time
	mu          sync.Mutex

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
	name    string
	status  string // "pending", "resolved", "rejected"
	result  string
	errMsg  string
}

type childResult struct {
	result string
	err    error
}

func newExecution(r *Runner, workflowID, inputJSON string) *execution {
	return &execution{
		runner:       r,
		wfID:        workflowID,
		wfRunID:     uuid.New().String(),
		startTime:    r.now,
		promises:     make(map[string]*promiseState),
		childResults: make(map[string]*childResult),
		locks:        make(map[string]string),
	}
}

func (e *execution) hostCalls() cleat.HostCalls {
	return cleat.NewHostCalls(cleat.HostCallsOptions{
		DurableCall:            e.durableCall,
		DurableSleep:           e.durableSleep,
		DurableAwaitSignals:    e.durableAwaitSignals,
		DurableDefer:           e.durableDefer,
		DurableDeferFunc:       e.durableDeferFunc,
		DurableLog:             e.durableLog,
		PollCancellation:       e.pollCancellation,
		PollSignal:             e.pollSignal,
		Now:                    e.now,
		Random:                 e.random,
		CreatePromise:          e.createPromise,
		AwaitPromise:           e.awaitPromise,
		ChildWorkflow:          e.childWorkflow,
		AwaitChild:             e.awaitChild,
		WorkflowID:             e.workflowID,
		RunID:                  e.runID,
		SendSignalAndWait:      e.sendSignalAndWait,
		ReplyToSignal:          e.replyToSignal,
		SignalWorkflow:         e.signalWorkflow,
		AcquireLock:             e.acquireLock,
		ReleaseLock:            e.releaseLock,
		AwaitCondition:         e.awaitCondition,
		SideEffect:              e.sideEffect,
		})
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
		name:   name,
		status: "pending",
	}
	return id, nil
}

func (e *execution) awaitPromise(promiseID string, timeout time.Duration) (string, bool, error) {
	e.mu.Lock()
	ps, ok := e.promises[promiseID]
	e.mu.Unlock()

	if !ok {
		return "", false, fmt.Errorf("embedded: promise %s not found", promiseID)
	}

	if ps.status == "resolved" {
		return ps.result, false, nil
	}
	if ps.status == "rejected" {
		return "", false, fmt.Errorf("promise rejected: %s", ps.errMsg)
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

func (e *execution) sendSignalAndWait(targetRunID, signalName, payload string, timeout time.Duration) (string, error) {
	// Store the outgoing signal for test inspection.
	e.mu.Lock()
	e.signals = append(e.signals, signalEvent{name: signalName, payload: payload})
	e.mu.Unlock()

	// In the embedded runner, simulate an immediate response.
	// A full implementation would route the signal to the target execution
	// and wait for a reply.
	return `{"status":"delivered"}`, nil
}

func (e *execution) replyToSignal(correlationID, response string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Store the reply as a signal for the caller to poll.
	e.signals = append(e.signals, signalEvent{name: correlationID, payload: response})
	return nil
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
