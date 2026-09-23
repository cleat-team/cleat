package engine

import (
	"context"
	"testing"
)

// cleat#2008 decision 2: WithCanStartNewWork gates every FRESH durable call
// on a worker-supplied predicate, in addition to (not instead of) the
// cancellation poll -- see the identical-shape TestCancellationObservedEndToEnd
// in host_dispatch_test.go, which this mirrors. Unlike cancellation, a
// refusal here is retryable: nothing about this specific run is known wrong,
// only that the worker calling canStartNewWork cannot currently vouch for it.

// TestFreshCallRefusedWhenCanStartNewWorkReportsFalse proves the gate: the
// real service is never invoked, and the packed result carries the retryable
// callFailureCode classification rather than a cancellation's callErrorUnknown.
func TestFreshCallRefusedWhenCanStartNewWorkReportsFalse(t *testing.T) {
	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.caller = caller
	s.engine.canStartNewWork = func() bool { return false }

	buf := make([]byte, 256)
	memCtx := contextWithRawMemBuf(context.Background(), buf)

	result := s.DurableCall(memCtx, nil, "my-svc", "my-op", `{"key":"val"}`, 0, uint32(len(buf)))

	if len(caller.calls) != 0 {
		t.Errorf("expected 0 calls to the real service, got %d: %+v", len(caller.calls), caller.calls)
	}

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 || callErrorCode != callFailureCode {
		t.Fatalf("expected a retryable refusal (errCode=1, callErrorCode=%d), got errCode=%d callErrorCode=%d (raw=%d)",
			callFailureCode, errCode, callErrorCode, result)
	}
	respLen := uint32(result >> 40)
	written := string(buf[:respLen])
	if written != heartbeatPresumedLostCallError {
		t.Errorf("expected response %q, got %q", heartbeatPresumedLostCallError, written)
	}
}

// TestFreshCallProceedsWhenCanStartNewWorkReportsTrue is the negative
// control: the gate must not refuse a call it has no reason to.
func TestFreshCallProceedsWhenCanStartNewWorkReportsTrue(t *testing.T) {
	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.caller = caller
	s.engine.canStartNewWork = func() bool { return true }

	result := s.DurableCall(context.Background(), nil, "my-svc", "my-op", `{"key":"val"}`, 0, 0)

	if len(caller.calls) != 1 {
		t.Errorf("expected the call to proceed, got %d calls", len(caller.calls))
	}
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected no refusal when canStartNewWork reports true, got errCode=%d", errCode)
	}
}

// TestFreshCallProceedsWhenCanStartNewWorkIsNil is the fail-open default:
// every caller of NewEngine that has no worker and no heartbeat to lose
// (cleatctl replay, cleat run_embedded, wasmtest) must see unchanged
// behaviour.
func TestFreshCallProceedsWhenCanStartNewWorkIsNil(t *testing.T) {
	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.caller = caller
	s.engine.canStartNewWork = nil

	result := s.DurableCall(context.Background(), nil, "my-svc", "my-op", `{"key":"val"}`, 0, 0)

	if len(caller.calls) != 1 {
		t.Errorf("expected the call to proceed with canStartNewWork unset, got %d calls", len(caller.calls))
	}
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected no refusal with canStartNewWork unset, got errCode=%d", errCode)
	}
}

// TestCancellationIsCheckedBeforeCanStartNewWork proves the ordering
// comment in freshCall: a cancelled workflow must report cancelled
// (non-retryable), not the presumed-lost refusal, even when both conditions
// are true at once. A guest branching on CallError.Retryable() must see the
// cancellation, not a transient-looking retryable failure it could loop on.
func TestCancellationIsCheckedBeforeCanStartNewWork(t *testing.T) {
	store := &keyedCancellationStore{}
	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.workflowID = "wf-both"
	s.engine.caller = caller
	s.engine.signalStore = store
	s.engine.canStartNewWork = func() bool { return false }

	if err := store.RequestCancellation(context.Background(), "wf-both", "operator requested stop"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	buf := make([]byte, 256)
	memCtx := contextWithRawMemBuf(context.Background(), buf)
	result := s.DurableCall(memCtx, nil, "my-svc", "my-op", `{"key":"val"}`, 0, uint32(len(buf)))

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 || callErrorCode != callErrorUnknown {
		t.Fatalf("expected cancellation to win (errCode=1, callErrorCode=%d), got errCode=%d callErrorCode=%d",
			callErrorUnknown, errCode, callErrorCode)
	}
}
