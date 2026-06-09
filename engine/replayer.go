package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tetratelabs/wazero"
)

// Replay replays a workflow from existing event history. Cached results are
// returned for matching steps; divergence triggers an error.
// queryState contains key-value state set via SetQueryState during execution.
func (e *Engine) Replay(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, history []EventRecord) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	// Check whether a WasmBackend is registered for this module's language.
	if backend := e.backendForWasm(wasmBytes); backend != nil {
		return e.executeWithBackend(ctx, backend, wasmBytes, entryPoint, input, history)
	}

	// Legacy path: compile and replay via the wazero Runtime.
	compiled, err := e.rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return "", nil, nil, nil, nil, fmt.Errorf("host: compile module: %w", err)
	}
	defer compiled.Close(ctx)
	return e.replayCompiled(ctx, compiled, entryPoint, input, history, wasmBytes)
}

// ReplayCompiled is like Replay but takes a pre-compiled module.
// Use this when the module has already been compiled and cached by a
// WorkflowLoader, avoiding redundant compilation.
func (e *Engine) ReplayCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage, history []EventRecord) (result string, resultHistory []EventRecord, suspended *SuspendResult, deferrals map[string]string, queryState map[string]string, err error) {
	return e.replayCompiled(ctx, compiled, entryPoint, input, history, nil)
}

// replayCompiled runs a replay using a pre-compiled module.
func (e *Engine) replayCompiled(ctx context.Context, compiled wazero.CompiledModule, entryPoint string, input json.RawMessage, history []EventRecord, wasmBytes []byte) (string, []EventRecord, *SuspendResult, map[string]string, map[string]string, error) {
	return e.executeCompiled(ctx, compiled, entryPoint, input, history, wasmBytes)
}

// advanceReplayStep increments stepCount and invokes the step callback if set.
// rec may be nil for inline replay paths without a full EventRecord.
// Returns false if the callback returned ReplayQuit (caller should abort).
func (s *execSession) advanceReplayStep(ctx context.Context, rec *EventRecord) bool {
	s.stepCount++
	if s.stepCallback == nil {
		return true
	}
	return s.invokeStepCallback(ctx, rec)
}

// invokeStepCallback invokes the step callback if set, building a queryState
// snapshot. Returns false if the callback returned ReplayQuit.
func (s *execSession) invokeStepCallback(ctx context.Context, rec *EventRecord) bool {
	if s.stepCallback == nil {
		return true
	}
	// Snapshot queryState to prevent callback from mutating it.
	qs := make(map[string]string, len(s.queryState))
	for k, v := range s.queryState {
		qs[k] = v
	}
	action := s.stepCallback(s.stepCount-1, rec, qs)
	if action == ReplayQuit {
		if s.stepCancel != nil {
			s.stepCancel()
		}
		return false
	}
	return true
}
