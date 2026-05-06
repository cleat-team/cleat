package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// EventType classifies event history records.
type EventType string

const (
	EventTypeCall           EventType = "call"
	EventTypeSleep          EventType = "sleep"
	EventTypeAwaitSignals   EventType = "await_signals"
	EventTypeSignalReceived EventType = "signal_received"
	EventTypeDefer          EventType = "defer"
	EventTypeChildWorkflow  EventType = "child_workflow"
	EventTypeAwaitChild     EventType = "await_child"
	EventTypeContinueAsNew  EventType = "continue_as_new"
)

// EventRecord is a single event in a workflow's execution history.
// It generalizes the previous CallRecord to support sleep, signal, and defer events.
type EventRecord struct {
	Step      int       `json:"step"`
	EventType EventType `json:"type"`

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

	// Child workflow fields.
	ChildName        string `json:"child_name,omitempty"`
	ChildInput       string `json:"child_input,omitempty"`
	RunID            string `json:"run_id,omitempty"`
	ParentWorkflowID string `json:"parent_workflow_id,omitempty"`

	// ContinueAsNew fields.
	NewInput string `json:"new_input,omitempty"`
}

// CallRecord is kept for backward compatibility in tests.
type CallRecord = EventRecord

// ServiceCaller makes actual external API calls on behalf of durable workflows.
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

// WorkflowState provides access to workflow instance state.
type WorkflowState interface {
	// Version returns the workflow definition version for this instance.
	Version() int
	// MinVersion returns the minimum version this code supports.
	MinVersion() int
}

// SuspendError signals that the workflow should be suspended.
type SuspendError struct {
	Reason   string
	Until    time.Time // if non-zero, the workflow should wake at this time
	NewInput string    // for continue_as_new: the new input payload
}

func (e *SuspendError) Error() string {
	if !e.Until.IsZero() {
		return fmt.Sprintf("durable: suspend until %s: %s", e.Until, e.Reason)
	}
	return fmt.Sprintf("durable: suspend: %s", e.Reason)
}

// SuspendResult holds the outcome of a suspended workflow execution.
type SuspendResult struct {
	History      []EventRecord
	SuspendUntil time.Time
	Reason       string
	NewInput     string // for continue_as_new: the new input payload
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
type ChildWorkflowStore interface {
	StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string) (string, error)
	GetChildResult(ctx context.Context, runID string) (resultJSON string, completed bool, err error)
}

// RetryableError is optionally implemented by errors to indicate retryability.
type RetryableError interface {
	Retryable() bool
}

// Engine provides durable execution semantics (Execute/Replay) on top of a
// Runtime. It implements the checkpoint/replay model: on first execution,
// every DurableCall is recorded in the event history; on replay, cached
// results are returned and divergence is detected.
type Engine struct {
	rt           *Runtime
	caller       ServiceCaller
	signalStore  SignalStore
	state        WorkflowState
	workflowID   string
	childWfStore ChildWorkflowStore
}

// EngineOption configures an Engine.
type EngineOption func(*Engine)

// WithSignalStore sets the signal store for signal delivery and cancellation.
func WithSignalStore(ss SignalStore) EngineOption {
	return func(e *Engine) { e.signalStore = ss }
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
	return e.run(ctx, wasmBytes, entryPoint, input, nil)
}

// Replay replays a workflow from existing event history. Cached results are
// returned for matching steps; divergence triggers an error.
// queryState contains key-value state set via SetQueryState during execution.
func (e *Engine) Replay(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, history []EventRecord) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	return e.run(ctx, wasmBytes, entryPoint, input, history)
}

func (e *Engine) run(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, history []EventRecord) (string, []EventRecord, *SuspendResult, map[string]string, map[string]string, error) {
	compiled, err := e.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: compile module: %w", err)
	}
	defer compiled.Close(ctx)

	mod, err := e.rt.InstantiateModule(ctx, compiled)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: instantiate module: %w", err)
	}
	defer mod.Close(ctx)

	if err := e.rt.InitModule(ctx, mod); err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: init module: %w", err)
	}

	session := &execSession{
		engine:      e,
		history:     history,
		isReplay:    len(history) > 0,
		nowMs:       nowMs.Load(),
		deferrals:   make(map[string]string),
		workflowID:  e.workflowID,
	}

	execCtx := withHandler(ctx, session)

	result, err := e.rt.CallExport(execCtx, mod, entryPoint, input)
	if err != nil {
		if errors.Is(err, ErrSuspended) || session.suspendErr != nil {
			se := session.suspendErr
			if se == nil {
				se = &SuspendError{Reason: "workflow suspended"}
			}
			return "", session.history, &SuspendResult{
				History:      session.history,
				SuspendUntil: se.Until,
				Reason:       se.Reason,
				NewInput:     se.NewInput,
				Deferrals:    session.deferrals,
			}, session.deferrals, session.queryState, nil
		}
		return "", session.history, nil, nil, nil, err
	}

	return result, session.history, nil, session.deferrals, session.queryState, nil
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
	engine      *Engine
	history     []EventRecord
	stepCount   int
	isReplay    bool
	nowMs       int64
	suspendErr  *SuspendError
	signals     map[string]string // pending signals delivered during this session
	deferrals   map[string]string // registered defer callbacks (deferID -> description)
	workflowID  string            // parent workflow instance ID (for child workflows)
	queryState  map[string]string // key-value state set via SetQueryState
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

	resp, err := s.engine.caller.Call(ctx, service, operation, requestJSON)

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
	s.history = append(s.history, rec)
	s.stepCount++

	if err != nil {
		written := writeWasmString(mem, responsePtr, err.Error(), responseMaxLen)
		return packDurableCallResult(int(written), 1, 1)
	}

	written := writeWasmString(mem, responsePtr, resp, responseMaxLen)
	return packDurableCallResult(int(written), 0, 0)
}

func (s *execSession) replayCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	mem := m.Memory()

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		s.stepCount++

		if rec.EventType != EventTypeCall {
			errMsg := fmt.Sprintf("replay divergence at step %d: expected call event, got %s", rec.Step, rec.EventType)
			written := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Service != service || rec.Op != operation {
			errMsg := fmt.Sprintf("replay divergence at step %d: workflow called %s.%s but history has %s.%s",
				rec.Step, service, operation, rec.Service, rec.Op)
			written := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		if rec.Err != "" {
			written := writeWasmString(mem, responsePtr, rec.Err, responseMaxLen)
			return packDurableCallResult(int(written), 1, 1)
		}

		written := writeWasmString(mem, responsePtr, rec.Response, responseMaxLen)
		return packDurableCallResult(int(written), 0, 0)
	}

	// Past recorded history — switch to fresh execution.
	s.isReplay = false
	return s.freshCall(ctx, m, service, operation, requestJSON, responsePtr, responseMaxLen)
}

const (
	sleepStatusCompleted = 0
	sleepStatusSuspend   = 1
)

func (s *execSession) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeSleep {
				s.stepCount++
				return packSleepResult(sleepStatusCompleted, 0)
			}
		}
		// Past history, switch to fresh.
		s.isReplay = false
	}

	// Fresh execution: record sleep event and signal suspend.
	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeSleep,
		DurationMs: durationMs,
	}
	s.history = append(s.history, rec)
	s.stepCount++

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("durable_sleep(%dms)", durationMs),
		Until:  time.UnixMilli(s.nowMs).Add(time.Duration(durationMs) * time.Millisecond),
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
				written := writeWasmString(mem, sigNamePtr, rec.SignalName, sigNameMaxLen)
				_ = writeWasmString(mem, payloadPtr, rec.SignalPayload, payloadMaxLen)
				return packAwaitSignalsResult(uint32(written), uint32(len(rec.SignalPayload)), false, 0)
			}
			if rec.EventType == EventTypeAwaitSignals {
				s.stepCount++
				// Check if there's a following signal_received event.
				if s.stepCount < len(s.history) {
					nextRec := s.history[s.stepCount]
					if nextRec.EventType == EventTypeSignalReceived {
						s.stepCount++
						written := writeWasmString(mem, sigNamePtr, nextRec.SignalName, sigNameMaxLen)
						_ = writeWasmString(mem, payloadPtr, nextRec.SignalPayload, payloadMaxLen)
						return packAwaitSignalsResult(uint32(written), uint32(len(nextRec.SignalPayload)), false, 0)
					}
				}
				// No signal yet — this is a replay of a wait that hasn't resolved.
				// Should not happen in practice (we only wake when signal arrives),
				// but handle gracefully.
				return packAwaitSignalsResult(0, 0, true, 0)
			}
		}
		s.isReplay = false
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
				s.history = append(s.history, rec)
				s.stepCount++

				written := writeWasmString(mem, sigNamePtr, name, sigNameMaxLen)
				_ = writeWasmString(mem, payloadPtr, payload, payloadMaxLen)
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
	s.history = append(s.history, rec)
	s.stepCount++

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
				written := writeWasmString(mem, deferIDPtr, rec.DeferID, deferIDMaxLen)
				return int64(uint64(written)<<32 | 0)
			}
		}
		s.isReplay = false
	}

	deferID := fmt.Sprintf("defer-%d", s.stepCount)

	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypeDefer,
		DeferDescription: description,
		DeferID:          deferID,
	}
	s.history = append(s.history, rec)
	s.stepCount++

	s.deferrals[deferID] = description

	mem := m.Memory()
	written := writeWasmString(mem, deferIDPtr, deferID, deferIDMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) DurableLog(ctx context.Context, m api.Module, message string) int64 {
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
			_ = writeWasmString(mem, reasonPtr, reason, reasonMaxLen)
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
			written := writeWasmString(mem, payloadPtr, payload, payloadMaxLen)
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
		s.isReplay = false
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeContinueAsNew,
		NewInput:  newInputJSON,
	}
	s.history = append(s.history, rec)
	s.stepCount++

	s.suspendErr = &SuspendError{
		Reason:   "continue_as_new",
		NewInput: newInputJSON,
	}
	return 0
}

func (s *execSession) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeChildWorkflow {
				s.stepCount++
				mem := m.Memory()
				written := writeWasmString(mem, runIDPtr, rec.RunID, runIDMaxLen)
				return int64(uint64(written)<<32 | 0)
			}
		}
		s.isReplay = false
	}

	// Fresh execution: create child workflow via store or generate synthetic ID.
	var runID string
	if s.engine.childWfStore != nil {
		parentID := s.workflowID
		if parentID == "" {
			parentID = fmt.Sprintf("unknown-%s-%d", name, s.stepCount)
		}
		var err error
		runID, err = s.engine.childWfStore.StartChildWorkflow(ctx, parentID, name, inputJSON)
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
	}
	s.history = append(s.history, rec)
	s.stepCount++

	mem := m.Memory()
	written := writeWasmString(mem, runIDPtr, runID, runIDMaxLen)
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
						written := writeWasmString(mem, resultPtr, rec.Err, resultMaxLen)
						return packAwaitChildResult(uint32(written), 1)
					}
					written := writeWasmString(mem, resultPtr, rec.Response, resultMaxLen)
					return packAwaitChildResult(uint32(written), 0)
				}
				// No cached result yet — fall through to fresh to re-check.
				s.stepCount++
				s.isReplay = false
			}
		} else {
			s.isReplay = false
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
			s.history = append(s.history, rec)
			s.stepCount++

			written := writeWasmString(mem, resultPtr, result, resultMaxLen)
			return packAwaitChildResult(uint32(written), 0)
		}
		if err != nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeAwaitChild,
				RunID:     runID,
				Err:       err.Error(),
			}
			s.history = append(s.history, rec)
			s.stepCount++

			written := writeWasmString(mem, resultPtr, err.Error(), resultMaxLen)
			return packAwaitChildResult(uint32(written), 1)
		}
	}

	// Child not completed — record event and suspend.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeAwaitChild,
		RunID:     runID,
	}
	s.history = append(s.history, rec)
	s.stepCount++

	s.suspendErr = &SuspendError{
		Reason: fmt.Sprintf("await_child(%s)", runID),
	}

	return packAwaitChildResultSuspend()
}

func (s *execSession) DurableCallWithRetry(ctx context.Context, m api.Module,
	service, operation, requestJSON string,
	maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
	nonRetryableErrorsJSON string,
	responsePtr, responseMaxLen uint32) int64 {

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
			s.history = append(s.history, rec)
			s.stepCount++

			written := writeWasmString(mem, responsePtr, resp, responseMaxLen)
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
				time.Sleep(time.Duration(backoffMs) * time.Millisecond)
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
	s.history = append(s.history, rec)
	s.stepCount++

	written := writeWasmString(mem, responsePtr, errMsg, responseMaxLen)
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

func (s *execSession) Now(ctx context.Context) int64 {
	return s.nowMs
}

func (s *execSession) Random(ctx context.Context) int64 {
	return 42
}

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
