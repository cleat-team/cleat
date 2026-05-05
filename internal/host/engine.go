package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// ServiceCaller makes actual external API calls on behalf of durable workflows.
type ServiceCaller interface {
	Call(ctx context.Context, service, operation, requestJSON string) (responseJSON string, err error)
}

// CallRecord records a single durable API call in the event history.
type CallRecord struct {
	Step     int    `json:"step"`
	Service  string `json:"service"`
	Op       string `json:"op"`
	Request  string `json:"request"`
	Response string `json:"response"`
	Err      string `json:"err,omitempty"`
}

// Engine provides durable execution semantics (Execute/Replay) on top of a
// Runtime. It implements the checkpoint/replay model: on first execution,
// every DurableCall is recorded in the event history; on replay, cached
// results are returned and divergence is detected.
type Engine struct {
	rt     *Runtime
	caller ServiceCaller
}

// NewEngine creates an Engine backed by the given Runtime and ServiceCaller.
func NewEngine(rt *Runtime, caller ServiceCaller) *Engine {
	return &Engine{rt: rt, caller: caller}
}

// Execute runs a fresh execution of the workflow and returns the result
// along with the complete event history.
func (e *Engine) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage) (result string, history []CallRecord, err error) {
	return e.run(ctx, wasmBytes, entryPoint, input, nil)
}

// Replay replays a workflow from existing event history. Cached results are
// returned for matching steps; divergence triggers an error.
func (e *Engine) Replay(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, history []CallRecord) (result string, resultHistory []CallRecord, err error) {
	return e.run(ctx, wasmBytes, entryPoint, input, history)
}

func (e *Engine) run(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, history []CallRecord) (string, []CallRecord, error) {
	compiled, err := e.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", nil, fmt.Errorf("host: compile module: %w", err)
	}
	defer compiled.Close(ctx)

	mod, err := e.rt.InstantiateModule(ctx, compiled)
	if err != nil {
		return "", nil, fmt.Errorf("host: instantiate module: %w", err)
	}
	defer mod.Close(ctx)

	// Initialize the Go wasip1 runtime (calls _start in a goroutine, which
	// initializes WASI and then blocks in main() to keep the module alive).
	if err := e.rt.InitModule(ctx, mod); err != nil {
		return "", nil, fmt.Errorf("host: init module: %w", err)
	}

	session := &execSession{
		engine:   e,
		history:  history,
		isReplay: history != nil,
	}

	execCtx := withHandler(ctx, session)

	result, err := e.rt.CallExport(execCtx, mod, entryPoint, input)
	if err != nil {
		return "", session.history, err
	}

	return result, session.history, nil
}

// execSession implements HostHandler for a single execution or replay.
type execSession struct {
	engine    *Engine
	history   []CallRecord
	stepCount int
	isReplay  bool
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

	rec := CallRecord{
		Step:     s.stepCount,
		Service:  service,
		Op:       operation,
		Request:  requestJSON,
		Response: resp,
		Err:      callErr,
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

func (s *execSession) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	return 0
}

func (s *execSession) DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64 {
	return 0 // timeout by default
}

func (s *execSession) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	deferID := fmt.Sprintf("defer-%d", s.stepCount)
	mem := m.Memory()
	written := writeWasmString(mem, deferIDPtr, deferID, deferIDMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	return 0
}

func (s *execSession) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	return 0 // not cancelled
}

func (s *execSession) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	return 0 // not found
}

func (s *execSession) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	return 0
}

func (s *execSession) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	runID := fmt.Sprintf("child-%s-%d", name, s.stepCount)
	mem := m.Memory()
	written := writeWasmString(mem, runIDPtr, runID, runIDMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	result := `{"status":"completed"}`
	mem := m.Memory()
	written := writeWasmString(mem, resultPtr, result, resultMaxLen)
	return int64(uint64(written)<<32 | 0)
}

func (s *execSession) Version(ctx context.Context) int64 {
	return 1
}

func (s *execSession) MinVersion(ctx context.Context) int64 {
	return 1
}

func (s *execSession) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	return 0
}

func (s *execSession) Now(ctx context.Context) int64 {
	return nowMs.Load()
}

func (s *execSession) Random(ctx context.Context) int64 {
	return 42 // deterministic for replay
}
