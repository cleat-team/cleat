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
	if callErrorCode != 1 {
		t.Errorf("callErrorCode = %d, want 1 (divergence)", callErrorCode)
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
	if callErrorCode != 1 {
		t.Errorf("callErrorCode = %d, want 1 (divergence)", callErrorCode)
	}
}

func TestDurableCallWithHeartbeat_Replay_ErrorEvent(t *testing.T) {
	s := &execSession{
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 1, EventType: EventTypeCall, Service: "my-svc", Op: "my-op",
				Request: `{"key":"val"}`, Response: "", Err: "something went wrong"},
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
	if callErrorCode != 1 {
		t.Errorf("callErrorCode = %d, want 1 (cached error)", callErrorCode)
	}
	if s.stepCount != 2 {
		t.Errorf("stepCount = %d, want 2 (consumed heartbeat + call)", s.stepCount)
	}
}

func TestDurableCallWithHeartbeat_Replay_PendingSentinel(t *testing.T) {
	s := &execSession{
		isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "my-svc", Op: "my-op"},
			{Step: 1, EventType: EventTypeCall, Service: "my-svc", Op: "my-op",
				Request: `{"key":"val"}`, Err: pendingSentinel},
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
	if callErrorCode != 1 {
		t.Errorf("callErrorCode = %d, want 1 (ambiguous)", callErrorCode)
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
	if callErrorCode != 1 {
		t.Errorf("callErrorCode = %d, want 1", callErrorCode)
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
