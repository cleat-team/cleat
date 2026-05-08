package host

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// wasmWithMemory returns a minimal valid WASM module that exports "memory"
// (1 page / 64 KB).  This is enough for unit tests that write to WASM linear
// memory but do not call any WASM exports.
func wasmWithMemory() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic   \x00asm
		0x01, 0x00, 0x00, 0x00, // version 1
		// Memory section (id=5): one memory, min=1 page, max=1 page.
		0x05, 0x04, 0x01, 0x01, 0x01, 0x01,
		// Export section (id=7): one export "memory" of kind memory (0x02), index 0.
		0x07, 0x0a, 0x01, 0x06,
		0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, // "memory"
		0x02, 0x00,
	}
}

// newTestModule compiles and instantiates wasmWithMemory, returning the module.
// Cleanup is registered via t.Cleanup.
func newTestModule(t *testing.T, rt *Runtime) api.Module {
	t.Helper()
	ctx := context.Background()
	compiled, err := rt.CompileModule(ctx, wasmWithMemory())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	t.Cleanup(func() { compiled.Close(ctx) })
	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	t.Cleanup(func() { mod.Close(ctx) })
	return mod
}

// decodeCallResult unpacks a packDurableCallResult value.
func decodeCallResult(v int64) (responseLen int, callErrorCode, errCode byte) {
	return int((v >> 40) & 0xFFFFFF), byte((v >> 8) & 0xFF), byte(v & 0xFF)
}

// decodeSimpleResult unpacks a packSimpleResult value.
func decodeSimpleResult(v int64) (errCode byte, extra uint32) {
	return byte(v & 0xFF), uint32((v >> 32) & 0xFFFFFFFF)
}

// decodeSleepResult unpacks a packSleepResult value.
func decodeSleepResult(v int64) (status byte, durationMs int64) {
	return byte(v >> 56), int64(uint64(v) & 0x00FFFFFFFFFFFFFF)
}

// mockSignalStore implements SignalStore for tests that need signal/cancel
// interactions.
type mockSignalStore struct {
	signals      map[string]string // signalName -> payload
	cancelled    bool
	cancelReason string
}

func (m *mockSignalStore) DeliverSignal(_ context.Context, _, signalName, payload string) error {
	if m.signals == nil {
		m.signals = make(map[string]string)
	}
	m.signals[signalName] = payload
	return nil
}

func (m *mockSignalStore) PollSignal(_ context.Context, _, signalName string) (string, bool, error) {
	if m.signals == nil {
		return "", false, nil
	}
	payload, found := m.signals[signalName]
	if found {
		delete(m.signals, signalName)
	}
	return payload, found, nil
}

func (m *mockSignalStore) PollCancellation(_ context.Context, _ string) (bool, string, error) {
	return m.cancelled, m.cancelReason, nil
}

// ---------------------------------------------------------------------------
// DurableCall replay tests
// ---------------------------------------------------------------------------

func TestReplayCall_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Response: `{"ok":true}`},
		},
		isReplay: true,
	}

	result := session.replayCall(ctx, mod, "svc", "op1", `{}`, 0, 4096)
	respLen, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}
	if respLen < 1 {
		t.Errorf("expected response written, got length %d", respLen)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
	if !session.isReplay {
		t.Error("expected isReplay to remain true")
	}

	// Verify the cached response was written into memory.
	mem := mod.Memory()
	data, ok := mem.Read(0, uint32(respLen))
	if !ok || string(data) != `{"ok":true}` {
		t.Errorf("expected cached response in memory, got %q", string(data))
	}
}

func TestReplayCall_Divergence_WrongService(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Response: `{"ok":true}`},
		},
		isReplay: true,
	}

	// Call with a different service name → divergence.
	result := session.replayCall(ctx, mod, "different_svc", "op1", `{}`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected non-zero error code for service divergence")
	}
	_ = callErrCode
}

func TestReplayCall_Divergence_WrongOperation(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Response: `{"ok":true}`},
		},
		isReplay: true,
	}

	result := session.replayCall(ctx, mod, "svc", "different_op", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected non-zero error code for operation divergence")
	}
}

func TestReplayCall_Divergence_WrongEventType(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeSleep, DurationMs: 5000}, // not a call event!
		},
		isReplay: true,
	}

	result := session.replayCall(ctx, mod, "svc", "op1", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected non-zero error code for event type divergence")
	}
}

func TestReplayCall_HistoryExhausted(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	caller := &mockCaller{}
	session := &execSession{
		engine:   &Engine{caller: caller},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Response: `{"ok":true}`},
		},
		isReplay: true,
		stepCount: 1, // already past the history
	}

	// History is exhausted → should fall through to fresh execution.
	result := session.replayCall(ctx, mod, "svc", "op1", `{}`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected fresh call to succeed, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}
	// Verify isReplay was switched to false.
	if session.isReplay {
		t.Error("expected isReplay to become false after history exhausted")
	}
	// Verify a real call was made.
	if len(caller.calls) != 1 {
		t.Errorf("expected 1 real call, got %d", len(caller.calls))
	}
}

func TestReplayCall_ErrorInHistory(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op1", Request: `{}`, Response: "", Err: "service unavailable"},
		},
		isReplay: true,
	}

	result := session.replayCall(ctx, mod, "svc", "op1", `{}`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	// Should report as error.
	if errCode == 0 {
		t.Error("expected error code for cached error in history")
	}
	_ = callErrCode
}

// ---------------------------------------------------------------------------
// PluginCall replay tests
// ---------------------------------------------------------------------------

func TestReplayPluginCall_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{pluginRegistry: NewPluginRegistry(), caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f", PluginOutput: `{"result":"cached"}`},
		},
		isReplay: true,
	}

	result := session.replayPluginCall(ctx, mod, "p", "f", `{"x":1}`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

func TestReplayPluginCall_Divergence(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{pluginRegistry: NewPluginRegistry(), caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f", PluginOutput: `{"result":"cached"}`},
		},
		isReplay: true,
	}

	// Call with mismatched plugin name.
	result := session.replayPluginCall(ctx, mod, "wrong", "f", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected non-zero error code for plugin divergence")
	}
}

func TestReplayPluginCall_ErrorInHistory(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{pluginRegistry: NewPluginRegistry(), caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f", PluginError: "plugin panic"},
		},
		isReplay: true,
	}

	result := session.replayPluginCall(ctx, mod, "p", "f", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for cached plugin error")
	}
}

func TestReplayPluginCall_IdempotentReinvoke(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	reg := NewPluginRegistry()
	reg.RegisterIdempotent("p", "f", func(_ context.Context, _ string) (string, error) {
		return `{"re-invoked":true}`, nil
	})

	session := &execSession{
		engine:   &Engine{pluginRegistry: reg, caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypePluginCall, PluginName: "p", PluginFunc: "f",
				PluginInput: `{"x":1}`, PluginOutput: `{"original":true}`, Idempotent: true},
		},
		isReplay: true,
	}

	// Idempotent functions are re-invoked during replay, so the history output
	// is replaced with fresh output.
	result := session.replayPluginCall(ctx, mod, "p", "f", `{"x":1}`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}
	mem := mod.Memory()
	respLen := int((result >> 40) & 0xFFFFFF)
	if respLen == 0 {
		t.Fatal("expected response written to memory")
	}
	data, ok := mem.Read(0, uint32(respLen))
	if !ok || string(data) != `{"re-invoked":true}` {
		t.Errorf("expected re-invoked result, got %q (len=%d)", string(data), respLen)
	}
	// stepCount must NOT advance (idempotent re-invocation reuses existing event).
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1 (no advancement), got %d", session.stepCount)
	}
}

// ---------------------------------------------------------------------------
// DurableSleep replay tests
// ---------------------------------------------------------------------------

func TestReplaySleep_Completed(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeSleep, DurationMs: 5000},
		},
		isReplay: true,
	}

	result := session.DurableSleep(ctx, mod, 5000)
	status, duration := decodeSleepResult(result)
	if status != sleepStatusCompleted {
		t.Errorf("expected sleep status completed (%d), got %d", sleepStatusCompleted, status)
	}
	if duration != 0 {
		t.Errorf("expected duration 0 on replay, got %d", duration)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

func TestReplaySleep_PastHistoryFallsThrough(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{},
		isReplay: true,
	}

	// No sleep event in history → falls through to fresh execution.
	result := session.DurableSleep(ctx, mod, 5000)
	status, duration := decodeSleepResult(result)
	if status != sleepStatusSuspend {
		t.Errorf("expected sleep status suspend (%d), got %d", sleepStatusSuspend, status)
	}
	if duration != 5000 {
		t.Errorf("expected duration 5000, got %d", duration)
	}
	if session.suspendErr == nil {
		t.Error("expected suspendErr to be set")
	}
}

// ---------------------------------------------------------------------------
// DurableAwaitSignals replay tests
// ---------------------------------------------------------------------------

func TestReplaySignal_Received(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeSignalReceived, SignalName: "payment", SignalPayload: `{"paid":true}`},
		},
		isReplay: true,
	}

	// AwaitSignals calls should replay the signal_received event.
	result := session.DurableAwaitSignals(ctx, mod, "payment", 10000, 0, 4096, 4096, 4096)
	// The packed result encodes sig name length and payload length in the upper
	// bits.  A non-zero value indicates a signal was returned.
	if result == 0 {
		t.Error("expected signal received result")
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

func TestReplaySignal_AwaitThenReceived(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// History has await_signals followed by signal_received.
	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: "payment", TimeoutMs: 10000},
			{Step: 1, EventType: EventTypeSignalReceived, SignalName: "payment", SignalPayload: `{"paid":true}`},
		},
		isReplay: true,
	}

	result := session.DurableAwaitSignals(ctx, mod, "payment", 10000, 0, 4096, 4096, 4096)
	if result == 0 {
		t.Error("expected signal received result after await+receive")
	}
	// Both events consumed.
	if session.stepCount != 2 {
		t.Errorf("expected stepCount=2, got %d", session.stepCount)
	}
}

// ---------------------------------------------------------------------------
// SideEffect replay tests
// ---------------------------------------------------------------------------

func TestReplaySideEffect_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeSideEffect, SideEffectResult: `{"random":42}`},
		},
		isReplay: true,
	}

	result := session.SideEffect(ctx, mod, `{"random":99}`, 0, 4096)
	errCode, written := decodeSimpleResult(result)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if written == 0 {
		t.Fatal("expected response written")
	}
	mem := mod.Memory()
	data, ok := mem.Read(0, written)
	if !ok || string(data) != `{"random":42}` {
		t.Errorf("expected cached side effect result, got %q", string(data))
	}
}

func TestReplaySideEffect_WrongType(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `{}`},
		},
		isReplay: true,
	}

	// History has a Call event but we're replaying a SideEffect → type mismatch.
	result := session.SideEffect(ctx, mod, `{"random":99}`, 0, 4096)
	errCode, _ := decodeSimpleResult(result)
	if errCode == 0 {
		t.Error("expected error code for side effect type mismatch")
	}
}

// ---------------------------------------------------------------------------
// AcquireLock replay tests
// ---------------------------------------------------------------------------

func TestReplayAcquireLock_Acquired(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeAcquireLock, LockKey: "my-lock", LockAcquired: 1},
		},
		isReplay: true,
	}

	result := session.replayAcquireLock(ctx, mod, "my-lock", 30000)
	acquired := ((result >> 8) & 0xFF) != 0
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if !acquired {
		t.Error("expected lock acquired=true")
	}
}

func TestReplayAcquireLock_NotAcquired(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeAcquireLock, LockKey: "my-lock", LockAcquired: 0},
		},
		isReplay: true,
	}

	result := session.replayAcquireLock(ctx, mod, "my-lock", 30000)
	acquired := ((result >> 8) & 0xFF) != 0
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if acquired {
		t.Error("expected lock acquired=false")
	}
}

// ---------------------------------------------------------------------------
// AwaitChild replay tests
// ---------------------------------------------------------------------------

func TestReplayAwaitChild_Completed(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeAwaitChild, RunID: "child-1", Response: `{"done":true}`},
		},
		isReplay: true,
		workflowID: "wf-parent",
	}

	result := session.AwaitChild(ctx, mod, "child-1", 0, 4096)
	// packed: upper 32 bits = written length, lower 32 bits = error code.
	written := uint32((result >> 32) & 0xFFFFFFFF)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if written == 0 {
		t.Fatal("expected response written")
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

func TestReplayAwaitChild_Error(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeAwaitChild, RunID: "child-1", Err: "child failed"},
		},
		isReplay: true,
		workflowID: "wf-parent",
	}

	result := session.AwaitChild(ctx, mod, "child-1", 0, 4096)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected error code 1, got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// ContinueAsNew replay tests
// ---------------------------------------------------------------------------

func TestReplayContinueAsNew_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeContinueAsNew, NewInput: `{"restart":true}`},
		},
		isReplay: true,
	}

	_ = session.ContinueAsNew(ctx, mod, `{"restart":true}`)
	// Should have consumed the event and set suspendErr.
	if session.suspendErr == nil {
		t.Fatal("expected suspendErr to be set after ContinueAsNew replay")
	}
	if session.suspendErr.NewInput != `{"restart":true}` {
		t.Errorf("expected NewInput=%q, got %q", `{"restart":true}`, session.suspendErr.NewInput)
	}
}

// ---------------------------------------------------------------------------
// RegisterUpdateHandler replay tests
// ---------------------------------------------------------------------------

func TestReplayRegisterUpdateHandler(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeUpdateHandler, UpdateHandlerName: "update-shipping"},
		},
		isReplay: true,
	}

	result := session.RegisterUpdateHandler(ctx, mod, "update-shipping")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

// ---------------------------------------------------------------------------
// ReleaseLock replay tests
// ---------------------------------------------------------------------------

func TestReplayReleaseLock_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeReleaseLock, LockKey: "my-lock"},
		},
		isReplay: true,
	}

	result := session.replayReleaseLock(ctx, mod, "my-lock")
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

// ---------------------------------------------------------------------------
// AwaitAllChildren replay tests
// ---------------------------------------------------------------------------

func TestReplayAwaitAllChildren_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1","c2"]`, Response: `[{"run_id":"c1","result":"ok"},{"run_id":"c2","result":"ok"}]`},
		},
		isReplay: true,
	}

	result := session.replayAwaitAllChildren(ctx, mod, `["c1","c2"]`, 0, 4096)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected success, got errCode=%d", errCode)
	}
}

// ---------------------------------------------------------------------------
// CallWithHeartbeat replay tests
// ---------------------------------------------------------------------------

func TestReplayCallWithHeartbeat_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// History: heartbeat events followed by a call event.
	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeHeartbeat, Service: "svc", Op: "long-op"},
			{Step: 1, EventType: EventTypeHeartbeat, Service: "svc", Op: "long-op"},
			{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "long-op", Response: `{"done":true}`},
		},
		isReplay: true,
	}

	result := session.replayCallWithHeartbeat(ctx, mod, "svc", "long-op", `{}`, 1000, 0, 4096)
	// Heartbeat events consumed, call event consumed.
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}
	// stepCount advances: 2 heartbeats + 1 call = 3.
	if session.stepCount != 3 {
		t.Errorf("expected stepCount=3 (2 heartbeats + 1 call), got %d", session.stepCount)
	}
}

func TestReplayCallWithHeartbeat_Divergence(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "other", Response: `{}`},
		},
		isReplay: true,
	}

	result := session.replayCallWithHeartbeat(ctx, mod, "svc", "long-op", `{}`, 1000, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code for heartbeat call divergence")
	}
}

// ---------------------------------------------------------------------------
// SignalWorkflow / ReplyToSignal / SendSignalAndWait replay tests
// ---------------------------------------------------------------------------

func TestReplaySignalWorkflow(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeSignalReceived, SignalName: "approval", SignalPayload: `{"approved":true}`, RunID: "target-wf"},
		},
		isReplay: true,
	}

	result := session.SignalWorkflow(ctx, mod, "target-wf", "approval", `{"approved":true}`)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestReplayReplyToSignal(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeSignalReceived, SignalName: "corr-1", SignalPayload: `{"response":"ok"}`},
		},
		isReplay: true,
	}

	result := session.ReplyToSignal(ctx, mod, "corr-1", `{"response":"ok"}`)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestReplaySendSignalAndWait_Received(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeSignalReceived, SignalName: "response", SignalPayload: `{"done":true}`},
		},
		isReplay: true,
	}

	result := session.SendSignalAndWait(ctx, mod, "target", "response", `{}`, 10000, 0, 4096)
	extra := uint32((result >> 32) & 0xFFFFFFFF)
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if extra == 0 {
		t.Error("expected signal payload written to memory")
	}
}

// ---------------------------------------------------------------------------
// Defer replay tests
// ---------------------------------------------------------------------------

func TestReplayDefer_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeDefer, DeferDescription: "cleanup", DeferID: "defer-0"},
		},
		deferrals: make(map[string]string),
		isReplay:  true,
	}

	result := session.DurableDefer(ctx, mod, "cleanup", 0, 4096)
	if result == 0 {
		t.Error("expected non-zero result with defer ID length")
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

func TestReplayDefer_PastHistoryFallsThrough(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{},
		deferrals: make(map[string]string),
		isReplay:  true,
	}

	// No defer in history → fresh execution creates one.
	result := session.DurableDefer(ctx, mod, "cleanup", 0, 4096)
	if result == 0 {
		t.Error("expected non-zero result with defer ID length")
	}
	if len(session.deferrals) != 1 {
		t.Errorf("expected 1 deferral, got %d", len(session.deferrals))
	}
}

// ---------------------------------------------------------------------------
// SetScope replay tests
// ---------------------------------------------------------------------------

func TestReplaySetScope_Acquired(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:     &Engine{caller: &mockCaller{}},
		history:    []EventRecord{
			{Step: 0, EventType: EventTypeScopeAcquired, ScopeKey: "vo:order:123:"},
		},
		isReplay:   true,
		heldScopes: make([]string, 0),
	}

	result := session.replaySetScope(ctx, mod, "order", "123", 0, 4096)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if !session.scopeSet {
		t.Error("expected scope to be set after replay")
	}
	if session.scopePrefix != "vo:order:123:" {
		t.Errorf("expected scope prefix 'vo:order:123:', got %q", session.scopePrefix)
	}
}

// ---------------------------------------------------------------------------
// CreatePromise replay tests
// ---------------------------------------------------------------------------

func TestReplayCreatePromise(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCreatePromise, PromiseName: "order-promise", PromiseID: "prom-001"},
		},
		isReplay: true,
	}

	result := session.CreatePromise(ctx, mod, "order-promise", 0, 4096)
	errCode, _ := decodeSimpleResult(result)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}
}

// ---------------------------------------------------------------------------
// AwaitPromise replay tests
// ---------------------------------------------------------------------------

func TestReplayAwaitPromise_Resolved(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypePromiseResolved, PromiseID: "prom-001", PromiseResult: `{"status":"completed"}`},
		},
		isReplay: true,
	}

	result := session.AwaitPromise(ctx, mod, "prom-001", 30000, 0, 4096)
	// packed: upper 32 bits = result length, bit 16 = timedOut, lower 16 = errCode.
	resultLen := uint32((result >> 32) & 0xFFFFFFFF)
	timedOut := ((result >> 16) & 0xFFFF) != 0
	errCode := uint16(result & 0xFFFF)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if timedOut {
		t.Error("expected not timed out")
	}
	if resultLen == 0 {
		t.Error("expected promise result written")
	}
}

func TestReplayAwaitPromise_Rejected(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypePromiseRejected, PromiseID: "prom-001", PromiseError: "card declined"},
		},
		isReplay: true,
	}

	result := session.AwaitPromise(ctx, mod, "prom-001", 30000, 0, 4096)
	errCode := uint16(result & 0xFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode=1 for rejected promise, got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// ContinueAsNewWithVersion replay tests
// ---------------------------------------------------------------------------

func TestReplayContinueAsNewWithVersion(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeContinueAsNew, NewInput: `{"upgraded":true}`, NewVersion: 2},
		},
		isReplay: true,
	}

	_ = session.ContinueAsNewWithVersion(ctx, mod, `{"upgraded":true}`, 2)
	if session.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if session.suspendErr.NewInput != `{"upgraded":true}` {
		t.Errorf("expected NewInput=%q, got %q", `{"upgraded":true}`, session.suspendErr.NewInput)
	}
	if session.suspendErr.NewVersion != 2 {
		t.Errorf("expected NewVersion=2, got %d", session.suspendErr.NewVersion)
	}
}

// ---------------------------------------------------------------------------
// ReplayPluginCallStreaming tests
// ---------------------------------------------------------------------------

func TestReplayPluginCallStreaming_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypePluginCallStreamChunk, PluginOutput: `{"chunk":1}`, StreamChunkIndex: 0, StreamFinish: false},
			{Step: 1, EventType: EventTypePluginCallStreamChunk, PluginOutput: `{"chunk":2}`, StreamChunkIndex: 1, StreamFinish: true},
		},
		isReplay: true,
	}

	result := session.replayPluginCallStreaming(ctx, mod, "p", "f", `{}`, 0, 4096)
	errCode := uint32(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected success, got errCode=%d", errCode)
	}
	if session.stepCount != 2 {
		t.Errorf("expected stepCount=2, got %d", session.stepCount)
	}
}

func TestReplayPluginCallStreaming_Empty(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// No stream chunk events → empty collection.
	session := &execSession{
		engine:   &Engine{caller: &mockCaller{}},
		history:  []EventRecord{
			{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o"},
		},
		isReplay: true,
	}

	result := session.replayPluginCallStreaming(ctx, mod, "p", "f", `{}`, 0, 4096)
	errCode := uint32(result & 0xFF)
	if errCode != 0 {
		t.Errorf("expected success (empty stream), got errCode=%d", errCode)
	}
	// Should not have consumed the Call event.
	if session.stepCount != 0 {
		t.Errorf("expected stepCount=0 (no stream events consumed), got %d", session.stepCount)
	}
}
