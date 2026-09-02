package engine

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock WorkflowState for Version / MinVersion tests.
// ---------------------------------------------------------------------------

type mockWorkflowState struct {
	version    int
	minVersion int
}

func (m *mockWorkflowState) Version() int                         { return m.version }
func (m *mockWorkflowState) MinVersion() int                      { return m.minVersion }
func (m *mockWorkflowState) ChildVersion(name string) (int, bool) { return 0, false }
func (m *mockWorkflowState) Priority() int                        { return 0 }

// ---------------------------------------------------------------------------
// Version tests.
// ---------------------------------------------------------------------------

func TestVersionDefault(t *testing.T) {
	s := newTestExecSession()
	// s.engine.state is nil → Version() returns 1.
	result := s.Version(context.Background())
	if result != 1 {
		t.Errorf("expected default version 1, got %d", result)
	}
}

func TestVersionWithState(t *testing.T) {
	s := newTestExecSession()
	s.engine.state = &mockWorkflowState{version: 3}
	result := s.Version(context.Background())
	if result != 3 {
		t.Errorf("expected version 3, got %d", result)
	}
}

// ---------------------------------------------------------------------------
// MinVersion tests.
// ---------------------------------------------------------------------------

func TestMinVersionDefault(t *testing.T) {
	s := newTestExecSession()
	// s.engine.state is nil → MinVersion() returns 1.
	result := s.MinVersion(context.Background())
	if result != 1 {
		t.Errorf("expected default min version 1, got %d", result)
	}
}

func TestMinVersionWithState(t *testing.T) {
	s := newTestExecSession()
	s.engine.state = &mockWorkflowState{minVersion: 2}
	result := s.MinVersion(context.Background())
	if result != 2 {
		t.Errorf("expected min version 2, got %d", result)
	}
}

// ---------------------------------------------------------------------------
// WorkflowID tests.
// ---------------------------------------------------------------------------

func TestWorkflowID(t *testing.T) {
	s := newTestExecSession()
	s.workflowID = "wf-001"
	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.WorkflowID(ctx, nil, 0, uint32(len(buf)))
	if result == 0 {
		t.Error("expected non-zero result (written length)")
	}
	got := string(buf[:6])
	if got != "wf-001" {
		t.Errorf("expected 'wf-001' written, got %q", got)
	}
}

func TestWorkflowIDUnknown(t *testing.T) {
	s := newTestExecSession()
	// workflowID is empty → should write "unknown".
	result := s.WorkflowID(context.Background(), nil, 0, 0)
	// writeResult with nil module returns 0, so the extra (written) is 0.
	written := uint32(result >> 32)
	if written != 0 {
		t.Errorf("expected written=0 with nil module, got %d", written)
	}

	// With a raw memory buffer in context, "unknown" should be written.
	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result = s.WorkflowID(ctx, nil, 0, 64)
	if result == 0 {
		t.Error("expected non-zero result when writing to buffer")
	}
}

// ---------------------------------------------------------------------------
// RunID tests.
// ---------------------------------------------------------------------------

func TestRunID(t *testing.T) {
	s := newTestExecSession()
	s.execRunID = "run-abc"
	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.RunID(ctx, nil, 0, uint32(len(buf)))
	if result == 0 {
		t.Error("expected non-zero result")
	}
	got := string(buf[:7])
	if got != "run-abc" {
		t.Errorf("expected 'run-abc' written, got %q", got)
	}
}

func TestRunIDUnknown(t *testing.T) {
	s := newTestExecSession()
	// execRunID is empty → should write "unknown".
	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.RunID(ctx, nil, 0, 64)
	written := uint32(result >> 32)
	if written == 0 {
		t.Error("expected non-zero written length for 'unknown'")
	}
}

// ---------------------------------------------------------------------------
// SetQueryState tests.
// ---------------------------------------------------------------------------

func TestSetQueryState(t *testing.T) {
	s := newTestExecSession()
	result := s.SetQueryState(context.Background(), nil, "my-key", "my-value")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.queryState["my-key"] != "my-value" {
		t.Errorf("expected queryState['my-key']='my-value', got %q", s.queryState["my-key"])
	}
}

func TestSetQueryStateOverwrite(t *testing.T) {
	s := newTestExecSession()
	s.SetQueryState(context.Background(), nil, "key", "first")
	s.SetQueryState(context.Background(), nil, "key", "second")
	if s.queryState["key"] != "second" {
		t.Errorf("expected queryState['key']='second', got %q", s.queryState["key"])
	}
	if len(s.queryState) != 1 {
		t.Errorf("expected exactly 1 entry, got %d", len(s.queryState))
	}
}

func TestSetQueryStateMultipleKeys(t *testing.T) {
	s := newTestExecSession()
	s.SetQueryState(context.Background(), nil, "a", "1")
	s.SetQueryState(context.Background(), nil, "b", "2")
	if s.queryState["a"] != "1" || s.queryState["b"] != "2" {
		t.Errorf("expected both keys to be set, got a=%q b=%q", s.queryState["a"], s.queryState["b"])
	}
	if len(s.queryState) != 2 {
		t.Errorf("expected 2 entries, got %d", len(s.queryState))
	}
}

// ---------------------------------------------------------------------------
// RegisterQueryHandler tests. This host call is a harmless ABI-compatibility
// no-op (see the doc comment on HostHandler.RegisterQueryHandler in
// imports.go) -- these tests pin that it still records the name and returns
// success, not that anything downstream acts on it.
// ---------------------------------------------------------------------------

func TestRegisterQueryHandler(t *testing.T) {
	s := newTestExecSession()
	result := s.RegisterQueryHandler(context.Background(), nil, "my-handler")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.queryHandlers) != 1 {
		t.Fatalf("expected 1 query handler, got %d", len(s.queryHandlers))
	}
	if s.queryHandlers[0] != "my-handler" {
		t.Errorf("expected handler 'my-handler', got %q", s.queryHandlers[0])
	}
}

func TestRegisterQueryHandlerMultiple(t *testing.T) {
	s := newTestExecSession()
	s.RegisterQueryHandler(context.Background(), nil, "handler-a")
	s.RegisterQueryHandler(context.Background(), nil, "handler-b")
	s.RegisterQueryHandler(context.Background(), nil, "handler-c")
	if len(s.queryHandlers) != 3 {
		t.Fatalf("expected 3 query handlers, got %d", len(s.queryHandlers))
	}
	if s.queryHandlers[0] != "handler-a" || s.queryHandlers[1] != "handler-b" || s.queryHandlers[2] != "handler-c" {
		t.Errorf("handler order mismatch: got %v", s.queryHandlers)
	}
}

// ---------------------------------------------------------------------------
// DurableLog tests.
// ---------------------------------------------------------------------------

func TestDurableLog(t *testing.T) {
	s := newTestExecSession()
	result := s.DurableLog(context.Background(), nil, "test message")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	// DurableLog does not write to history.
	if len(s.history) != 0 {
		t.Errorf("expected 0 history entries, got %d", len(s.history))
	}
}

// ---------------------------------------------------------------------------
// UUID tests.
// ---------------------------------------------------------------------------

func TestUUIDDeterminism(t *testing.T) {
	s := newTestExecSession()
	s.workflowID = "wf-uuid-test"
	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)

	// Same seed with same workflowID produces identical UUIDs.
	u1 := s.UUID(ctx, nil, "seed-1", 0, uint32(len(buf)))
	u2 := s.UUID(ctx, nil, "seed-1", 0, uint32(len(buf)))
	if u1 != u2 {
		t.Errorf("expected deterministic UUIDs, got %d and %d", u1, u2)
	}
}

func TestUUIDDifferentSeed(t *testing.T) {
	// This used to be an unconditional t.Skip with its entire body commented
	// out -- not environment-conditional at all, just a disabled test
	// reporting as a skip, which is strictly worse than no test: it reads as
	// coverage that "different seeds produce different UUIDs" exists when it
	// does not. The stated reason ("raw memory buffer does not expose UUID
	// hash to return value") is correct about the *packed* result -- s.UUID
	// returns packSimpleResult(0, written), which encodes only the error
	// code and the number of bytes written, not their content, so comparing
	// the two int64 results directly can never distinguish seeds (both
	// produce a 36-byte UUID string, so both pack to the same written
	// length). But contextWithRawMemBuf hands the test the actual
	// destination []byte, exactly as TestUUIDDeterminism above uses it, so
	// the UUID text itself is readable directly from the buffer -- no real
	// WASM module is needed, just decoding the written length the same way
	// the rest of this file already does (see TestWorkflowIDUnknown,
	// TestRunID) and slicing the buffer.
	s := newTestExecSession()
	s.workflowID = "wf-uuid-test"

	buf1 := make([]byte, 64)
	ctx1 := contextWithRawMemBuf(context.Background(), buf1)
	r1 := s.UUID(ctx1, nil, "seed-a", 0, uint32(len(buf1)))
	n1 := uint32(r1 >> 32)
	uuid1 := string(buf1[:n1])

	buf2 := make([]byte, 64)
	ctx2 := contextWithRawMemBuf(context.Background(), buf2)
	r2 := s.UUID(ctx2, nil, "seed-b", 0, uint32(len(buf2)))
	n2 := uint32(r2 >> 32)
	uuid2 := string(buf2[:n2])

	if uuid1 == "" || uuid2 == "" {
		t.Fatalf("expected non-empty UUIDs, got %q and %q", uuid1, uuid2)
	}
	if uuid1 == uuid2 {
		t.Errorf("expected different UUIDs for different seeds, got %q for both", uuid1)
	}
}

func TestUUIDUnknownWorkflowID(t *testing.T) {
	s := newTestExecSession()
	// workflowID is empty → function uses "unknown".
	buf := make([]byte, 64)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.UUID(ctx, nil, "seed", 0, uint32(len(buf)))
	if result == 0 {
		t.Error("expected non-zero result")
	}
}

// ---------------------------------------------------------------------------
// Now tests.
// ---------------------------------------------------------------------------

func TestNowFresh(t *testing.T) {
	s := newTestExecSession()
	s.nowMs = 1234567
	result := s.Now(context.Background())
	if result != 1234567 {
		t.Errorf("expected nowMs=1234567, got %d", result)
	}
}

func TestNowReplay(t *testing.T) {
	s := newTestExecSession()
	s.nowMs = 1000
	s.stepCount = 1
	s.history = []EventRecord{
		{Step: 0, EventType: EventTypeCall, TimestampMs: 500},
		{Step: 1, EventType: EventTypeCall, TimestampMs: 900},
	}
	// stepCount=1 and history[0] is the last consumed event.
	result := s.Now(context.Background())
	if result != 500 {
		t.Errorf("expected 500 (timestamp of last consumed event), got %d", result)
	}
}

// ---------------------------------------------------------------------------
// Random tests.
// ---------------------------------------------------------------------------

func TestRandomDeterminism(t *testing.T) {
	s := newTestExecSession()
	s.workflowID = "wf-random"
	ctx := context.Background()

	r1 := s.Random(ctx)
	r2 := s.Random(ctx)
	// Each call increments randomSeq, so values should differ.
	if r1 == r2 {
		t.Error("expected different random values for sequential calls")
	}
}

func TestRandomZeroStepCount(t *testing.T) {
	s := newTestExecSession()
	s.workflowID = "wf-random"
	s.stepCount = 0
	ctx := context.Background()

	// Should not panic and should return a reasonable value.
	result := s.Random(ctx)
	if result == 0 && s.randomSeq == 0 {
		t.Error("expected non-zero random value")
	}
}

// ---------------------------------------------------------------------------
// GetScope tests.
// ---------------------------------------------------------------------------

func TestGetScopeNotSet(t *testing.T) {
	s := newTestExecSession()
	result := s.GetScope(context.Background(), nil, 0, 0, 0, 0)
	if result != 0 {
		t.Errorf("expected 0 when scope is not set, got %d", result)
	}
}

// GetScope returns packed (objTypeLen << 32 | instKeyLen) when scope is set.
func TestGetScopeSet(t *testing.T) {
	s := newTestExecSession()
	s.scopeSet = true
	s.scopeObjType = "account"
	s.scopeInstKey = "acct-123"

	result := s.GetScope(context.Background(), nil, 0, 0, 0, 0)
	// With nil module, writeResult returns 0,0 so result should be 0.
	_ = result

	// With a raw memory buffer, it should write the obj type and instance key.
	buf := make([]byte, 128)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result = s.GetScope(ctx, nil, 0, 64, 64, 64)
	objTypeLen := uint32(result >> 32)
	instKeyLen := uint32(result)
	if objTypeLen == 0 {
		t.Error("expected non-zero objTypeLen when scope is set")
	}
	if instKeyLen == 0 {
		t.Error("expected non-zero instKeyLen when scope is set")
	}
}

// ---------------------------------------------------------------------------
// ReplyToSignal tests.
// ---------------------------------------------------------------------------

func TestReplyToSignalReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:          0,
		EventType:     EventTypeSignalReceived,
		SignalName:    "corr-001",
		SignalPayload: `{"response":"ok"}`,
	}}
	result := s.ReplyToSignal(context.Background(), nil, "corr-001", `{"response":"ok"}`)

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

func TestReplyToSignalReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type — should be EventTypeSignalReceived
	}}
	result := s.ReplyToSignal(context.Background(), nil, "corr-001", `{"response":"ok"}`)

	// After exitReplay, the fresh path records a new event.
	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if len(s.history) < 2 {
		t.Fatalf("expected at least 2 history entries, got %d", len(s.history))
	}
	if s.history[1].EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", s.history[1].EventType)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestReplyToSignalReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // stepCount(0) >= len(0) → past end

	result := s.ReplyToSignal(context.Background(), nil, "corr-001", `{"response":"ok"}`)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", s.history[0].EventType)
	}
	if s.history[0].SignalName != "corr-001" {
		t.Errorf("expected SignalName 'corr-001', got %q", s.history[0].SignalName)
	}
	if s.history[0].SignalPayload != `{"response":"ok"}` {
		t.Errorf("expected SignalPayload, got %q", s.history[0].SignalPayload)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestReplyToSignalFresh(t *testing.T) {
	s := newTestExecSession()

	result := s.ReplyToSignal(context.Background(), nil, "corr-001", `{"response":"ok"}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	r := s.history[0]
	if r.EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", r.EventType)
	}
	if r.SignalName != "corr-001" {
		t.Errorf("expected SignalName 'corr-001', got %q", r.SignalName)
	}
	if r.SignalPayload != `{"response":"ok"}` {
		t.Errorf("expected SignalPayload '{\"response\":\"ok\"}', got %q", r.SignalPayload)
	}
}
