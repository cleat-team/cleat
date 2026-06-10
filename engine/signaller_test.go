package engine

import (
	"context"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// DurableAwaitSignals tests
// ---------------------------------------------------------------------------

func TestDurableAwaitSignals_ReplayWithSignalReceived(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeSignalReceived,
		SignalName: "my-signal", SignalPayload: `{"msg":"hello"}`,
	}}

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.DurableAwaitSignals(ctx, nil, `["my-signal"]`, 5000, 0, 255, 0, 255)

	sigNameLen := uint32((result >> 48) & 0xFFFFFFFF)
	payloadLen := uint32((result >> 32) & 0xFFFF)
	timedOut := uint16((result >> 16) & 0xFFFF)
	errCode := uint32(result & 0xFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if timedOut != 0 {
		t.Errorf("expected timedOut 0, got %d", timedOut)
	}
	if sigNameLen == 0 {
		t.Error("expected non-zero sigNameLen")
	}
	if payloadLen == 0 {
		t.Error("expected non-zero payloadLen")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestDurableAwaitSignals_ReplayWithAwaitAndSignalReceived(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{
		{
			Step: 0, EventType: EventTypeAwaitSignals,
			SignalNames: `["my-signal"]`, TimeoutMs: 5000,
		},
		{
			Step: 1, EventType: EventTypeSignalReceived,
			SignalName: "my-signal", SignalPayload: `{"msg":"hello"}`,
		},
	}

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.DurableAwaitSignals(ctx, nil, `["my-signal"]`, 5000, 0, 255, 0, 255)

	errCode := uint32(result & 0xFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 2 {
		t.Errorf("expected stepCount=2, got %d", s.stepCount)
	}
}

func TestDurableAwaitSignals_ReplayWithAwaitNoSignal(t *testing.T) {
	store := newMockSignalWorkflowStore()
	s := newTestExecSession()
	s.engine.signalStore = store
	s.isReplay = true
	s.history = []EventRecord{
		{
			Step: 0, EventType: EventTypeAwaitSignals,
			SignalNames: `["my-signal"]`, TimeoutMs: 5000,
		},
	}

	result := s.DurableAwaitSignals(context.Background(), nil, `["my-signal"]`, 5000, 0, 0, 0, 0)

	timedOut := uint16((result >> 16) & 0xFFFF)
	if timedOut != 1 {
		t.Errorf("expected timedOut 1 (suspend), got %d", timedOut)
	}
}

func TestDurableAwaitSignals_ReplayWithAwaitAndSignalInStore(t *testing.T) {
	store := newMockSignalWorkflowStore()
	ctx := context.Background()
	err := store.DeliverSignal(ctx, "", "my-signal", `{"msg":"from_store"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	s := newTestExecSession()
	s.engine.signalStore = store
	s.engine.workflowID = ""
	s.isReplay = true
	s.history = []EventRecord{
		{
			Step: 0, EventType: EventTypeAwaitSignals,
			SignalNames: `["my-signal"]`, TimeoutMs: 5000,
		},
	}

	buf := make([]byte, 256)
	ctx2 := contextWithRawMemBuf(ctx, buf)
	result := s.DurableAwaitSignals(ctx2, nil, `["my-signal"]`, 5000, 0, 255, 0, 255)

	errCode := uint32(result & 0xFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	// Should have recorded a SignalReceived event.
	if len(s.history) != 2 {
		t.Errorf("expected 2 history entries, got %d", len(s.history))
	}
	if s.history[1].EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", s.history[1].EventType)
	}
}

func TestDurableAwaitSignals_FreshSignalFoundInStore(t *testing.T) {
	store := newMockSignalWorkflowStore()
	ctx := context.Background()
	err := store.DeliverSignal(ctx, "wf-123", "my-signal", `{"msg":"hello"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	s := newTestExecSession()
	s.engine.signalStore = store
	s.engine.workflowID = "wf-123"

	buf := make([]byte, 256)
	ctx2 := contextWithRawMemBuf(ctx, buf)
	result := s.DurableAwaitSignals(ctx2, nil, `my-signal`, 5000, 0, 255, 0, 255)

	errCode := uint32(result & 0xFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", s.history[0].EventType)
	}
	if s.suspendErr != nil {
		t.Errorf("unexpected suspendErr: %v", s.suspendErr)
	}
}

func TestDurableAwaitSignals_FreshNoSignalSuspends(t *testing.T) {
	s := newTestExecSession()
	// No signal store, no signals.

	result := s.DurableAwaitSignals(context.Background(), nil, `["my-signal"]`, 5000, 0, 0, 0, 0)

	timedOut := uint16((result >> 16) & 0xFFFF)
	if timedOut != 1 {
		t.Errorf("expected timedOut 1 (suspend), got %d", timedOut)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_signals") {
		t.Errorf("expected 'await_signals' in reason, got %q", s.suspendErr.Reason)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeAwaitSignals {
		t.Errorf("expected EventTypeAwaitSignals, got %q", s.history[0].EventType)
	}
}

func TestDurableAwaitSignals_ReplayPastEnd(t *testing.T) {
	store := newMockSignalWorkflowStore()
	s := newTestExecSession()
	s.engine.signalStore = store
	s.engine.workflowID = "wf-123"
	s.isReplay = true
	s.history = nil

	result := s.DurableAwaitSignals(context.Background(), nil, `my-signal`, 5000, 0, 0, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	// With empty signal store, should suspend.
	if s.suspendErr == nil {
		t.Error("expected suspendErr after exitReplay with no signals")
	}
	_ = result
}

func TestDurableAwaitSignals_ReplayWrongEventType(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step: 0, EventType: EventTypeCall, // wrong type
	}}

	result := s.DurableAwaitSignals(context.Background(), nil, `my-signal`, 5000, 0, 0, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay (wrong event type)")
	}
	_ = result
}

// ---------------------------------------------------------------------------
// PollCancellation tests
// ---------------------------------------------------------------------------

func TestPollCancellation_Replay(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true

	result := s.PollCancellation(context.Background(), nil, 0, 0)

	if result != 0 {
		t.Errorf("expected 0 during replay, got %d", result)
	}
}

func TestPollCancellation_FreshNotCancelled(t *testing.T) {
	s := newTestExecSession()
	// No signal store.

	result := s.PollCancellation(context.Background(), nil, 0, 0)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

// ---------------------------------------------------------------------------
// PollSignal tests
// ---------------------------------------------------------------------------

func TestPollSignal_FreshFound(t *testing.T) {
	store := newMockSignalWorkflowStore()
	ctx := context.Background()
	err := store.DeliverSignal(ctx, "wf-123", "my-signal", `{"msg":"hello"}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	s := newTestExecSession()
	s.engine.signalStore = store
	s.engine.workflowID = "wf-123"

	buf := make([]byte, 256)
	ctx2 := contextWithRawMemBuf(ctx, buf)
	result := s.PollSignal(ctx2, nil, "my-signal", 0, 255)

	if result == 0 {
		t.Error("expected non-zero result when signal found")
	}
	flags := uint32(result & 0xFFFFFFFF)
	if flags&0x0100 == 0 {
		t.Error("expected found flag (0x0100) to be set")
	}
}

func TestPollSignal_FreshNotFound(t *testing.T) {
	s := newTestExecSession()
	// No signal store.

	result := s.PollSignal(context.Background(), nil, "my-signal", 0, 0)

	if result != 0 {
		t.Errorf("expected 0 when signal not found, got %d", result)
	}
}
