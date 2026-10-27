//go:build cgo

package engine

import (
	"context"
	"testing"
)

// cleat#3428: durable-run-detached and durable-defer-phase linked to the
// cbTypeDefault fallback. These drive the dispatchers over a REAL execSession,
// not mockHostHandler (which records the call and writes nothing), so the
// assertion is on what the session did.

const (
	dispatchRunDetachedMethod = 26
	dispatchDeferPhaseMethod  = 27
)

func TestDispatchRunDetachedStartsTheWorkflowAndRecordsIt(t *testing.T) {
	type start struct{ name, input string }
	var starts []start
	s := newTestExecSession()
	s.engine.childWfStore = &mockChildStore{
		startChildFn: func(_ context.Context, _ /*parent*/, name, input string, _ int, _ string, _ int) (string, error) {
			starts = append(starts, start{name, input})
			return "detached-run-1", nil
		},
	}
	b := &wasmtimeBackend{handler: s}

	argsPtr, n, free := cgotestMakeStrArgs("child-wf", `{"k":1}`)
	defer free()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(dispatchRunDetachedMethod, argsPtr, n, resultPtr); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(starts) != 1 || starts[0] != (start{"child-wf", `{"k":1}`}) {
		t.Fatalf("the child store saw %v, want exactly one start of child-wf with {\"k\":1} -- the fallback "+
			"writes a u64 0 and calls nothing", starts)
	}
	if got := cgotestReadResultU64(resultPtr); got != 0 {
		t.Errorf("result = %#x, want 0 (success)", got)
	}
	if len(s.history) != 1 || s.history[0].EventType != EventTypeRunDetached {
		t.Errorf("history = %+v, want one run_detached event", s.history)
	}
}

// The stop sentinel is a u64 on this call, so it must come through untouched:
// the Python SDK's _raise_if_stopped is what turns it into a stop, and the
// stop-correspondence guard names run_detached as covered by exactly that.
func TestDispatchRunDetachedPassesTheStopSentinelThrough(t *testing.T) {
	s := newTestExecSession()
	s.engine.deferPhase = true // a defer segment: stopBeforeNewWork refuses new work
	s.engine.childWfStore = &mockChildStore{
		startChildFn: func(context.Context, string, string, string, int, string, int) (string, error) {
			t.Error("a defer segment started a detached run; it must be refused")
			return "x", nil
		},
	}
	b := &wasmtimeBackend{handler: s}

	argsPtr, n, free := cgotestMakeStrArgs("child-wf", "{}")
	defer free()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(dispatchRunDetachedMethod, argsPtr, n, resultPtr); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != uint64(callSuspendSentinel) {
		t.Errorf("result = %#x, want the stop sentinel %#x", got, uint64(callSuspendSentinel))
	}
}

func TestDispatchDeferPhaseSetsAndClearsTheSessionsMark(t *testing.T) {
	s := newTestExecSession()
	b := &wasmtimeBackend{handler: s}
	resultPtr := cgotestAllocResult()

	for _, on := range []bool{true, false, true, false} {
		argsPtr, n, free := cgotestMakeMixedArgs(on)
		if err := b.cgotestDispatchU64(dispatchDeferPhaseMethod, argsPtr, n, resultPtr); err != nil {
			free()
			t.Fatalf("dispatch(%v): %v", on, err)
		}
		free()
		s.mu.Lock()
		got := s.inDeferPhase
		s.mu.Unlock()
		if got != on {
			t.Fatalf("after defer_phase(%v) the session's inDeferPhase = %v", on, got)
		}
	}
}
