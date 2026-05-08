package localdev

import (
	"context"
	"strings"
	"testing"
)
// =========================================================================
// Helpers
// =========================================================================

type stubCaller struct {
	fn func(ctx context.Context, svc, op, req string) (string, error)
}

func (s *stubCaller) Call(ctx context.Context, svc, op, req string) (string, error) {
	return s.fn(ctx, svc, op, req)
}

var _ ServiceCaller = (*stubCaller)(nil)

// =========================================================================
// Construction and options
// =========================================================================

func TestLR_New_CreatesWithDefaults(t *testing.T) {
	r := NewLocalRunner()
	if r == nil {
		t.Fatal("NewLocalRunner returned nil")
	}
	if r.H() == nil {
		t.Error("H() returned nil")
	}
	if r.Events() == nil {
		t.Error("Events() returned nil")
	}
}

func TestLR_New_WithWorkflowID(t *testing.T) {
	r := NewLocalRunner(WithWorkflowID("my-wf-id"))
	if r.workflowID != "my-wf-id" {
		t.Errorf("workflowID: want my-wf-id, got %q", r.workflowID)
	}
}

func TestLR_New_WithServiceCaller(t *testing.T) {
	called := false
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		called = true
		return "ok", nil
	}}
	r := NewLocalRunner(WithServiceCaller(caller))
	resp, err := r.durableCall("test-svc", "test-op", "{}")
	if err != nil {
		t.Fatalf("durableCall: %v", err)
	}
	if resp != "ok" {
		t.Errorf("want ok, got %q", resp)
	}
	if !called {
		t.Error("expected caller to be invoked")
	}
}

func TestLR_New_WithLogWriter(t *testing.T) {
	var buf strings.Builder
	r := NewLocalRunner(WithLogWriter(&buf))
	r.durableLog("test message")
	if !strings.Contains(buf.String(), "test message") {
		t.Errorf("expected log message, got %q", buf.String())
	}
}

// =========================================================================
// Events recording
// =========================================================================

func TestLR_Events_RecordsDurableCall(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return "resp", nil
	}}
	r := NewLocalRunner(WithServiceCaller(caller))
	_, err := r.durableCall("mysvc", "myop", `{"a":1}`)
	if err != nil {
		t.Fatalf("durableCall: %v", err)
	}
	events := r.Events()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Type != "call" || events[0].Service != "mysvc" || events[0].Operation != "myop" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

func TestLR_Events_NoEventOnCallerNil(t *testing.T) {
	r := NewLocalRunner()
	_, err := r.durableCall("svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error without ServiceCaller")
	}
	// When caller is nil, no event is recorded (call fails before recording).
	events := r.Events()
	if len(events) != 0 {
		t.Errorf("expected no events when caller is nil, got %d", len(events))
	}
}

// =========================================================================
// DurableLog, elapsed
// =========================================================================

func TestLR_DurableLog(t *testing.T) {
	var buf strings.Builder
	r := NewLocalRunner(WithLogWriter(&buf))
	r.durableLog("hello world")
	if !strings.Contains(buf.String(), "hello world") {
		t.Errorf("log missing message: %q", buf.String())
	}
}

func TestLR_Elapsed(t *testing.T) {
	r := NewLocalRunner()
	d := r.elapsed()
	if d < 0 {
		t.Errorf("elapsed should be non-negative, got %v", d)
	}
}

// =========================================================================
// Cancellation
// =========================================================================

func TestLR_Cancellation_InitiallyNotCancelled(t *testing.T) {
	r := NewLocalRunner()
	cancelled, _ := r.pollCancellation()
	if cancelled {
		t.Error("expected not cancelled initially")
	}
}

func TestLR_Cancellation_AfterCancel(t *testing.T) {
	r := NewLocalRunner()
	r.Cancel("test reason")
	cancelled, reason := r.pollCancellation()
	if !cancelled {
		t.Error("expected cancelled after Cancel")
	}
	if reason != "test reason" {
		t.Errorf("want 'test reason', got %q", reason)
	}
}

// =========================================================================
// Signals
// =========================================================================

func TestLR_SendAndPollSignal(t *testing.T) {
	r := NewLocalRunner()
	r.SendSignal("my-signal", `{"key":"value"}`)
	payload, ok, err := r.pollSignal("my-signal")
	if err != nil {
		t.Fatalf("pollSignal: %v", err)
	}
	if !ok {
		t.Fatal("expected signal to be present")
	}
	if payload != `{"key":"value"}` {
		t.Errorf("want payload, got %q", payload)
	}
}

func TestLR_PollSignal_NotPresent(t *testing.T) {
	r := NewLocalRunner()
	_, ok, err := r.pollSignal("nonexistent")
	if err != nil {
		t.Fatalf("pollSignal: %v", err)
	}
	if ok {
		t.Error("expected signal not present")
	}
}

func TestLR_PollSignal_Consumed(t *testing.T) {
	r := NewLocalRunner()
	r.SendSignal("sig1", "first")
	_, ok, _ := r.pollSignal("sig1")
	if !ok {
		t.Fatal("signal should be present")
	}
	_, ok, _ = r.pollSignal("sig1")
	if ok {
		t.Error("signal should be consumed after poll")
	}
}

// =========================================================================
// ContinueAsNew
// =========================================================================

func TestLR_ContinueAsNew_InitiallyEmpty(t *testing.T) {
	r := NewLocalRunner()
	input, ok := r.ContinueAsNewInput()
	if ok || input != "" {
		t.Errorf("expected no continue-as-new input, got ok=%v input=%q", ok, input)
	}
}

func TestLR_ContinueAsNew_AfterSet(t *testing.T) {
	r := NewLocalRunner()
	r.continueAsNew(`{"new":"input"}`)
	input, ok := r.ContinueAsNewInput()
	if !ok {
		t.Error("expected continue-as-new input present")
	}
	if input != `{"new":"input"}` {
		t.Errorf("want new input, got %q", input)
	}
}

// =========================================================================
// version / minVersion / nowMs / random
// =========================================================================

func TestLR_Version(t *testing.T) {
	r := NewLocalRunner()
	if r.version() <= 0 {
		t.Errorf("version should be > 0, got %d", r.version())
	}
}

func TestLR_MinVersion(t *testing.T) {
	r := NewLocalRunner()
	if r.minVersion() <= 0 {
		t.Errorf("minVersion should be > 0, got %d", r.minVersion())
	}
}

func TestLR_NowMs(t *testing.T) {
	r := NewLocalRunner()
	if r.nowMs() <= 0 {
		t.Errorf("nowMs should be > 0")
	}
}

func TestLR_Random(t *testing.T) {
	r := NewLocalRunner()
	r1 := r.random()
	r2 := r.random()
	if r1 == r2 && r1 == 0 {
		t.Logf("random returned same value twice (%d), may be flaky", r1)
	}
}

// =========================================================================
// Concurrency keys
// =========================================================================

func TestLR_AcquireConcurrencyKey_Success(t *testing.T) {
	r := NewLocalRunner()
	acquired, err := r.AcquireConcurrencyKey("my-lock", "test-wf", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Error("expected first acquisition to succeed")
	}
}

func TestLR_AcquireConcurrencyKey_DuplicateFails(t *testing.T) {
	r := NewLocalRunner()
	r.AcquireConcurrencyKey("my-lock", "wf-1", 0)
	// Same workflow re-acquiring same key is OK (idempotent).
	acquiredSame, _ := r.AcquireConcurrencyKey("my-lock", "wf-1", 0)
	if !acquiredSame {
		t.Error("re-acquire by same workflow should succeed (idempotent)")
	}
	// Different workflow trying same key should fail.
	acquiredDiff, err := r.AcquireConcurrencyKey("my-lock", "wf-2", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquiredDiff {
		t.Error("different workflow should not acquire already-held key")
	}
}

func TestLR_ReleaseConcurrencyKeys(t *testing.T) {
	r := NewLocalRunner()
	_, _ = r.AcquireConcurrencyKey("lock-a", "test-wf", 0)
	_, _ = r.AcquireConcurrencyKey("lock-b", "test-wf", 0)
	r.ReleaseConcurrencyKeys("test-wf")

	acquired, err := r.AcquireConcurrencyKey("lock-a", "test-wf2", 0)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if !acquired {
		t.Error("expected re-acquisition to succeed after release")
	}
}
