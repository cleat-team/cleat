package engine

import (
	"context"
	"sync/atomic"
	"testing"
)

// blockingCaller blocks until its context is cancelled, which is what a
// long-running call subject to heartbeats looks like from the engine's side.
type blockingCaller struct {
	calls atomic.Int32
}

func (c *blockingCaller) Call(ctx context.Context, _, _, _ string) (string, error) {
	c.calls.Add(1)
	<-ctx.Done()
	return "", ctx.Err()
}

// TestDurableCallWithHeartbeat_CancelledMidCallIsNotRetryable pins the one
// behaviour freshCall gets right and freshCallWithHeartbeat does not.
//
// Both paths detect cancellation. freshCall reports it as callErrorUnknown,
// with a comment that says why: "Not retryable: the workflow was cancelled, so
// repeating the call is the one thing the caller must not do." callerrors.go
// agrees, naming "a cancelled workflow" as the first of the three canonical
// non-retryable cases.
//
// freshCallWithHeartbeat cancels the call's context and then falls through to
// its generic error branch, which returns callFailureCode -- callErrorUnavailable,
// documented as "Retryable". So a workflow cancelled during a heartbeat call is
// told the call is worth trying again, which is the opposite of what
// cancellation means, and a guest branching on Retryable() will re-issue the
// call it was just cancelled out of.
//
// The recorded event is the durable half of the same defect. It carries
// whatever string the cancelled context produced -- "context canceled" -- with
// ErrNonRetryable unset, so replay reproduces a retryable generic failure
// rather than a cancellation. recordedFailureCode exists precisely so that
// fresh and replay cannot disagree about retryability; this path routes around
// it by never recording the classification in the first place.
func TestDurableCallWithHeartbeat_CancelledMidCallIsNotRetryable(t *testing.T) {
	caller := &blockingCaller{}
	store := &mockCancellationStore{cancelled: true, reason: "operator cancelled"}
	eng := NewEngine(nil, caller,
		WithSignalStore(store),
		WithWorkflowID("wf-heartbeat-cancel"),
	)
	s := &execSession{engine: eng}

	// A short interval so the first tick lands while the call is still
	// blocked. The call never returns on its own, so if cancellation is not
	// detected this test hangs rather than failing -- which is itself the
	// signal that the mechanism is gone.
	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"payments", "Charge", `{"amount":100}`, 10, 0, 0)

	if got := caller.calls.Load(); got != 1 {
		t.Fatalf("caller saw %d calls, want 1", got)
	}

	errFlag := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)

	if errFlag != 1 {
		t.Errorf("errFlag = %d, want 1: a cancelled call is a failed call", errFlag)
	}
	if callErrorCode != callErrorUnknown {
		t.Errorf("callErrorCode = %d, want %d (callErrorUnknown, non-retryable).\n\n"+
			"%d is callFailureCode/callErrorUnavailable, which callerrors.go "+
			"documents as retryable. A guest that branches on Retryable() will "+
			"re-issue the call the workflow was just cancelled out of -- the one "+
			"thing freshCall's cancellation path exists to prevent.",
			callErrorCode, callErrorUnknown, callFailureCode)
	}

	if len(s.history) == 0 {
		t.Fatal("no events recorded")
	}
	last := s.history[len(s.history)-1]
	if last.EventType != EventTypeCall {
		t.Fatalf("last event is %q, want %q", last.EventType, EventTypeCall)
	}
	if last.Err != cancelledCallError {
		t.Errorf("recorded Err = %q, want %q.\n\n"+
			"Replay reads the classification off the event. An event carrying "+
			"the raw context error replays as an ordinary retryable failure, so "+
			"the same step is non-retryable on the first run and retryable on "+
			"the replay of it.", last.Err, cancelledCallError)
	}
	if !last.ErrNonRetryable {
		t.Error("recorded ErrNonRetryable = false; recordedFailureCode will map " +
			"this back to callFailureCode on replay, so fresh and replay disagree " +
			"about whether a cancelled call may be retried")
	}
}

// TestDurableCallWithHeartbeat_NotCancelledStillSucceeds is the control. If the
// cancellation path above were reached unconditionally, every heartbeat call
// would report cancellation and the test above would pass for the wrong reason.
func TestDurableCallWithHeartbeat_NotCancelledStillSucceeds(t *testing.T) {
	caller := &mockCaller{}
	store := &mockCancellationStore{cancelled: false}
	eng := NewEngine(nil, caller,
		WithSignalStore(store),
		WithWorkflowID("wf-heartbeat-live"),
	)
	s := &execSession{engine: eng}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"payments", "Charge", `{"amount":100}`, 10, 0, 0)

	if errFlag := byte(result & 0xFF); errFlag != 0 {
		t.Errorf("errFlag = %d, want 0 for an uncancelled call", errFlag)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(s.history))
	}
	if s.history[0].Err != "" {
		t.Errorf("recorded Err = %q, want empty", s.history[0].Err)
	}
}

// TestDurableCallWithHeartbeat_PollErrorDoesNotCancel pins the deliberate half
// of the `pollErr == nil && cancelled` guard.
//
// A failing PollCancellation must not be read as "cancelled" -- that would let
// a database blip abort a call that is running normally. Failing open is the
// right default here because the poll repeats on every tick, so a transient
// error costs at most one interval of delay. The test exists because the guard
// is one character away from the opposite behaviour.
func TestDurableCallWithHeartbeat_PollErrorDoesNotCancel(t *testing.T) {
	caller := &mockCaller{}
	store := &mockCancellationStore{cancelled: true, err: context.DeadlineExceeded}
	eng := NewEngine(nil, caller,
		WithSignalStore(store),
		WithWorkflowID("wf-heartbeat-pollerr"),
	)
	s := &execSession{engine: eng}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"payments", "Charge", `{"amount":100}`, 10, 0, 0)

	if errFlag := byte(result & 0xFF); errFlag != 0 {
		t.Errorf("errFlag = %d, want 0: a PollCancellation error must not be "+
			"treated as a cancellation", errFlag)
	}
}
