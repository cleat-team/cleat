package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#2285: once the worker's shutdown signal fires, a run that is being cut off must not START anything.
// The case that matters is a guest that compensates on failure: told a call failed, it runs its compensation,
// and the compensation is a fresh call with a real side effect for a fault that never happened. Every host call
// that starts fresh work asks stopBeforeNewWork, so that is where shutdown is refused.

func shutdownSession(signal <-chan struct{}, caller *mockCaller) *execSession {
	eng := NewEngine(nil, caller, WithWorkflowID("wf-1"), WithShutdownSignal(signal))
	return &execSession{
		engine: eng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
	}
}

func freshDurableCall(s *execSession) int64 {
	buf := make([]byte, 4096)
	c := contextWithRawMemBuf(context.Background(), buf)
	return s.DurableCall(c, nil, "svc", "op", `{}`, 0, 4000)
}

func TestAFreshCallAfterShutdownIsRefusedWithTheStopSentinel(t *testing.T) {
	sig := make(chan struct{})
	caller := &mockCaller{}
	s := shutdownSession(sig, caller)

	// Control: with the signal not fired the call is made, so the refusal below is the signal's doing.
	if got := freshDurableCall(s); got == callSuspendSentinel {
		t.Fatal("a call was refused with no shutdown signalled, so this test cannot tell the signal from anything else")
	}
	if len(caller.calls) != 1 {
		t.Fatalf("control: %d calls reached the service, want 1", len(caller.calls))
	}

	close(sig)
	if got := freshDurableCall(s); got != callSuspendSentinel {
		t.Errorf("a fresh call after shutdown returned %#x, want the stop sentinel %#x", got, callSuspendSentinel)
	}
	if len(caller.calls) != 1 {
		t.Errorf("%d calls reached the service, want 1: a call started after shutdown", len(caller.calls))
	}
}

func TestShutdownObservedIsFalseForAnEngineWithNoWorker(t *testing.T) {
	// nil channel: cleatctl replay, cleat run_embedded and wasmtest have no worker to shut down.
	if NewEngine(nil, &mockCaller{}).shutdownObserved() {
		t.Error("an engine with no shutdown signal reported shutdown")
	}
}
