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
	backend, err := e.resolveBackend(wasmBytes)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	if backend != nil {
		return e.executeWithBackend(ctx, backend, wasmBytes, entryPoint, input, history)
	}

	// Legacy path: compile and replay via the wazero Runtime. The nil check is
	// not defensive padding -- the worker constructs its engines with a nil
	// Runtime, so reaching here without one used to panic rather than report.
	if e.rt == nil {
		return "", nil, nil, nil, nil, fmt.Errorf(
			"host: no runtime available for WASM replay; register a backend for this language with WithBackend")
	}
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

	// Track the virtual clock off consumed events, mirroring recordEvent doing
	// it off recorded ones. Without this the two executions are asymmetric: a
	// fresh run's nowMs follows its events, while a replay's stays at the
	// session seed for the whole replay.
	//
	// That asymmetry is observable because the seed and the event timestamps do
	// not come from the same clock. The seed is the workflow row's created_at,
	// written by the database; event timestamps are written by the worker
	// process. Measured 40ms apart with PostgreSQL in a container -- enough
	// that a replay could read a LATER clock than the original execution did,
	// which is a replay divergence and fails any SideEffect that read it.
	if rec != nil && rec.TimestampMs > 0 {
		s.nowMs = rec.TimestampMs
	}

	// Advance the checksum chain over consumed events too.
	//
	// lastChecksum is the predecessor the NEXT recorded event chains from, and
	// nothing seeded it from replayed history: it started empty on every
	// resumed session. So the first event a workflow recorded after a
	// resumption was written with a checksum chained from nothing, while
	// verification recomputes the chain from the events that are actually
	// there -- and the two disagree. The workflow then fails with
	//
	//	checksum mismatch (expected ..., got ...)
	//
	// which reads like corruption and is the integrity mechanism reporting on
	// its own bookkeeping.
	//
	// Found by a workflow that awaits a promise: the await suspends, the
	// resumed execution replays the await from history, finds the promise
	// still pending, and records a second await -- the first new event after a
	// replay, which is exactly the case that was wrong.
	//
	// The fresh paths already do this: recordEvent, callintent, and the atomic
	// child insert in children.go, which had the same omission fixed
	// call-site-by-call-site (#777). This is the same fix in the one place
	// every consumed event passes through, so it does not need repeating for
	// each event type.
	if rec != nil {
		s.lastChecksum = computeEventChecksum(*rec, s.lastChecksum)
	}
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
