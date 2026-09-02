package engine

import (
	"context"
	"fmt"
	"testing"
)

// errorCaller records call attempts and returns a configurable error.
type errorCaller struct {
	calls  int
	errMsg string
}

func (c *errorCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	c.calls++
	var err error
	if c.errMsg != "" {
		err = fmt.Errorf("%s", c.errMsg)
	}
	return "", err
}

// ---------------------------------------------------------------------------
// replayCallWithHeartbeat tests
// ---------------------------------------------------------------------------

func TestDurableCallWithHeartbeat_Replay_ConsumesHeartbeats(t *testing.T) {
	caller := &mockCaller{}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine:   engine,
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 1, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 2, EventType: EventTypeCall, Service: "my-svc", Op: "my-op", Request: `{"key":"val"}`, Response: `{"result":"ok"}`},
		},
		stepCount: 0,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	if s.stepCount != 3 {
		t.Errorf("stepCount = %d, want 3 (consumed 2 heartbeats + 1 call)", s.stepCount)
	}

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
	if callErrorCode != 0 {
		t.Errorf("callErrorCode = %d, want 0", callErrorCode)
	}
	if len(caller.calls) != 0 {
		t.Errorf("expected 0 real calls during replay, got %d", len(caller.calls))
	}
}

func TestDurableCallWithHeartbeat_Replay_ConsumesHeartbeats_EmptyHistory(t *testing.T) {
	caller := &mockCaller{}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine:   engine,
		isReplay: true,
		history:  nil,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay to be false after exiting replay")
	}
	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 real call, got %d", len(caller.calls))
	}
	if caller.calls[0].Service != "my-svc" {
		t.Errorf("service = %q, want %q", caller.calls[0].Service, "my-svc")
	}

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
}

func TestDurableCallWithHeartbeat_Replay_ConsumesHeartbeats_OnlyHeartbeats(t *testing.T) {
	caller := &mockCaller{}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine:   engine,
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 1, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
		},
		stepCount: 0,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay to be false after exiting replay")
	}
	// stepCount = 2 (consumed heartbeats) + 1 (fresh call event recorded)
	if s.stepCount != 3 {
		t.Errorf("stepCount = %d, want 3 (consumed 2 heartbeats + fresh call)", s.stepCount)
	}
	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 real call, got %d", len(caller.calls))
	}

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
}

func TestDurableCallWithHeartbeat_Replay_Divergence_WrongEventType(t *testing.T) {
	s := &execSession{
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 1, EventType: EventTypeDefer, Service: "my-svc", Op: "my-op"},
		},
		stepCount: 0,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("errCode = %d, want 1 (divergence)", errCode)
	}
	if callErrorCode != callErrorUnknown {
		t.Errorf("callErrorCode = %d, want %d -- a divergence is a bug in the workflow code, not something to retry", callErrorCode, callErrorUnknown)
	}
}

func TestDurableCallWithHeartbeat_Replay_Divergence_ServiceMismatch(t *testing.T) {
	s := &execSession{
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "other-svc", Op: "my-op",
				Request: `{"key":"val"}`, Response: `{}`},
		},
		stepCount: 0,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("errCode = %d, want 1 (divergence)", errCode)
	}
	if callErrorCode != callErrorUnknown {
		t.Errorf("callErrorCode = %d, want %d -- a divergence is a bug in the workflow code, not something to retry", callErrorCode, callErrorUnknown)
	}
}

func TestDurableCallWithHeartbeat_Replay_ErrorEvent(t *testing.T) {
	s := &execSession{
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 1, EventType: EventTypeCall, Service: "my-svc", Op: "my-op",
				Request: `{"key":"val"}`, Response: "", Err: "something went wrong", ErrNonRetryable: false},
		},
		stepCount: 0,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("errCode = %d, want 1 (cached error)", errCode)
	}
	if callErrorCode != callFailureCode {
		t.Errorf("callErrorCode = %d, want %d -- a recorded RETRYABLE service failure, classified the same as on the fresh path", callErrorCode, callFailureCode)
	}
	if s.stepCount != 2 {
		t.Errorf("stepCount = %d, want 2 (consumed heartbeat + call)", s.stepCount)
	}
}

// TestDurableCallWithHeartbeat_Replay_ErrorEvent_NonRetryable is the fixture
// TestDurableCallWithHeartbeat_Replay_ErrorEvent could not be: that test left
// ErrNonRetryable unset (the Go zero value, false), so it could not tell a
// correct classification from a broken one that always returns callFailureCode
// regardless of what was recorded. This test pins the other half: an event
// recorded non-retryable must replay non-retryable.
func TestDurableCallWithHeartbeat_Replay_ErrorEvent_NonRetryable(t *testing.T) {
	s := &execSession{
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 1, EventType: EventTypeCall, Service: "my-svc", Op: "my-op",
				Request: `{"key":"val"}`, Response: "", Err: "bad request", ErrNonRetryable: true},
		},
		stepCount: 0,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("errCode = %d, want 1 (cached error)", errCode)
	}
	if callErrorCode != callErrorUnknown {
		t.Errorf("callErrorCode = %d, want %d (non-retryable) -- freshCallWithHeartbeat recorded "+
			"ErrNonRetryable=true for this event, so replay must reproduce that classification "+
			"instead of guessing retryable", callErrorCode, callErrorUnknown)
	}
	if s.stepCount != 2 {
		t.Errorf("stepCount = %d, want 2 (consumed heartbeat + call)", s.stepCount)
	}
}

// TestFreshAndReplayAgreeOnNonRetryableFailure_Heartbeat is the determinism
// invariant for the heartbeat call path, mirroring
// TestFreshAndReplayAgreeOnNonRetryableFailure in callerrors_test.go for the
// plain DurableCall path. It replays the event freshCallWithHeartbeat actually
// recorded rather than a hand-written literal, so the writer and the reader
// cannot agree with themselves while disagreeing with each other.
func TestFreshAndReplayAgreeOnNonRetryableFailure_Heartbeat(t *testing.T) {
	// freshCallWithHeartbeat only marks ErrNonRetryable=true when the
	// heartbeat loop itself cancelled the call (see the cancelledByWorkflow
	// branch); an ordinary service failure always records false. Build the
	// event directly, as freshCallWithHeartbeat would have written it, for
	// both classes.
	for _, tc := range []struct {
		name            string
		errMsg          string
		errNonRetryable bool
		wantCode        byte
	}{
		{"retryable service failure", "connection reset", false, callFailureCode},
		{"cancelled by workflow", cancelledCallError, true, callErrorUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := EventRecord{
				Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op",
				Request: `{}`, Err: tc.errMsg, ErrNonRetryable: tc.errNonRetryable,
			}

			replay := &execSession{
				engine:    NewEngine(nil, &mockCaller{}),
				isReplay:  true,
				history:   []EventRecord{rec},
				stepCount: 0,
			}
			result := replay.DurableCallWithHeartbeat(context.Background(), nil,
				"svc", "op", `{}`, 60000, 0, 0)

			callErrorCode := byte((result >> 8) & 0xFF)
			if callErrorCode != tc.wantCode {
				t.Errorf("callErrorCode = %d, want %d -- replay must reproduce the classification "+
					"recorded on the event (ErrNonRetryable=%v)", callErrorCode, tc.wantCode, tc.errNonRetryable)
			}
		})
	}
}

func TestDurableCallWithHeartbeat_Replay_PendingIntent(t *testing.T) {
	s := &execSession{
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 1, EventType: EventTypeCall, Service: "my-svc", Op: "my-op",
				Request: `{"key":"val"}`, Pending: true},
		},
		stepCount: 0,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("errCode = %d, want 1 (ambiguous)", errCode)
	}
	if callErrorCode != callErrorUnknown {
		t.Errorf("callErrorCode = %d, want %d -- ambiguous: the call may have succeeded, so retrying risks a duplicate", callErrorCode, callErrorUnknown)
	}
}

// ---------------------------------------------------------------------------
// freshCallWithHeartbeat tests
// ---------------------------------------------------------------------------

func TestDurableCallWithHeartbeat_Fresh_Success(t *testing.T) {
	caller := &mockCaller{}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine: engine,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 real call, got %d", len(caller.calls))
	}
	if caller.calls[0].Service != "my-svc" {
		t.Errorf("service = %q, want %q", caller.calls[0].Service, "my-svc")
	}
	if caller.calls[0].Op != "my-op" {
		t.Errorf("op = %q, want %q", caller.calls[0].Op, "my-op")
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeCall {
		t.Errorf("event type = %q, want %q", s.history[0].EventType, EventTypeCall)
	}
	if s.history[0].Response == "" {
		t.Error("expected non-empty response")
	}

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
}

func TestDurableCallWithHeartbeat_Fresh_Error(t *testing.T) {
	caller := &errorCaller{errMsg: "call failed"}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine: engine,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	if caller.calls != 1 {
		t.Fatalf("expected 1 call attempt, got %d", caller.calls)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeCall {
		t.Errorf("event type = %q, want %q", s.history[0].EventType, EventTypeCall)
	}
	if s.history[0].Err != "call failed" {
		t.Errorf("event error = %q, want %q", s.history[0].Err, "call failed")
	}

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("errCode = %d, want 1", errCode)
	}
	if callErrorCode != callFailureCode {
		t.Errorf("callErrorCode = %d, want %d -- a service failure the engine cannot classify further", callErrorCode, callFailureCode)
	}
}

func TestDurableCallWithHeartbeat_Fresh_RecordedHistory(t *testing.T) {
	caller := &mockCaller{}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine: engine,
	}

	_ = s.DurableCallWithHeartbeat(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 60000, 0, 0)

	// History should contain a call event with correct service/op.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].Service != "my-svc" {
		t.Errorf("service = %q, want %q", s.history[0].Service, "my-svc")
	}
	if s.history[0].Op != "my-op" {
		t.Errorf("op = %q, want %q", s.history[0].Op, "my-op")
	}
	if s.history[0].Request != `{"key":"val"}` {
		t.Errorf("request = %q, want %q", s.history[0].Request, `{"key":"val"}`)
	}
}
