package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
)

// ---------------------------------------------------------------------------
// mockFetcher implements the Fetcher interface for Fetch tests.
// ---------------------------------------------------------------------------

type mockFetcher struct {
	response string
	err      error
}

func (f *mockFetcher) Fetch(ctx context.Context, method, url, headersJSON, body string) (string, error) {
	return f.response, f.err
}

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// DurableCall tests.
// ---------------------------------------------------------------------------

func TestDurableCall_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeCall,
		Service: "my-svc", Op: "my-op",
		Request: `{"key":"val"}`, Response: `{"result":"ok"}`,
	}}

	result := s.DurableCall(context.Background(), nil, "my-svc", "my-op", `{"key":"val"}`, 0, 0)

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestDurableCall_ReplayCachedError(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeCall,
		Service: "my-svc", Op: "my-op",
		Request: `{"key":"val"}`, Err: "service unavailable",
	}}

	result := s.DurableCall(context.Background(), nil, "my-svc", "my-op", `{"key":"val"}`, 0, 0)

	errCode := byte(result & 0xFF)
	callErrorCode := byte((result >> 8) & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if callErrorCode != callFailureCode {
		t.Errorf("callErrorCode = %d, want %d -- a service failure the engine cannot classify further", callErrorCode, callFailureCode)
	}
}

func TestDurableCall_ReplayMismatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeDefer, // wrong type
	}}

	result := s.DurableCall(context.Background(), nil, "my-svc", "my-op", `{}`, 0, 0)

	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (divergence), got %d", errCode)
	}
}

func TestDurableCall_ReplayPastEnd(t *testing.T) {
	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.caller = caller
	s.isReplay = true
	s.history = nil

	result := s.DurableCall(context.Background(), nil, "my-svc", "my-op", `{"key":"val"}`, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if !s.replayJustEnded {
		t.Error("expected replayJustEnded=true")
	}
	if len(caller.calls) != 1 {
		t.Errorf("expected 1 real call, got %d", len(caller.calls))
	}
	_ = result
}

func TestDurableCall_Fresh(t *testing.T) {
	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.caller = caller

	result := s.DurableCall(context.Background(), nil, "my-svc", "my-op", `{"key":"val"}`, 0, 0)

	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(caller.calls))
	}
	if caller.calls[0].Service != "my-svc" || caller.calls[0].Op != "my-op" {
		t.Errorf("unexpected call: %+v", caller.calls[0])
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeCall {
		t.Errorf("expected EventTypeCall, got %q", s.history[0].EventType)
	}
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// DurableSleep tests.
// ---------------------------------------------------------------------------

func TestDurableSleep_FreshSuspends(t *testing.T) {
	s := newTestExecSession()

	result := s.DurableSleep(context.Background(), nil, 5000)

	status := byte(result >> 56)
	if status != sleepStatusSuspend {
		t.Errorf("expected sleepStatusSuspend (%d), got %d", sleepStatusSuspend, status)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr for fresh sleep")
	}
	if !strings.Contains(s.suspendErr.Reason, "cleat_sleep") {
		t.Errorf("expected 'cleat_sleep' in reason, got %q", s.suspendErr.Reason)
	}
}

func TestDurableSleep_ReplayJustEnded(t *testing.T) {
	s := newTestExecSession()
	s.replayJustEnded = true
	s.history = []EventRecord{{Step: 0, EventType: EventTypeCall}}

	result := s.DurableSleep(context.Background(), nil, 5000)

	status := byte(result >> 56)
	if status != sleepStatusCompleted {
		t.Errorf("expected sleepStatusCompleted (%d), got %d", sleepStatusCompleted, status)
	}
	if s.replayJustEnded {
		t.Error("expected replayJustEnded=false after consuming it")
	}
}

func TestDurableSleep_ZeroDuration(t *testing.T) {
	s := newTestExecSession()

	result := s.DurableSleep(context.Background(), nil, 0)

	status := byte(result >> 56)
	if status != sleepStatusSuspend {
		t.Errorf("expected sleepStatusSuspend for zero duration, got %d", status)
	}
	if s.suspendErr == nil {
		t.Error("expected suspendErr for zero duration sleep")
	}
}

// ---------------------------------------------------------------------------
// DurableCallWithRetry tests.
// ---------------------------------------------------------------------------

func TestDurableCallWithRetry_Replay(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeCall,
		Service: "my-svc", Op: "my-op",
		Request: `{"key":"val"}`, Response: `{"ok":true}`,
	}}

	result := s.DurableCallWithRetry(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 3, 100, 200, 1000, "", 0, 0)

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestDurableCallWithRetry_FreshSuccess(t *testing.T) {
	caller := &mockCaller{}
	s := newTestExecSession()
	s.engine.caller = caller

	result := s.DurableCallWithRetry(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 3, 100, 200, 1000, "", 0, 0)

	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(caller.calls))
	}
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

func TestDurableCallWithRetry_FreshRetriesExhausted(t *testing.T) {
	errCaller := &errorCaller{calls: 0, errMsg: "transient error"}
	s := newTestExecSession()
	s.engine.caller = errCaller

	result := s.DurableCallWithRetry(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 3, 1, 100, 10, "", 0, 0)

	if errCaller.calls != 3 {
		t.Errorf("expected 3 call attempts, got %d", errCaller.calls)
	}
	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (failure), got %d", errCode)
	}
}

func TestDurableCallWithRetry_FreshNonRetryable(t *testing.T) {
	errCaller := &errorCaller{calls: 0, errMsg: "NON_RETRYABLE: invalid input"}
	s := newTestExecSession()
	s.engine.caller = errCaller

	result := s.DurableCallWithRetry(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`, 3, 100, 200, 1000,
		`["NON_RETRYABLE"]`, 0, 0)

	if errCaller.calls != 1 {
		t.Errorf("expected 1 call (non-retryable stops immediately), got %d", errCaller.calls)
	}
	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// ContinueAsNew tests.
// ---------------------------------------------------------------------------

func TestContinueAsNew_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeContinueAsNew,
		NewInput: `{"restart":true}`,
	}}

	result := s.ContinueAsNew(context.Background(), nil, `{"restart":true}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if s.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", s.suspendErr.Reason)
	}
	if s.suspendErr.NewInput != `{"restart":true}` {
		t.Errorf("expected NewInput %q, got %q", `{"restart":true}`, s.suspendErr.NewInput)
	}
}

func TestContinueAsNew_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.ContinueAsNew(context.Background(), nil, `{"restart":true}`)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.suspendErr == nil || s.suspendErr.NewInput != `{"restart":true}` {
		t.Errorf("expected suspendErr with NewInput")
	}
}

func TestContinueAsNew_Fresh(t *testing.T) {
	s := newTestExecSession()

	result := s.ContinueAsNew(context.Background(), nil, `{"fresh":true}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeContinueAsNew {
		t.Errorf("expected EventTypeContinueAsNew, got %q", s.history[0].EventType)
	}
	if s.history[0].NewInput != `{"fresh":true}` {
		t.Errorf("expected NewInput %q, got %q", `{"fresh":true}`, s.history[0].NewInput)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr")
	}
	if s.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", s.suspendErr.Reason)
	}
}

// ---------------------------------------------------------------------------
// ContinueAsNewWithVersion tests.
// ---------------------------------------------------------------------------

func TestContinueAsNewWithVersion_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeContinueAsNew,
		NewInput: `{"restart":true}`, NewVersion: 5,
	}}

	result := s.ContinueAsNewWithVersion(context.Background(), nil, `{"restart":true}`, 5)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if s.suspendErr == nil || s.suspendErr.NewVersion != 5 {
		t.Errorf("expected NewVersion 5, got %d", s.suspendErr.NewVersion)
	}
}

func TestContinueAsNewWithVersion_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.ContinueAsNewWithVersion(context.Background(), nil, `{"restart":true}`, 7)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.suspendErr == nil || s.suspendErr.NewVersion != 7 {
		t.Errorf("expected NewVersion 7")
	}
}

func TestContinueAsNewWithVersion_Fresh(t *testing.T) {
	s := newTestExecSession()

	result := s.ContinueAsNewWithVersion(context.Background(), nil, `{"fresh":true}`, 3)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeContinueAsNew {
		t.Errorf("expected EventTypeContinueAsNew, got %q", s.history[0].EventType)
	}
	if s.history[0].NewVersion != 3 {
		t.Errorf("expected NewVersion 3, got %d", s.history[0].NewVersion)
	}
	if s.suspendErr == nil || s.suspendErr.NewVersion != 3 {
		t.Errorf("expected NewVersion 3 in suspendErr")
	}
}

// ---------------------------------------------------------------------------
// SideEffect tests.
// ---------------------------------------------------------------------------

func TestSideEffect_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeSideEffect,
		SideEffectResult: `{"computed":"result"}`,
	}}

	result := s.SideEffect(context.Background(), nil, `{"computed":"result"}`, 0, 0)

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestSideEffect_ReplayMismatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeSideEffect,
		SideEffectResult: `{"different":"value"}`,
	}}

	result := s.SideEffect(context.Background(), nil, `{"computed":"result"}`, 0, 0)

	errCode := byte(result)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (divergence), got %d", errCode)
	}
}

func TestSideEffect_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.SideEffect(context.Background(), nil, `{"computed":"result"}`, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

func TestSideEffect_Fresh(t *testing.T) {
	s := newTestExecSession()

	result := s.SideEffect(context.Background(), nil, `{"computed":"result"}`, 0, 0)

	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeSideEffect {
		t.Errorf("expected EventTypeSideEffect, got %q", s.history[0].EventType)
	}
	if s.history[0].SideEffectResult != `{"computed":"result"}` {
		t.Errorf("expected SideEffectResult %q, got %q", `{"computed":"result"}`, s.history[0].SideEffectResult)
	}
	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// SetState tests.
// ---------------------------------------------------------------------------

func TestSetState_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeStateMutation,
		StateKey: "my-key", StateValue: "my-val", StateOp: "set",
	}}

	result := s.SetState(context.Background(), nil, "my-key", "my-val")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if s.stateStore["my-key"] != "my-val" {
		t.Errorf("expected stateStore['my-key']='my-val', got %q", s.stateStore["my-key"])
	}
}

func TestSetState_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.SetState(context.Background(), nil, "my-key", "my-val")

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stateStore["my-key"] != "my-val" {
		t.Errorf("expected stateStore set")
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
}

func TestSetState_Fresh(t *testing.T) {
	s := newTestExecSession()

	result := s.SetState(context.Background(), nil, "my-key", "my-val")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stateStore["my-key"] != "my-val" {
		t.Errorf("expected stateStore['my-key']='my-val', got %q", s.stateStore["my-key"])
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].StateOp != "set" {
		t.Errorf("expected StateOp 'set', got %q", s.history[0].StateOp)
	}
}

// ---------------------------------------------------------------------------
// GetState tests.
// ---------------------------------------------------------------------------

func TestGetState_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeStateMutation,
		StateKey: "my-key", StateValue: "my-val", StateOp: "get",
	}}

	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.GetState(ctx, nil, "my-key", 0, uint32(len(buf)))

	// Replay path writes StateValue from history.
	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestGetState_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.stateStore["my-key"] = "replay-val"
	s.isReplay = true
	s.history = nil

	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.GetState(ctx, nil, "my-key", 0, uint32(len(buf)))

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

func TestGetState_Fresh(t *testing.T) {
	s := newTestExecSession()
	s.stateStore["my-key"] = "my-val"

	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.GetState(ctx, nil, "my-key", 0, uint32(len(buf)))

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].StateOp != "get" {
		t.Errorf("expected StateOp 'get', got %q", s.history[0].StateOp)
	}
}

func TestGetState_FreshMissingKey(t *testing.T) {
	s := newTestExecSession()
	// stateStore is empty.

	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.GetState(ctx, nil, "nonexistent", 0, uint32(len(buf)))

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	// Empty string written for missing key.
	written := uint32(result >> 32)
	if written != 0 {
		t.Errorf("expected written=0 for missing key, got %d", written)
	}
}

// ---------------------------------------------------------------------------
// DeleteState tests.
// ---------------------------------------------------------------------------

func TestDeleteState_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.stateStore["my-key"] = "my-val"
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeStateMutation,
		StateKey: "my-key", StateOp: "del",
	}}

	result := s.DeleteState(context.Background(), nil, "my-key")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if _, ok := s.stateStore["my-key"]; ok {
		t.Error("expected my-key to be deleted")
	}
}

func TestDeleteState_Fresh(t *testing.T) {
	s := newTestExecSession()
	s.stateStore["my-key"] = "my-val"

	result := s.DeleteState(context.Background(), nil, "my-key")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if _, ok := s.stateStore["my-key"]; ok {
		t.Error("expected my-key to be deleted")
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].StateOp != "del" {
		t.Errorf("expected StateOp 'del', got %q", s.history[0].StateOp)
	}
}

func TestDeleteState_FreshMissingKey(t *testing.T) {
	s := newTestExecSession()
	// Key doesn't exist — should not panic.

	result := s.DeleteState(context.Background(), nil, "nonexistent")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

// ---------------------------------------------------------------------------
// IncrState tests.
// ---------------------------------------------------------------------------

func TestIncrState_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeStateMutation,
		StateKey: "counter", StateValue: "5", StateDelta: 1, StateOp: "incr",
	}}

	result := s.IncrState(context.Background(), nil, "counter", 1)

	if result != 5 {
		t.Errorf("expected 5 (replayed value), got %d", result)
	}
	if s.stateStore["counter"] != "5" {
		t.Errorf("expected stateStore['counter']='5', got %q", s.stateStore["counter"])
	}
}

func TestIncrState_Fresh(t *testing.T) {
	s := newTestExecSession()

	result := s.IncrState(context.Background(), nil, "counter", 3)

	if result != 3 {
		t.Errorf("expected 3, got %d", result)
	}
	if s.stateStore["counter"] != "3" {
		t.Errorf("expected stateStore['counter']='3', got %q", s.stateStore["counter"])
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].StateOp != "incr" {
		t.Errorf("expected StateOp 'incr', got %q", s.history[0].StateOp)
	}
}

func TestIncrState_FreshExisting(t *testing.T) {
	s := newTestExecSession()
	s.stateStore["counter"] = "10"

	result := s.IncrState(context.Background(), nil, "counter", 5)

	if result != 15 {
		t.Errorf("expected 15, got %d", result)
	}
	if s.stateStore["counter"] != "15" {
		t.Errorf("expected stateStore['counter']='15', got %q", s.stateStore["counter"])
	}
}

func TestIncrState_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.IncrState(context.Background(), nil, "counter", 7)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != 7 {
		t.Errorf("expected 7, got %d", result)
	}
}

// ---------------------------------------------------------------------------
// HasState tests.
// ---------------------------------------------------------------------------

func TestHasState_ReplayMatchExists(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeStateMutation,
		StateKey: "my-key", StateValue: "1", StateOp: "has",
	}}

	result := s.HasState(context.Background(), nil, "my-key")

	if result != 1 {
		t.Errorf("expected 1 (exists), got %d", result)
	}
}

func TestHasState_ReplayMatchNotExists(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeStateMutation,
		StateKey: "my-key", StateValue: "0", StateOp: "has",
	}}

	result := s.HasState(context.Background(), nil, "my-key")

	if result != 0 {
		t.Errorf("expected 0 (not exists), got %d", result)
	}
}

func TestHasState_FreshExists(t *testing.T) {
	s := newTestExecSession()
	s.stateStore["my-key"] = "my-val"

	result := s.HasState(context.Background(), nil, "my-key")

	if result != 1 {
		t.Errorf("expected 1 (exists), got %d", result)
	}
}

func TestHasState_FreshNotExists(t *testing.T) {
	s := newTestExecSession()

	result := s.HasState(context.Background(), nil, "nonexistent")

	if result != 0 {
		t.Errorf("expected 0 (not exists), got %d", result)
	}
}

// ---------------------------------------------------------------------------
// ListState tests.
// ---------------------------------------------------------------------------

func TestListState_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeStateMutation,
		StateKey: "prefix-", StateKeys: `["prefix-a","prefix-b"]`, StateOp: "list",
	}}

	buf := make([]byte, 128)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.ListState(ctx, nil, "prefix-", 0, uint32(len(buf)))

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

func TestListState_Fresh(t *testing.T) {
	s := newTestExecSession()
	s.stateStore["prefix-a"] = "1"
	s.stateStore["prefix-b"] = "2"
	s.stateStore["other"] = "3"

	buf := make([]byte, 128)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.ListState(ctx, nil, "prefix-", 0, uint32(len(buf)))

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}

	written := uint32(result >> 32)
	var keys []string
	if err := json.Unmarshal(buf[:written], &keys); err != nil {
		t.Fatalf("unmarshal keys: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("expected 2 keys, got %d: %v", len(keys), keys)
	}
}

func TestListState_FreshEmpty(t *testing.T) {
	s := newTestExecSession()
	// Empty stateStore.

	buf := make([]byte, 128)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.ListState(ctx, nil, "prefix-", 0, uint32(len(buf)))

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	written := uint32(result >> 32)
	if written <= 2 {
		t.Errorf("expected at least '[]', got %d bytes", written)
	}
}

// ---------------------------------------------------------------------------
// AwaitPromise tests.
// ---------------------------------------------------------------------------

func TestAwaitPromise_ReplayResolved(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePromiseResolved,
		PromiseID: "prom-1", PromiseResult: `{"status":"done"}`,
	}}

	result := s.AwaitPromise(context.Background(), nil, "prom-1", 5000, 0, 0)

	errCode := uint16(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestAwaitPromise_ReplayRejected(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePromiseRejected,
		PromiseID: "prom-1", PromiseError: "timed out",
	}}

	result := s.AwaitPromise(context.Background(), nil, "prom-1", 5000, 0, 0)

	errCode := uint16(result)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
}

func TestAwaitPromise_ReplayPendingThenExit(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	// EventTypeAwaitPromise means promise was pending in original execution.
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeAwaitPromise,
		PromiseID: "prom-1",
	}}

	result := s.AwaitPromise(context.Background(), nil, "prom-1", 5000, 0, 0)

	// Should exitReplay and check store. With no store, should suspend.
	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	// With no promiseStore, falls through to suspend.
	if s.suspendErr == nil {
		t.Error("expected suspendErr from pending await")
	}
	_ = result
}

func TestAwaitPromise_FreshResolved(t *testing.T) {
	promiseStore := &mockPromiseStore{
		status: "resolved",
		result: `{"status":"done"}`,
	}
	s := newTestExecSession()
	s.engine.promiseStore = promiseStore

	result := s.AwaitPromise(context.Background(), nil, "prom-1", 5000, 0, 0)

	errCode := uint16(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypePromiseResolved {
		t.Errorf("expected EventTypePromiseResolved, got %q", s.history[0].EventType)
	}
}

func TestAwaitPromise_FreshRejected(t *testing.T) {
	promiseStore := &mockPromiseStore{
		status: "rejected",
		errMsg: "payment failed",
	}
	s := newTestExecSession()
	s.engine.promiseStore = promiseStore

	result := s.AwaitPromise(context.Background(), nil, "prom-1", 5000, 0, 0)

	errCode := uint16(result)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if s.history[0].EventType != EventTypePromiseRejected {
		t.Errorf("expected EventTypePromiseRejected, got %q", s.history[0].EventType)
	}
}

func TestAwaitPromise_FreshPending(t *testing.T) {
	s := newTestExecSession()
	// No promiseStore → falls through to suspend.

	result := s.AwaitPromise(context.Background(), nil, "prom-1", 5000, 0, 0)

	// Should suspend (timeout flag = true).
	timeoutFlag := uint16((result >> 16) & 0xFFFF)
	if timeoutFlag != 1 {
		t.Errorf("expected timeout flag 1 for suspend, got %d", timeoutFlag)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr for pending promise")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_promise(prom-1)") {
		t.Errorf("expected 'await_promise(prom-1)' in reason, got %q", s.suspendErr.Reason)
	}
}

// ---------------------------------------------------------------------------
// ResolvePromise tests.
// ---------------------------------------------------------------------------

func TestResolvePromise_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePromiseResolved,
		PromiseID: "prom-1", PromiseResult: `{"status":"done"}`,
	}}

	result := s.ResolvePromise(context.Background(), nil, "prom-1", `{"status":"done"}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestResolvePromise_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.ResolvePromise(context.Background(), nil, "prom-1", `{"status":"done"}`)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestResolvePromise_Fresh(t *testing.T) {
	s := newTestExecSession()

	result := s.ResolvePromise(context.Background(), nil, "prom-1", `{"status":"done"}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypePromiseResolved {
		t.Errorf("expected EventTypePromiseResolved, got %q", s.history[0].EventType)
	}
	if s.history[0].PromiseResult != `{"status":"done"}` {
		t.Errorf("expected PromiseResult %q, got %q", `{"status":"done"}`, s.history[0].PromiseResult)
	}
}

// ---------------------------------------------------------------------------
// RejectPromise tests.
// ---------------------------------------------------------------------------

func TestRejectPromise_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypePromiseRejected,
		PromiseID: "prom-1", PromiseError: "error msg",
	}}

	result := s.RejectPromise(context.Background(), nil, "prom-1", "error msg")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestRejectPromise_Fresh(t *testing.T) {
	s := newTestExecSession()

	result := s.RejectPromise(context.Background(), nil, "prom-1", "error msg")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypePromiseRejected {
		t.Errorf("expected EventTypePromiseRejected, got %q", s.history[0].EventType)
	}
	if s.history[0].PromiseError != "error msg" {
		t.Errorf("expected PromiseError 'error msg', got %q", s.history[0].PromiseError)
	}
}

// ---------------------------------------------------------------------------
// RegisterUpdateHandler tests.
// ---------------------------------------------------------------------------

func TestRegisterUpdateHandler_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeUpdateHandler,
		UpdateHandlerName: "my-handler",
	}}

	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestRegisterUpdateHandler_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeUpdateHandler {
		t.Errorf("expected EventTypeUpdateHandler, got %q", s.history[0].EventType)
	}
}

func TestRegisterUpdateHandler_Fresh(t *testing.T) {
	s := newTestExecSession()

	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeUpdateHandler {
		t.Errorf("expected EventTypeUpdateHandler, got %q", s.history[0].EventType)
	}
	if s.history[0].UpdateHandlerName != "my-handler" {
		t.Errorf("expected UpdateHandlerName 'my-handler', got %q", s.history[0].UpdateHandlerName)
	}
}

// ---------------------------------------------------------------------------
// SignalWorkflow tests.
// ---------------------------------------------------------------------------

func TestSignalWorkflow_Fresh(t *testing.T) {
	store := newMockSignalWorkflowStore()
	s := newTestExecSession()
	s.engine.signalStore = store

	result := s.SignalWorkflow(context.Background(), nil, "target-wf", "my-signal", `{"msg":"hello"}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", s.history[0].EventType)
	}
	if s.history[0].SignalName != "my-signal" {
		t.Errorf("expected SignalName 'my-signal', got %q", s.history[0].SignalName)
	}
	// Verify signal was delivered to store.
	payload, found, err := store.PollSignal(context.Background(), "target-wf", "my-signal")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Error("expected signal to be found in store")
	}
	if payload != `{"msg":"hello"}` {
		t.Errorf("expected payload %q, got %q", `{"msg":"hello"}`, payload)
	}
}

func TestSignalWorkflow_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeSignalReceived,
		SignalName: "my-signal", SignalPayload: `{"msg":"hello"}`,
		RunID: "target-wf",
	}}

	result := s.SignalWorkflow(context.Background(), nil, "target-wf", "my-signal", `{"msg":"hello"}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestSignalWorkflow_ReplayPastEnd(t *testing.T) {
	store := newMockSignalWorkflowStore()
	s := newTestExecSession()
	s.engine.signalStore = store
	s.isReplay = true
	s.history = nil

	result := s.SignalWorkflow(context.Background(), nil, "target-wf", "my-signal", `{}`)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	// Fresh path should deliver signal.
	if store.deliverCount != 1 {
		t.Errorf("expected 1 delivery, got %d", store.deliverCount)
	}
}

// ---------------------------------------------------------------------------
// Fetch tests.
// ---------------------------------------------------------------------------

func TestFetch_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeFetch,
		FetchMethod: "GET", FetchURL: "http://example.com",
		FetchBody: "", FetchResponse: `{"status":"ok"}`,
	}}

	result := s.Fetch(context.Background(), nil, "GET", "http://example.com", "", "", 0, 0)

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestFetch_ReplayCachedError(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeFetch,
		FetchMethod: "GET", FetchURL: "http://example.com",
		FetchBody: "", Err: "connection failed",
	}}

	result := s.Fetch(context.Background(), nil, "GET", "http://example.com", "", "", 0, 0)

	errCode := byte(result)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
}

func TestFetch_ReplayMismatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeFetch,
		FetchMethod: "POST", FetchURL: "http://other.com",
		FetchBody: "", FetchResponse: `{}`,
	}}

	result := s.Fetch(context.Background(), nil, "GET", "http://example.com", "", "", 0, 0)

	errCode := byte(result)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (divergence), got %d", errCode)
	}
}

func TestFetch_Fresh(t *testing.T) {
	fetcher := &mockFetcher{response: `{"status":"ok"}`, err: nil}
	s := newTestExecSession()
	s.engine.fetcher = fetcher

	result := s.Fetch(context.Background(), nil, "GET", "http://example.com", "{}", "", 0, 0)

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeFetch {
		t.Errorf("expected EventTypeFetch, got %q", s.history[0].EventType)
	}
	if s.history[0].FetchResponse != `{"status":"ok"}` {
		t.Errorf("expected FetchResponse %q, got %q", `{"status":"ok"}`, s.history[0].FetchResponse)
	}
}

func TestFetch_FreshError(t *testing.T) {
	fetcher := &mockFetcher{response: "", err: fmt.Errorf("network error")}
	s := newTestExecSession()
	s.engine.fetcher = fetcher

	result := s.Fetch(context.Background(), nil, "GET", "http://example.com", "", "", 0, 0)

	errCode := byte(result)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if s.history[0].Err != "network error" {
		t.Errorf("expected Err 'network error', got %q", s.history[0].Err)
	}
}

func TestFetch_FreshNoFetcher(t *testing.T) {
	s := newTestExecSession()
	// engine.fetcher is nil

	result := s.Fetch(context.Background(), nil, "GET", "http://example.com", "", "", 0, 0)

	errCode := byte(result)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (no fetcher), got %d", errCode)
	}
	if s.history[0].Err == "" {
		t.Error("expected error message in history")
	}
}

// ---------------------------------------------------------------------------
// UUID format verification.
// ---------------------------------------------------------------------------

func TestUUID_Format(t *testing.T) {
	s := newTestExecSession()
	s.workflowID = "wf-uuid-format"

	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.UUID(ctx, nil, "seed", 0, uint32(len(buf)))

	written := uint32(result >> 32)
	if written == 0 {
		t.Fatal("expected non-zero written length")
	}
	uuidStr := string(buf[:written])
	// UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	if len(uuidStr) != 36 {
		t.Errorf("expected UUID length 36, got %d: %q", len(uuidStr), uuidStr)
	}
	// Check hyphens at positions 8, 13, 18, 23.
	if uuidStr[8] != '-' || uuidStr[13] != '-' || uuidStr[18] != '-' || uuidStr[23] != '-' {
		t.Errorf("invalid UUID format: %q", uuidStr)
	}
}

// ---------------------------------------------------------------------------
// SendSignalAndWait tests (fresh path).
// ---------------------------------------------------------------------------

func TestSendSignalAndWait_FreshSignalFound(t *testing.T) {
	store := newMockSignalWorkflowStore()
	ctx := context.Background()
	err := store.DeliverSignal(ctx, "target-wf", "my-signal", `{"response":"ok"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	s := newTestExecSession()
	s.engine.signalStore = store

	result := s.SendSignalAndWait(ctx, nil, "target-wf", "my-signal", `{"req":"hello"}`, 5000, 0, 0)

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", s.history[0].EventType)
	}
}

func TestSendSignalAndWait_FreshNoSignalSuspend(t *testing.T) {
	s := newTestExecSession()
	// No signal store → should suspend.

	result := s.SendSignalAndWait(context.Background(), nil, "target-wf", "my-signal", `{}`, 5000, 0, 0)

	errCode := byte(result)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (suspend), got %d", errCode)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr")
	}
}

func TestSendSignalAndWait_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeSignalReceived,
		SignalName: "my-signal", SignalPayload: `{"response":"ok"}`,
	}}

	result := s.SendSignalAndWait(context.Background(), nil, "target-wf", "my-signal", `{}`, 5000, 0, 0)

	errCode := byte(result)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestSendSignalAndWait_ReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil

	result := s.SendSignalAndWait(context.Background(), nil, "target-wf", "my-signal", `{}`, 5000, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	_ = result
}

func TestJsonParse(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	t.Cleanup(func() { rt.Close(ctx) })

	compMod, err := rt.CompileModule(ctx, minimalMemoryWasm())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cfg := wazero.NewModuleConfig().WithName("json-parse-test")
	mod, err := rt.InstantiateModule(ctx, compMod, cfg)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	tests := []struct {
		name         string
		input        string
		wantOK       bool
		wantContains string
	}{
		{"valid object", `{"a":1}`, true, `"a"`},
		{"valid array", `[1,2,3]`, true, `1`},
		{"invalid json", `{bad`, false, ""},
		{"empty string", ``, false, ""},
		{"nested", `{"b":{"c":"d"}}`, true, `"c"`},
	}

	s := newTestExecSession()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := mod.Memory()

			// The input is passed as a decoded string now; the wrapper reads
			// it out of guest memory (see engine/imports.go). The "invalid
			// json" and "empty string" cases still fail here, just at
			// json.Unmarshal rather than at the read.
			outPtr := uint32(4096) // offset in memory for output
			outMaxLen := uint32(4096)
			result := s.JsonParse(ctx, mod, tt.input, outPtr, outMaxLen)

			errCode := byte(result & 0xFF)
			if tt.wantOK && errCode != 0 {
				t.Errorf("expected success (code 0), got code %d", errCode)
			}
			if !tt.wantOK && errCode == 0 {
				t.Errorf("expected failure, got success")
			}

			if tt.wantOK {
				// Read back the result from output buffer
				out, ok := mem.Read(outPtr, outMaxLen)
				if !ok {
					t.Fatal("read output failed")
				}
				outStr := string(out)
				// Find the null terminator
				nullIdx := strings.IndexByte(outStr, 0)
				if nullIdx >= 0 {
					outStr = outStr[:nullIdx]
				}
				if !strings.Contains(outStr, tt.wantContains) {
					t.Errorf("output %q does not contain %q", outStr, tt.wantContains)
				}
			}
		})
	}
}
