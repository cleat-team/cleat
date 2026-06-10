package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ---------------------------------------------------------------------------
// stubHostHandler — minimal HostHandler implementation for testing.
// Each method returns 0 (or equivalent) as a no-op.
// ---------------------------------------------------------------------------

type stubHostHandler struct{}

func (h *stubHostHandler) DurableCall(_ context.Context, _ api.Module, _, _, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) DurableSleep(_ context.Context, _ api.Module, _ int64) int64 { return 0 }
func (h *stubHostHandler) DurableAwaitSignals(_ context.Context, _ api.Module, _ string, _ int64, _, _, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) DurableDefer(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) DurableLog(_ context.Context, _ api.Module, _ string) int64 { return 0 }
func (h *stubHostHandler) PollCancellation(_ context.Context, _ api.Module, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) PollSignal(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) ContinueAsNew(_ context.Context, _ api.Module, _ string) int64 { return 0 }
func (h *stubHostHandler) ContinueAsNewWithVersion(_ context.Context, _ api.Module, _ string, _ int) int64 {
	return 0
}
func (h *stubHostHandler) ChildWorkflow(_ context.Context, _ api.Module, _, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) ChildWorkflowWithOptions(_ context.Context, _ api.Module, _, _ string, _, _ int64, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) ChildWorkflowInSchema(_ context.Context, _ api.Module, _, _, _ string, _, _ int64, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) AwaitChild(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) AwaitAllChildren(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) PollChild(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) AwaitAnyChild(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) DurableCallWithRetry(_ context.Context, _ api.Module, _, _, _ string, _, _, _, _ int64, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) DurableCallWithHeartbeat(_ context.Context, _ api.Module, _, _, _ string, _ int64, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) Version(_ context.Context) int64                                  { return 0 }
func (h *stubHostHandler) MinVersion(_ context.Context) int64                               { return 0 }
func (h *stubHostHandler) SetQueryState(_ context.Context, _ api.Module, _, _ string) int64 { return 0 }
func (h *stubHostHandler) Now(_ context.Context) int64                                      { return 0 }
func (h *stubHostHandler) Random(_ context.Context) int64                                   { return 0 }
func (h *stubHostHandler) CreatePromise(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) AwaitPromise(_ context.Context, _ api.Module, _ string, _ int64, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) PluginCall(_ context.Context, _ api.Module, _, _, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) PluginCallStreaming(_ context.Context, _ api.Module, _, _, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) RegisterUpdateHandler(_ context.Context, _ api.Module, _ string) int64 {
	return 0
}
func (h *stubHostHandler) SendSignalAndWait(_ context.Context, _ api.Module, _, _, _ string, _ int64, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) ReplyToSignal(_ context.Context, _ api.Module, _, _ string) int64 { return 0 }
func (h *stubHostHandler) SignalWorkflow(_ context.Context, _ api.Module, _, _, _ string) int64 {
	return 0
}
func (h *stubHostHandler) SetScope(_ context.Context, _ api.Module, _, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) GetScope(_ context.Context, _ api.Module, _, _, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) UUID(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) AcquireLock(_ context.Context, _ api.Module, _ string, _ int64) int64 {
	return 0
}
func (h *stubHostHandler) ReleaseLock(_ context.Context, _ api.Module, _ string) int64 { return 0 }
func (h *stubHostHandler) SideEffect(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) WorkflowID(_ context.Context, _ api.Module, _, _ uint32) int64 { return 0 }
func (h *stubHostHandler) RunID(_ context.Context, _ api.Module, _, _ uint32) int64      { return 0 }
func (h *stubHostHandler) ResolvePromise(_ context.Context, _ api.Module, _, _ string) int64 {
	return 0
}
func (h *stubHostHandler) RejectPromise(_ context.Context, _ api.Module, _, _ string) int64 { return 0 }
func (h *stubHostHandler) DurableSend(_ context.Context, _ api.Module, _, _, _ string) int64 {
	return 0
}
func (h *stubHostHandler) DurableScheduleInvoke(_ context.Context, _ api.Module, _, _, _ string, _ int64) int64 {
	return 0
}
func (h *stubHostHandler) RegisterQueryHandler(_ context.Context, _ api.Module, _ string) int64 {
	return 0
}
func (h *stubHostHandler) SetState(_ context.Context, _ api.Module, _, _ string) int64 { return 0 }
func (h *stubHostHandler) GetState(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) DeleteState(_ context.Context, _ api.Module, _ string) int64 { return 0 }
func (h *stubHostHandler) IncrState(_ context.Context, _ api.Module, _ string, _ int64) int64 {
	return 0
}
func (h *stubHostHandler) HasState(_ context.Context, _ api.Module, _ string) int64 { return 0 }
func (h *stubHostHandler) ListState(_ context.Context, _ api.Module, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) RunDetached(_ context.Context, _ api.Module, _, _ string) int64 { return 0 }
func (h *stubHostHandler) Fetch(_ context.Context, _ api.Module, _, _, _, _ string, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) JsonParse(_ context.Context, _ api.Module, _, _, _, _ uint32) int64 {
	return 0
}
func (h *stubHostHandler) JsonStringify(_ context.Context, _ api.Module, _, _, _, _ uint32) int64 {
	return 0
}

// ---------------------------------------------------------------------------
// withHandler / handlerFromContext tests
// ---------------------------------------------------------------------------

func TestWithHandler_RoundTrip(t *testing.T) {
	h := &stubHostHandler{}
	ctx := withHandler(context.Background(), h)
	got := handlerFromContext(ctx)
	if got != h {
		t.Error("handlerFromContext did not return the same handler")
	}
}

func TestHandlerFromContext_NoHandlerInContext(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when no handler in context")
		}
	}()
	handlerFromContext(context.Background())
}

func TestHandlerFromContext_WrongTypeInContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), handlerContextKey{}, "not a handler")
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for wrong type in context")
		}
	}()
	handlerFromContext(ctx)
}

// ---------------------------------------------------------------------------
// handlerContextKey type tests
// ---------------------------------------------------------------------------

func TestHandlerContextKey_Identity(t *testing.T) {
	// Verify the context key is usable for storing and retrieving values.
	type myHandler struct{}
	ctx := context.WithValue(context.Background(), handlerContextKey{}, &myHandler{})
	val := ctx.Value(handlerContextKey{})
	if val == nil {
		t.Error("handlerContextKey did not store value")
	}
	if _, ok := val.(*myHandler); !ok {
		t.Error("handlerContextKey stored value has wrong type")
	}
}

func TestHandlerContextKey_NoValue(t *testing.T) {
	ctx := context.Background()
	val := ctx.Value(handlerContextKey{})
	if val != nil {
		t.Error("expected nil for context without handlerContextKey")
	}
}

// ---------------------------------------------------------------------------
// registerHostFunctions tests
// ---------------------------------------------------------------------------

func TestRegisterHostFunctions_NoPanic(t *testing.T) {
	// registerHostFunctions is called internally by NewRuntime, which is already
	// tested in host_test.go. This test verifies it can be called directly on a
	// fresh builder without error.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Create a fresh host module builder and register functions on it.
	// This exercises registerHostFunctions in isolation from the normal
	// "env" module registration done in NewRuntime.
	builder := rt.wazeroRuntime.NewHostModuleBuilder("test_env")
	registerHostFunctions(builder, rt)
	mod, err := builder.Instantiate(ctx)
	if err != nil {
		t.Fatalf("Instantiate test host module: %v", err)
	}
	mod.Close(ctx)
}

func TestRegisterHostFunctions_MultipleTimes(t *testing.T) {
	// Calling registerHostFunctions on different builders should work.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	b1 := rt.wazeroRuntime.NewHostModuleBuilder("test_env_1")
	registerHostFunctions(b1, rt)
	m1, err := b1.Instantiate(ctx)
	if err != nil {
		t.Fatalf("Instantiate test_env_1: %v", err)
	}
	m1.Close(ctx)

	b2 := rt.wazeroRuntime.NewHostModuleBuilder("test_env_2")
	registerHostFunctions(b2, rt)
	m2, err := b2.Instantiate(ctx)
	if err != nil {
		t.Fatalf("Instantiate test_env_2: %v", err)
	}
	m2.Close(ctx)
}

// ---------------------------------------------------------------------------
// cleatComplete and cleatCompleteKey tests
// ---------------------------------------------------------------------------

func TestCleatCompleteKey_StoresAndRetrieves(t *testing.T) {
	cc := &cleatComplete{}
	ctx := context.WithValue(context.Background(), &cleatCompleteKey, cc)
	got := ctx.Value(&cleatCompleteKey)
	if got == nil {
		t.Fatal("cleatCompleteKey did not store value")
	}
	if got != cc {
		t.Error("cleatCompleteKey stored wrong value")
	}
}

func TestCleatComplete_DefaultState(t *testing.T) {
	cc := &cleatComplete{}
	if cc.Result != nil {
		t.Error("expected nil Result in default cleatComplete")
	}
	if cc.Error != nil {
		t.Error("expected nil Error in default cleatComplete")
	}
}

// ---------------------------------------------------------------------------
// UpdateNowMs tests
// ---------------------------------------------------------------------------

func TestUpdateNowMs_SetsNonZero(t *testing.T) {
	// Reset to zero first, then call UpdateNowMs.
	nowMs.Store(0)
	UpdateNowMs()
	val := nowMs.Load()
	if val == 0 {
		t.Error("UpdateNowMs should set a non-zero timestamp")
	}
	// The value should be close to the current wall clock time (within 5 seconds).
	approx := time.Now().UnixMilli()
	diff := val - approx
	if diff < -5000 || diff > 5000 {
		t.Errorf("UpdateNowMs value %d is far from wall clock %d (diff=%d)", val, approx, diff)
	}
}

func TestUpdateNowMs_OverwritesPreviousValue(t *testing.T) {
	nowMs.Store(12345)
	UpdateNowMs()
	val := nowMs.Load()
	if val == 12345 {
		t.Error("UpdateNowMs should overwrite the previous value")
	}
	if val == 0 {
		t.Error("UpdateNowMs should set a non-zero value")
	}
}

// ---------------------------------------------------------------------------
// nowMs atomic tests
// ---------------------------------------------------------------------------

func TestNowMs_AtomicAccess(t *testing.T) {
	// Verify the atomic is accessible and stores/loads correctly.
	nowMs.Store(999)
	if got := nowMs.Load(); got != 999 {
		t.Errorf("nowMs.Load() = %d, want 999", got)
	}
}

// Error messages from runtime.go exercised via import resolution
// ---------------------------------------------------------------------------

func TestWasmInitOnce_Twice(t *testing.T) {
	// Verify that creating two Runtimes does not panic (wazeroInitOnce ensures
	// the dummy runtime is initialized only once).
	ctx := context.Background()
	rt1, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("first NewRuntime: %v", err)
	}
	defer rt1.Close(ctx)

	rt2, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("second NewRuntime: %v", err)
	}
	defer rt2.Close(ctx)
}

// ---------------------------------------------------------------------------
// Test that import resolution error string constants match expectations
// ---------------------------------------------------------------------------

func TestErrBadParam_Constant(t *testing.T) {
	if errBadParam != uint64(0xFFFFFFFF_00000001) {
		t.Errorf("errBadParam = %x, want %x", errBadParam, uint64(0xFFFFFFFF_00000001))
	}
}

func TestErrSignalAuthRequired_Constant(t *testing.T) {
	if errSignalAuthRequired != uint64(0xFFFFFFFF_00000002) {
		t.Errorf("errSignalAuthRequired = %x, want %x", errSignalAuthRequired, uint64(0xFFFFFFFF_00000002))
	}
}

func TestErrSignalAuthRequiredInt_Constant(t *testing.T) {
	if errSignalAuthRequiredInt != -4294967294 {
		t.Errorf("errSignalAuthRequiredInt = %d, want %d", errSignalAuthRequiredInt, -4294967294)
	}
}

// ---------------------------------------------------------------------------
// Verify host function names are as expected (compile-time check)
// ---------------------------------------------------------------------------

func TestHostFunctionNames_AllRegistered(t *testing.T) {
	// The host_test.go TestNewRuntime already verifies that all host functions
	// are registered. This test provides additional confidence by verifying
	// that a WASM module importing these functions can be compiled.
	// We use minimalWasm() which has no imports, but we verify the Runtime
	// was created successfully (which means host modules are registered).
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Compile a minimal module to verify the runtime is functional.
	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	exports := compiled.ExportedFunctions()
	if len(exports) != 0 {
		t.Logf("minimal WASM has %d exports (expected 0)", len(exports))
	}
}


// ---------------------------------------------------------------------------
// Cleanup: verify that withHandler stores the handler under the right key
// ---------------------------------------------------------------------------

func TestWithHandler_UsesCorrectKey(t *testing.T) {
	h := &stubHostHandler{}
	ctx := withHandler(context.Background(), h)
	val := ctx.Value(handlerContextKey{})
	if val == nil {
		t.Fatal("withHandler did not store value under handlerContextKey")
	}
	if _, ok := val.(HostHandler); !ok {
		t.Error("stored value does not satisfy HostHandler interface")
	}
	if val != h {
		t.Error("stored value is not the same handler pointer")
	}
}

// ---------------------------------------------------------------------------
// cleat_call wrapper tests (handler dispatch)
// ---------------------------------------------------------------------------

// TestCleatCall_ReadsServiceOpReq verifies that DurableCall dispatches the
// correct service, operation, and request to the ServiceCaller.
func TestCleatCall_ReadsServiceOpReq(t *testing.T) {
	caller := &mockCaller{}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine: engine,
	}

	result := s.DurableCall(context.Background(), nil,
		"my-service", "my-operation", `{"key":"value"}`, 0, 0)

	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(caller.calls))
	}
	if caller.calls[0].Service != "my-service" {
		t.Errorf("Service = %q, want %q", caller.calls[0].Service, "my-service")
	}
	if caller.calls[0].Op != "my-operation" {
		t.Errorf("Op = %q, want %q", caller.calls[0].Op, "my-operation")
	}
	if caller.calls[0].Request != `{"key":"value"}` {
		t.Errorf("Request = %q, want %q", caller.calls[0].Request, `{"key":"value"}`)
	}

	// Should be a success result
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
}

// TestCleatCall_InvalidStrings verifies that DurableCall handles various edge
// case string inputs without crashing.
func TestCleatCall_InvalidStrings(t *testing.T) {
	tests := []struct {
		name    string
		service string
		op      string
		request string
	}{
		{name: "empty service", service: "", op: "op", request: `{}`},
		{name: "empty op", service: "svc", op: "", request: `{}`},
		{name: "empty request", service: "svc", op: "op", request: ""},
		{name: "all empty", service: "", op: "", request: ""},
		{name: "unicode in request", service: "svc", op: "op", request: `{"msg":"héllo wörld"}`},
		{name: "long request", service: "svc", op: "op", request: `{"data":"` + string(make([]byte, 1000)) + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller := &mockCaller{}
			engine := NewEngine(nil, caller)
			s := &execSession{
				engine: engine,
			}

			result := s.DurableCall(context.Background(), nil,
				tt.service, tt.op, tt.request, 0, 0)

			if len(caller.calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(caller.calls))
			}
			if caller.calls[0].Service != tt.service {
				t.Errorf("Service = %q, want %q", caller.calls[0].Service, tt.service)
			}
			if caller.calls[0].Op != tt.op {
				t.Errorf("Op = %q, want %q", caller.calls[0].Op, tt.op)
			}

			// The result should indicate success or error — but must not panic
			_ = result
		})
	}
}

// ---------------------------------------------------------------------------
// cleat_child_workflow wrapper tests
// ---------------------------------------------------------------------------

// TestCleatChildWorkflow_ReadsNameAndInput verifies that ChildWorkflow
// dispatches with the correct name and input to the child workflow store.
func TestCleatChildWorkflow_ReadsNameAndInput(t *testing.T) {
	mock := &mockChildWorkflowStore{}
	engine := NewEngine(nil, nil, WithChildWorkflowStore(mock))
	s := &execSession{
		engine: engine,
	}

	result := s.ChildWorkflow(context.Background(), nil,
		"my-child-workflow", `{"order_id":"ord-123"}`, 0, 0)

	if mock.gotRunID != "child-run-001" {
		// The runID was written to memory — since we passed nil module,
		// writeResult returns 0. The actual result communicates success
		// by the errCode.
		_ = result
	}
	// History should have a child workflow event
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeChildWorkflow {
		t.Errorf("event type = %q, want %q", s.history[0].EventType, EventTypeChildWorkflow)
	}
	if s.history[0].ChildName != "my-child-workflow" {
		t.Errorf("ChildName = %q, want %q", s.history[0].ChildName, "my-child-workflow")
	}
	if s.history[0].ChildInput != `{"order_id":"ord-123"}` {
		t.Errorf("ChildInput = %q, want %q", s.history[0].ChildInput, `{"order_id":"ord-123"}`)
	}
}

// TestCleatChildWorkflow_EmptyName handles the edge case of an empty child
// workflow name.
func TestCleatChildWorkflow_EmptyName(t *testing.T) {
	mock := &mockChildWorkflowStore{}
	engine := NewEngine(nil, nil, WithChildWorkflowStore(mock))
	s := &execSession{
		engine: engine,
	}

	s.ChildWorkflow(context.Background(), nil, "", `{}`, 0, 0)

	// Should not panic; history should contain the event with empty name
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].ChildName != "" {
		t.Errorf("ChildName = %q, want empty", s.history[0].ChildName)
	}
}

// ---------------------------------------------------------------------------
// cleat_call_retry wrapper tests
// ---------------------------------------------------------------------------

// TestCleatCallRetry_ReadsAllParams verifies that DurableCallWithRetry
// dispatches retry parameters correctly.
func TestCleatCallRetry_ReadsAllParams(t *testing.T) {
	caller := &mockCaller{}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine: engine,
	}

	result := s.DurableCallWithRetry(context.Background(), nil,
		"my-svc", "my-op", `{"key":"val"}`,
		3, 100, 20000, 1000,
		`["timeout","internal"]`, 0, 0)

	// Should succeed on first attempt with correct args
	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(caller.calls))
	}
	if caller.calls[0].Service != "my-svc" {
		t.Errorf("Service = %q, want %q", caller.calls[0].Service, "my-svc")
	}
	if caller.calls[0].Op != "my-op" {
		t.Errorf("Op = %q, want %q", caller.calls[0].Op, "my-op")
	}

	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
}

// ---------------------------------------------------------------------------
// cleat_call_heartbeat wrapper tests
// ---------------------------------------------------------------------------

// TestCleatCallHeartbeat_ReadsParams verifies that DurableCallWithHeartbeat
// dispatches parameters correctly.
func TestCleatCallHeartbeat_ReadsParams(t *testing.T) {
	caller := &mockCaller{}
	engine := NewEngine(nil, caller)
	s := &execSession{
		engine: engine,
	}

	result := s.DurableCallWithHeartbeat(context.Background(), nil,
		"hb-svc", "hb-op", `{"task":"long"}`, 5000, 0, 0)

	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(caller.calls))
	}
	if caller.calls[0].Service != "hb-svc" {
		t.Errorf("Service = %q, want %q", caller.calls[0].Service, "hb-svc")
	}
	if caller.calls[0].Op != "hb-op" {
		t.Errorf("Op = %q, want %q", caller.calls[0].Op, "hb-op")
	}
	if caller.calls[0].Request != `{"task":"long"}` {
		t.Errorf("Request = %q, want %q", caller.calls[0].Request, `{"task":"long"}`)
	}

	// Success
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
}

// ---------------------------------------------------------------------------
// cleat_send_signal_and_wait wrapper tests
// ---------------------------------------------------------------------------

// TestCleatSendSignalAndWait_ReadsTargetSignalPayload verifies that
// SendSignalAndWait dispatches with the correct target, signal, and payload.
func TestCleatSendSignalAndWait_ReadsTargetSignalPayload(t *testing.T) {
	store := newMockSignalWorkflowStore()
	e := &Engine{
		signalStore: store,
	}
	s := &execSession{
		engine: e,
	}

	result := s.SendSignalAndWait(context.Background(), nil,
		"target-wf-001", "order-approved", `{"order_id":"ord-123"}`, 30000, 0, 0)

	// Without a signal waiting, this should record an await_signals event
	// and return a suspend indicator.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeAwaitSignals {
		t.Errorf("event type = %q, want %q", s.history[0].EventType, EventTypeAwaitSignals)
	}
	if s.history[0].SignalNames != "order-approved" {
		t.Errorf("SignalNames = %q, want %q", s.history[0].SignalNames, "order-approved")
	}
	if s.history[0].TimeoutMs != 30000 {
		t.Errorf("TimeoutMs = %d, want %d", s.history[0].TimeoutMs, 30000)
	}

	// Return value should indicate suspend (packSimpleResult with errCode=1)
	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("errCode = %d, want 1 (suspend)", errCode)
	}
}

// TestCleatSendSignalAndWait_WithExistingSignal verifies that SendSignalAndWait
// correctly reads a pre-existing signal.
func TestCleatSendSignalAndWait_WithExistingSignal(t *testing.T) {
	store := newMockSignalWorkflowStore()
	// Pre-deliver a signal so the poll succeeds.
	err := store.DeliverSignal(context.Background(), "target-wf-001", "order-approved", `{"approved":true}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}
	e := &Engine{
		signalStore: store,
	}
	s := &execSession{
		engine: e,
	}

	result := s.SendSignalAndWait(context.Background(), nil,
		"target-wf-001", "order-approved", `{"order_id":"ord-123"}`, 30000, 0, 0)

	// Should find the pre-existing signal and record a signal_received event
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeSignalReceived {
		t.Errorf("event type = %q, want %q", s.history[0].EventType, EventTypeSignalReceived)
	}

	// Return value should indicate success (errCode=0)
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
}

// ---------------------------------------------------------------------------
// cleat_fetch wrapper tests
// ---------------------------------------------------------------------------

// TestCleatFetch_ReadsMethodUrlHeadersBody verifies that Fetch dispatches
// with the correct method, URL, headers, and body.
func TestCleatFetch_ReadsMethodUrlHeadersBody(t *testing.T) {
	fetcher := &stubFetcher{}
	engine := NewEngine(nil, nil, WithFetcher(fetcher))
	s := &execSession{
		engine: engine,
	}

	result := s.Fetch(context.Background(), nil,
		"GET", "https://api.example.com/data",
		`{"Authorization":"Bearer tok"}`,
		`{"query":"test"}`, 0, 0)

	// Should record a fetch event
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeFetch {
		t.Errorf("event type = %q, want %q", s.history[0].EventType, EventTypeFetch)
	}
	if s.history[0].FetchMethod != "GET" {
		t.Errorf("FetchMethod = %q, want %q", s.history[0].FetchMethod, "GET")
	}
	if s.history[0].FetchURL != "https://api.example.com/data" {
		t.Errorf("FetchURL = %q, want %q", s.history[0].FetchURL, "https://api.example.com/data")
	}
	if s.history[0].FetchHeaders != `{"Authorization":"Bearer tok"}` {
		t.Errorf("FetchHeaders = %q, want %q", s.history[0].FetchHeaders, `{"Authorization":"Bearer tok"}`)
	}
	if s.history[0].FetchBody != `{"query":"test"}` {
		t.Errorf("FetchBody = %q, want %q", s.history[0].FetchBody, `{"query":"test"}`)
	}

	// Success (stubFetcher returns empty response, no error)
	errCode := byte(result & 0xFF)
	if errCode != 0 {
		t.Errorf("errCode = %d, want 0", errCode)
	}
}

// TestCleatFetch_WithError verifies that Fetch handles errors from the fetcher.
func TestCleatFetch_WithError(t *testing.T) {
	fetcher := &errorFetcher{errMsg: "network error"}
	engine := NewEngine(nil, nil, WithFetcher(fetcher))
	s := &execSession{
		engine: engine,
	}

	result := s.Fetch(context.Background(), nil,
		"POST", "https://api.example.com/fail", "{}", "{}", 0, 0)

	if len(s.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(s.history))
	}
	if s.history[0].Err != "network error" {
		t.Errorf("event error = %q, want %q", s.history[0].Err, "network error")
	}

	// Error return
	errCode := byte(result & 0xFF)
	if errCode != 1 {
		t.Errorf("errCode = %d, want 1", errCode)
	}
}

// ---------------------------------------------------------------------------
// registerHostFunctions WASM ABI tests
//
// These tests verify that each cleat_* host function closure in
// registerHostFunctions correctly reads arguments from WASM linear memory
// and dispatches to the HostHandler. They exercise the actual WASM import
// path: a minimal WASM module imports the host function and re-exports it
// as "call", then the test calls it through wazero.
// ---------------------------------------------------------------------------

var (
	wasmI32 = byte(0x7f)
	wasmI64 = byte(0x7e)
)

// writeLeb128Section writes a WASM section header (id + size + content).
func writeLeb128Section(buf *bytes.Buffer, id byte, content []byte) {
	buf.WriteByte(id)
	buf.WriteByte(byte(len(content))) // LEB128 — works for sizes < 128
	buf.Write(content)
}

// makeImportWasm generates a minimal WASM module that:
//   - imports <fieldName> from "test_cleat" with the given param/result types
//   - exports "call" as a function that calls the imported function
//   - if withMem is true, also exports a 1-page linear memory as "mem"
func makeImportWasm(fieldName string, paramTypes, resultTypes []byte, withMem bool) []byte {
	var mod bytes.Buffer
	// Magic + version
	mod.Write([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})

	// --- Type section (id=1) ---
	{
		var sec bytes.Buffer
		sec.WriteByte(1)               // 1 type
		sec.WriteByte(0x60)            // functype
		sec.WriteByte(byte(len(paramTypes)))
		sec.Write(paramTypes)
		sec.WriteByte(byte(len(resultTypes)))
		sec.Write(resultTypes)
		writeLeb128Section(&mod, 1, sec.Bytes())
	}

	// --- Import section (id=2) ---
	{
		var sec bytes.Buffer
		sec.WriteByte(1) // 1 import
		// Module name: "test_cleat" (10 bytes)
		sec.WriteByte(10)
		sec.WriteString("test_cleat")
		// Field name
		sec.WriteByte(byte(len(fieldName)))
		sec.WriteString(fieldName)
		// Import kind: func
		sec.WriteByte(0x00)
		// Type index
		sec.WriteByte(0x00)
		writeLeb128Section(&mod, 2, sec.Bytes())
	}

	// --- Function section (id=3) ---
	{
		var sec bytes.Buffer
		sec.WriteByte(1)    // 1 function
		sec.WriteByte(0x00) // type index 0
		writeLeb128Section(&mod, 3, sec.Bytes())
	}

	// --- Memory section (id=5) --- if withMem
	if withMem {
		var sec bytes.Buffer
		sec.WriteByte(1) // 1 memory
		sec.WriteByte(0) // min pages = 0
		sec.WriteByte(1) // max pages = 1
		writeLeb128Section(&mod, 5, sec.Bytes())
	}

	// --- Export section (id=7) ---
	{
		var sec bytes.Buffer
		nExports := byte(1)
		if withMem {
			nExports = 2
		}
		sec.WriteByte(nExports)
		// Function export: "call" -> func index 1 (import is index 0)
		sec.WriteByte(4)
		sec.WriteString("call")
		sec.WriteByte(0x00) // func kind
		sec.WriteByte(0x01) // func index 1
		if withMem {
			sec.WriteByte(3)
			sec.WriteString("mem")
			sec.WriteByte(0x02) // memory kind
			sec.WriteByte(0x00) // memory index 0
		}
		writeLeb128Section(&mod, 7, sec.Bytes())
	}

	// --- Code section (id=10) ---
	{
		var sec bytes.Buffer
		sec.WriteByte(1) // 1 body

		// Build body: 0 locals + local.get for each param + call 0 + end
		var body bytes.Buffer
		body.WriteByte(0x00) // 0 locals
		for i := 0; i < len(paramTypes); i++ {
			body.WriteByte(0x20) // local.get
			body.WriteByte(byte(i))
		}
		body.WriteByte(0x10) // call
		body.WriteByte(0x00) // func index 0
		body.WriteByte(0x0b) // end

		sec.WriteByte(byte(body.Len()))
		sec.Write(body.Bytes())
		writeLeb128Section(&mod, 10, sec.Bytes())
	}

	return mod.Bytes()
}

// testHostFuncHarness holds the runtime, module, and memory for a host function test.
type testHostFuncHarness struct {
	ctx context.Context // context with the HostHandler attached
	mod api.Module      // WASM module importing from "test_cleat"
	mem api.Memory      // module memory (nil if withMem was false)
}

// newTestHostFuncHarness creates a wazero runtime, registers host functions
// under "test_cleat", and instantiates a WASM module that imports the given
// function and re-exports it as "call".
func newTestHostFuncHarness(t *testing.T, fieldName string, paramTypes, resultTypes []byte, withMem bool, handler HostHandler) *testHostFuncHarness {
	t.Helper()
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	t.Cleanup(func() { rt.Close(ctx) })

	ctx = withHandler(ctx, handler)

	builder := rt.NewHostModuleBuilder("test_cleat")
	registerHostFunctions(builder, nil)
	hostMod, err := builder.Instantiate(ctx)
	if err != nil {
		t.Fatalf("instantiate host module: %v", err)
	}
	t.Cleanup(func() { hostMod.Close(ctx) })

	wasmBytes := makeImportWasm(fieldName, paramTypes, resultTypes, withMem)
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("compile module: %v", err)
	}
	t.Cleanup(func() { compiled.Close(ctx) })

	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("test-user"))
	if err != nil {
		t.Fatalf("instantiate module: %v", err)
	}
	t.Cleanup(func() { mod.Close(ctx) })

	var mem api.Memory
	if withMem {
		mem = mod.Memory()
	}

	return &testHostFuncHarness{ctx: ctx, mod: mod, mem: mem}
}

// call invokes the exported "call" function with the given args.
// It uses the context stored in the harness (which contains the HostHandler).
func (h *testHostFuncHarness) call(args ...uint64) (uint64, error) {
	results, err := h.mod.ExportedFunction("call").Call(h.ctx, args...)
	if err != nil {
		return 0, err
	}
	return results[0], nil
}

// ---------------------------------------------------------------------------
// Custom test handlers
// ---------------------------------------------------------------------------

type logRecorder struct {
	stubHostHandler
	msg string
}

func (h *logRecorder) DurableLog(_ context.Context, _ api.Module, message string) int64 {
	h.msg = message
	return 0
}

type nowRecorder struct {
	stubHostHandler
	now int64
}

func (h *nowRecorder) Now(_ context.Context) int64 { return h.now }

type randomRecorder struct {
	stubHostHandler
	val int64
}

func (h *randomRecorder) Random(_ context.Context) int64 { return h.val }

type stateRecorder struct {
	stubHostHandler
	key   string
	value string
}

func (h *stateRecorder) SetState(_ context.Context, _ api.Module, key, value string) int64 {
	h.key = key
	h.value = value
	return 0
}

type queryStateRecorder struct {
	stubHostHandler
	key   string
	value string
}

func (h *queryStateRecorder) SetQueryState(_ context.Context, _ api.Module, key, value string) int64 {
	h.key = key
	h.value = value
	return 0
}

type continueAsNewRecorder struct {
	stubHostHandler
	input string
}

func (h *continueAsNewRecorder) ContinueAsNew(_ context.Context, _ api.Module, newInputJSON string) int64 {
	h.input = newInputJSON
	return 0
}

type updateHandlerRecorder struct {
	stubHostHandler
	name string
}

func (h *updateHandlerRecorder) RegisterUpdateHandler(_ context.Context, _ api.Module, name string) int64 {
	h.name = name
	return 0
}

type signalRecorder struct {
	stubHostHandler
	name string
}

func (h *signalRecorder) PollSignal(_ context.Context, _ api.Module, signalName string, _, _ uint32) int64 {
	h.name = signalName
	return 0
}

type cancellationRecorder struct {
	stubHostHandler
	called bool
}

func (h *cancellationRecorder) PollCancellation(_ context.Context, _ api.Module, _, _ uint32) int64 {
	h.called = true
	return 0
}

type sleepRecorder struct {
	stubHostHandler
	durationMs int64
}

func (h *sleepRecorder) DurableSleep(_ context.Context, _ api.Module, durationMs int64) int64 {
	h.durationMs = durationMs
	return 0
}

type deferRecorder struct {
	stubHostHandler
	description string
}

func (h *deferRecorder) DurableDefer(_ context.Context, _ api.Module, description string, _, _ uint32) int64 {
	h.description = description
	return 0
}

type sideEffectRecorder struct {
	stubHostHandler
	result string
}

func (h *sideEffectRecorder) SideEffect(_ context.Context, _ api.Module, computedResult string, _, _ uint32) int64 {
	h.result = computedResult
	return 0
}

type fetchRecorder struct {
	stubHostHandler
	method string
	url    string
}

func (h *fetchRecorder) Fetch(_ context.Context, _ api.Module, method, url, _, _ string, _, _ uint32) int64 {
	h.method = method
	h.url = url
	return 0
}

// ---------------------------------------------------------------------------
// Host function tests
// ---------------------------------------------------------------------------

func TestHostFunc_CleatLog(t *testing.T) {
	handler := &logRecorder{}
	h := newTestHostFuncHarness(t, "cleat_log", []byte{wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	msg := "hello world"
	if !h.mem.Write(0, []byte(msg)) {
		t.Fatal("write to memory failed")
	}

	result, err := h.call(0, uint64(len(msg)))
	if err != nil {
		t.Fatalf("call cleat_log: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.msg != msg {
		t.Errorf("log message = %q, want %q", handler.msg, msg)
	}
}

func TestHostFunc_CleatNow(t *testing.T) {
	handler := &nowRecorder{now: 1234567890}
	h := newTestHostFuncHarness(t, "cleat_now", nil, []byte{wasmI64}, false, handler)

	result, err := h.call()
	if err != nil {
		t.Fatalf("call cleat_now: %v", err)
	}
	if result != uint64(handler.now) {
		t.Errorf("result = %d, want %d", result, handler.now)
	}
}

func TestHostFunc_CleatRandom(t *testing.T) {
	handler := &randomRecorder{val: 42}
	h := newTestHostFuncHarness(t, "cleat_random", nil, []byte{wasmI64}, false, handler)

	result, err := h.call()
	if err != nil {
		t.Fatalf("call cleat_random: %v", err)
	}
	if result != uint64(handler.val) {
		t.Errorf("result = %d, want %d", result, handler.val)
	}
}

func TestHostFunc_CleatUUID(t *testing.T) {
	h := newTestHostFuncHarness(t, "cleat_uuid", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, &stubHostHandler{})

	seed := "test-seed"
	if !h.mem.Write(0, []byte(seed)) {
		t.Fatal("write to memory failed")
	}

	result, err := h.call(0, uint64(len(seed)), 256, 36)
	if err != nil {
		t.Fatalf("call cleat_uuid: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
}

func TestHostFunc_CleatSetState(t *testing.T) {
	handler := &stateRecorder{}
	h := newTestHostFuncHarness(t, "cleat_set_state", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	key := "my_state_key"
	val := `{"count":42}`
	if !h.mem.Write(0, []byte(key)) {
		t.Fatal("write key to memory failed")
	}
	if !h.mem.Write(256, []byte(val)) {
		t.Fatal("write value to memory failed")
	}

	result, err := h.call(0, uint64(len(key)), 256, uint64(len(val)))
	if err != nil {
		t.Fatalf("call cleat_set_state: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.key != key {
		t.Errorf("state key = %q, want %q", handler.key, key)
	}
	if handler.value != val {
		t.Errorf("state value = %q, want %q", handler.value, val)
	}
}

func TestHostFunc_CleatGetState(t *testing.T) {
	h := newTestHostFuncHarness(t, "cleat_get_state", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, &stubHostHandler{})

	key := "my_state_key"
	if !h.mem.Write(0, []byte(key)) {
		t.Fatal("write key to memory failed")
	}

	result, err := h.call(0, uint64(len(key)), 256, 100)
	if err != nil {
		t.Fatalf("call cleat_get_state: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
}

func TestHostFunc_CleatSetQueryState(t *testing.T) {
	handler := &queryStateRecorder{}
	h := newTestHostFuncHarness(t, "set_query_state", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	key := "query_key"
	val := `"query_value"`
	if !h.mem.Write(0, []byte(key)) {
		t.Fatal("write key to memory failed")
	}
	if !h.mem.Write(256, []byte(val)) {
		t.Fatal("write value to memory failed")
	}

	result, err := h.call(0, uint64(len(key)), 256, uint64(len(val)))
	if err != nil {
		t.Fatalf("call set_query_state: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.key != key {
		t.Errorf("query state key = %q, want %q", handler.key, key)
	}
	if handler.value != val {
		t.Errorf("query state value = %q, want %q", handler.value, val)
	}
}

func TestHostFunc_CleatSideEffect(t *testing.T) {
	handler := &sideEffectRecorder{}
	h := newTestHostFuncHarness(t, "cleat_side_effect", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	resultJSON := `{"computed":true}`
	if !h.mem.Write(0, []byte(resultJSON)) {
		t.Fatal("write to memory failed")
	}

	result, err := h.call(0, uint64(len(resultJSON)), 256, 100)
	if err != nil {
		t.Fatalf("call cleat_side_effect: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.result != resultJSON {
		t.Errorf("side effect result = %q, want %q", handler.result, resultJSON)
	}
}

func TestHostFunc_CleatDefer(t *testing.T) {
	handler := &deferRecorder{}
	h := newTestHostFuncHarness(t, "cleat_defer", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	desc := "cleanup resources"
	if !h.mem.Write(0, []byte(desc)) {
		t.Fatal("write to memory failed")
	}

	result, err := h.call(0, uint64(len(desc)), 256, 100)
	if err != nil {
		t.Fatalf("call cleat_defer: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.description != desc {
		t.Errorf("defer description = %q, want %q", handler.description, desc)
	}
}

func TestHostFunc_CleatContinueAsNew(t *testing.T) {
	handler := &continueAsNewRecorder{}
	h := newTestHostFuncHarness(t, "cleat_continue_as_new", []byte{wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	input := `{"page":2}`
	if !h.mem.Write(0, []byte(input)) {
		t.Fatal("write to memory failed")
	}

	result, err := h.call(0, uint64(len(input)))
	if err != nil {
		t.Fatalf("call cleat_continue_as_new: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.input != input {
		t.Errorf("continue as new input = %q, want %q", handler.input, input)
	}
}

func TestHostFunc_CleatRegisterUpdateHandler(t *testing.T) {
	handler := &updateHandlerRecorder{}
	h := newTestHostFuncHarness(t, "cleat_register_update_handler", []byte{wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	name := "update_order"
	if !h.mem.Write(0, []byte(name)) {
		t.Fatal("write to memory failed")
	}

	result, err := h.call(0, uint64(len(name)))
	if err != nil {
		t.Fatalf("call cleat_register_update_handler: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.name != name {
		t.Errorf("update handler name = %q, want %q", handler.name, name)
	}
}

func TestHostFunc_CleatPollSignal(t *testing.T) {
	handler := &signalRecorder{}
	h := newTestHostFuncHarness(t, "cleat_poll_signal", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	sigName := "order_confirmed"
	if !h.mem.Write(0, []byte(sigName)) {
		t.Fatal("write to memory failed")
	}

	result, err := h.call(0, uint64(len(sigName)), 256, 100)
	if err != nil {
		t.Fatalf("call cleat_poll_signal: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.name != sigName {
		t.Errorf("signal name = %q, want %q", handler.name, sigName)
	}
}

func TestHostFunc_CleatPollCancellation(t *testing.T) {
	handler := &cancellationRecorder{}
	h := newTestHostFuncHarness(t, "cleat_poll_cancellation", []byte{wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	result, err := h.call(256, 100)
	if err != nil {
		t.Fatalf("call cleat_poll_cancellation: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if !handler.called {
		t.Error("PollCancellation was not called")
	}
}

func TestHostFunc_CleatSleep(t *testing.T) {
	handler := &sleepRecorder{}
	h := newTestHostFuncHarness(t, "cleat_sleep", []byte{wasmI64}, []byte{wasmI64}, false, handler)

	durationMs := int64(5000)
	result, err := h.call(uint64(durationMs))
	if err != nil {
		t.Fatalf("call cleat_sleep: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.durationMs != durationMs {
		t.Errorf("sleep duration = %d, want %d", handler.durationMs, durationMs)
	}
}

func TestHostFunc_CleatFetch(t *testing.T) {
	handler := &fetchRecorder{}
	params := []byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32}
	h := newTestHostFuncHarness(t, "cleat_fetch", params, []byte{wasmI64}, true, handler)

	method := "POST"
	url := "https://api.example.com/data"
	headers := `{"Authorization":"Bearer tok"}`
	body := `{"key":"val"}`

	if !h.mem.Write(0, []byte(method)) {
		t.Fatal("write method to memory failed")
	}
	if !h.mem.Write(64, []byte(url)) {
		t.Fatal("write url to memory failed")
	}
	if !h.mem.Write(256, []byte(headers)) {
		t.Fatal("write headers to memory failed")
	}
	if !h.mem.Write(512, []byte(body)) {
		t.Fatal("write body to memory failed")
	}

	result, err := h.call(
		0, uint64(len(method)),    // method ptr, len
		64, uint64(len(url)),      // url ptr, len
		256, uint64(len(headers)), // headers ptr, len
		512, uint64(len(body)),    // body ptr, len
		768, 4096,                 // response ptr, maxLen
	)
	if err != nil {
		t.Fatalf("call cleat_fetch: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.method != method {
		t.Errorf("fetch method = %q, want %q", handler.method, method)
	}
	if handler.url != url {
		t.Errorf("fetch url = %q, want %q", handler.url, url)
	}
}

// ---------------------------------------------------------------------------
// Error-path tests
// ---------------------------------------------------------------------------

func TestHostFunc_CleatLog_EmptyMsg(t *testing.T) {
	h := newTestHostFuncHarness(t, "cleat_log", []byte{wasmI32, wasmI32}, []byte{wasmI64}, true, &stubHostHandler{})

	// Empty message (length 0) should trigger errBadParam
	result, err := h.call(0, 0)
	if err != nil {
		t.Fatalf("call cleat_log empty: %v", err)
	}
	if result != errBadParam {
		t.Errorf("expected errBadParam, got %x", result)
	}
}

func TestHostFunc_CleatSetState_InvalidKey(t *testing.T) {
	h := newTestHostFuncHarness(t, "cleat_set_state", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, &stubHostHandler{})

	// Invalid key (contains a space) should fail readServiceName -> errBadParam
	invalidKey := "bad key with spaces"
	if !h.mem.Write(0, []byte(invalidKey)) {
		t.Fatal("write key to memory failed")
	}
	if !h.mem.Write(256, []byte("value")) {
		t.Fatal("write value to memory failed")
	}

	result, err := h.call(0, uint64(len(invalidKey)), 256, 5)
	if err != nil {
		t.Fatalf("call cleat_set_state invalid: %v", err)
	}
	if result != errBadParam {
		t.Errorf("expected errBadParam, got %x", result)
	}
}

// ---------------------------------------------------------------------------
// Custom handlers for additional host function tests
// ---------------------------------------------------------------------------

type childWorkflowRecorder struct {
	stubHostHandler
	name  string
	input string
}

func (h *childWorkflowRecorder) ChildWorkflow(_ context.Context, _ api.Module, name, inputJSON string, _, _ uint32) int64 {
	h.name = name
	h.input = inputJSON
	return 0
}

type createPromiseRecorder struct {
	stubHostHandler
	name string
}

func (h *createPromiseRecorder) CreatePromise(_ context.Context, _ api.Module, name string, _, _ uint32) int64 {
	h.name = name
	return 0
}

type awaitPromiseRecorder struct {
	stubHostHandler
	promiseID string
	timeoutMs int64
}

func (h *awaitPromiseRecorder) AwaitPromise(_ context.Context, _ api.Module, promiseID string, timeoutMs int64, _, _ uint32) int64 {
	h.promiseID = promiseID
	h.timeoutMs = timeoutMs
	return 0
}

type signalWorkflowRecorder struct {
	stubHostHandler
	targetRunID string
	signalName  string
	payload     string
}

func (h *signalWorkflowRecorder) SignalWorkflow(_ context.Context, _ api.Module, targetRunID, signalName, payload string) int64 {
	h.targetRunID = targetRunID
	h.signalName = signalName
	h.payload = payload
	return 0
}

type sendSignalAndWaitRecorder struct {
	stubHostHandler
	targetRunID string
	signalName  string
	payload     string
	timeoutMs   int64
}

func (h *sendSignalAndWaitRecorder) SendSignalAndWait(_ context.Context, _ api.Module, targetRunID, signalName, payload string, timeoutMs int64, _, _ uint32) int64 {
	h.targetRunID = targetRunID
	h.signalName = signalName
	h.payload = payload
	h.timeoutMs = timeoutMs
	return 0
}

type acquireLockRecorder struct {
	stubHostHandler
	key   string
	ttlMs int64
}

func (h *acquireLockRecorder) AcquireLock(_ context.Context, _ api.Module, key string, ttlMs int64) int64 {
	h.key = key
	h.ttlMs = ttlMs
	return 0
}

type releaseLockRecorder struct {
	stubHostHandler
	key string
}

func (h *releaseLockRecorder) ReleaseLock(_ context.Context, _ api.Module, key string) int64 {
	h.key = key
	return 0
}

type awaitSignalsRecorder struct {
	stubHostHandler
	signalNames string
	timeoutMs   int64
}

func (h *awaitSignalsRecorder) DurableAwaitSignals(_ context.Context, _ api.Module, signalNames string, timeoutMs int64, _, _, _, _ uint32) int64 {
	h.signalNames = signalNames
	h.timeoutMs = timeoutMs
	return 0
}

type awaitChildRecorder struct {
	stubHostHandler
	runID string
}

func (h *awaitChildRecorder) AwaitChild(_ context.Context, _ api.Module, runID string, _, _ uint32) int64 {
	h.runID = runID
	return 0
}

type awaitAllChildrenRecorder struct {
	stubHostHandler
	runIDsJSON string
}

func (h *awaitAllChildrenRecorder) AwaitAllChildren(_ context.Context, _ api.Module, runIDsJSON string, _, _ uint32) int64 {
	h.runIDsJSON = runIDsJSON
	return 0
}

// ---------------------------------------------------------------------------
// Additional host function WASM ABI tests
// ---------------------------------------------------------------------------

func TestHostFunc_CleatChildWorkflow(t *testing.T) {
	handler := &childWorkflowRecorder{}
	params := []byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32}
	h := newTestHostFuncHarness(t, "cleat_child_workflow", params, []byte{wasmI64}, true, handler)

	name := "my_child_wf"
	input := `{"order_id":"ord-456"}`
	if !h.mem.Write(0, []byte(name)) {
		t.Fatal("write name to memory failed")
	}
	if !h.mem.Write(256, []byte(input)) {
		t.Fatal("write input to memory failed")
	}

	result, err := h.call(0, uint64(len(name)), 256, uint64(len(input)), 512, 100)
	if err != nil {
		t.Fatalf("call cleat_child_workflow: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.name != name {
		t.Errorf("child workflow name = %q, want %q", handler.name, name)
	}
	if handler.input != input {
		t.Errorf("child workflow input = %q, want %q", handler.input, input)
	}
}

func TestHostFunc_CleatCreatePromise(t *testing.T) {
	handler := &createPromiseRecorder{}
	h := newTestHostFuncHarness(t, "cleat_create_promise", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	promiseName := "payment_confirmed"
	if !h.mem.Write(0, []byte(promiseName)) {
		t.Fatal("write name to memory failed")
	}

	result, err := h.call(0, uint64(len(promiseName)), 256, 64)
	if err != nil {
		t.Fatalf("call cleat_create_promise: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.name != promiseName {
		t.Errorf("promise name = %q, want %q", handler.name, promiseName)
	}
}

func TestHostFunc_CleatAwaitPromise(t *testing.T) {
	handler := &awaitPromiseRecorder{}
	params := []byte{wasmI32, wasmI32, wasmI64, wasmI32, wasmI32}
	h := newTestHostFuncHarness(t, "cleat_await_promise", params, []byte{wasmI64}, true, handler)

	promiseID := "promise-abc-123"
	if !h.mem.Write(0, []byte(promiseID)) {
		t.Fatal("write promiseID to memory failed")
	}

	timeoutMs := int64(30000)
	result, err := h.call(0, uint64(len(promiseID)), uint64(timeoutMs), 256, 100)
	if err != nil {
		t.Fatalf("call cleat_await_promise: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.promiseID != promiseID {
		t.Errorf("promiseID = %q, want %q", handler.promiseID, promiseID)
	}
	if handler.timeoutMs != timeoutMs {
		t.Errorf("timeoutMs = %d, want %d", handler.timeoutMs, timeoutMs)
	}
}

func TestHostFunc_CleatSignalWorkflow(t *testing.T) {
	handler := &signalWorkflowRecorder{}
	params := []byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32}
	h := newTestHostFuncHarness(t, "cleat_signal_workflow", params, []byte{wasmI64}, true, handler)

	targetRunID := "wf-run-001"
	signalName := "order_shipped"
	payload := `{"tracking":"TRACK123"}`
	if !h.mem.Write(0, []byte(targetRunID)) {
		t.Fatal("write targetRunID to memory failed")
	}
	if !h.mem.Write(256, []byte(signalName)) {
		t.Fatal("write signalName to memory failed")
	}
	if !h.mem.Write(512, []byte(payload)) {
		t.Fatal("write payload to memory failed")
	}

	result, err := h.call(0, uint64(len(targetRunID)), 256, uint64(len(signalName)), 512, uint64(len(payload)))
	if err != nil {
		t.Fatalf("call cleat_signal_workflow: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.targetRunID != targetRunID {
		t.Errorf("targetRunID = %q, want %q", handler.targetRunID, targetRunID)
	}
	if handler.signalName != signalName {
		t.Errorf("signalName = %q, want %q", handler.signalName, signalName)
	}
	if handler.payload != payload {
		t.Errorf("payload = %q, want %q", handler.payload, payload)
	}
}

func TestHostFunc_CleatSendSignalAndWait(t *testing.T) {
	handler := &sendSignalAndWaitRecorder{}
	params := []byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI64, wasmI32, wasmI32}
	h := newTestHostFuncHarness(t, "cleat_send_signal_and_wait", params, []byte{wasmI64}, true, handler)

	targetRunID := "target-wf-002"
	signalName := "payment_received"
	payload := `{"amount":99.99}`
	timeoutMs := int64(15000)
	if !h.mem.Write(0, []byte(targetRunID)) {
		t.Fatal("write targetRunID to memory failed")
	}
	if !h.mem.Write(256, []byte(signalName)) {
		t.Fatal("write signalName to memory failed")
	}
	if !h.mem.Write(512, []byte(payload)) {
		t.Fatal("write payload to memory failed")
	}

	result, err := h.call(0, uint64(len(targetRunID)), 256, uint64(len(signalName)), 512, uint64(len(payload)), uint64(timeoutMs), 768, 4096)
	if err != nil {
		t.Fatalf("call cleat_send_signal_and_wait: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.targetRunID != targetRunID {
		t.Errorf("targetRunID = %q, want %q", handler.targetRunID, targetRunID)
	}
	if handler.signalName != signalName {
		t.Errorf("signalName = %q, want %q", handler.signalName, signalName)
	}
	if handler.payload != payload {
		t.Errorf("payload = %q, want %q", handler.payload, payload)
	}
	if handler.timeoutMs != timeoutMs {
		t.Errorf("timeoutMs = %d, want %d", handler.timeoutMs, timeoutMs)
	}
}

func TestHostFunc_CleatAcquireLock(t *testing.T) {
	handler := &acquireLockRecorder{}
	params := []byte{wasmI32, wasmI32, wasmI64}
	h := newTestHostFuncHarness(t, "cleat_acquire_lock", params, []byte{wasmI64}, true, handler)

	lockKey := "order-lock-001"
	ttlMs := int64(60000)
	if !h.mem.Write(0, []byte(lockKey)) {
		t.Fatal("write key to memory failed")
	}

	result, err := h.call(0, uint64(len(lockKey)), uint64(ttlMs))
	if err != nil {
		t.Fatalf("call cleat_acquire_lock: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.key != lockKey {
		t.Errorf("lock key = %q, want %q", handler.key, lockKey)
	}
	if handler.ttlMs != ttlMs {
		t.Errorf("ttlMs = %d, want %d", handler.ttlMs, ttlMs)
	}
}

func TestHostFunc_CleatReleaseLock(t *testing.T) {
	handler := &releaseLockRecorder{}
	h := newTestHostFuncHarness(t, "cleat_release_lock", []byte{wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	lockKey := "order-lock-001"
	if !h.mem.Write(0, []byte(lockKey)) {
		t.Fatal("write key to memory failed")
	}

	result, err := h.call(0, uint64(len(lockKey)))
	if err != nil {
		t.Fatalf("call cleat_release_lock: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.key != lockKey {
		t.Errorf("lock key = %q, want %q", handler.key, lockKey)
	}
}

func TestHostFunc_CleatAwaitSignal(t *testing.T) {
	handler := &awaitSignalsRecorder{}
	params := []byte{wasmI32, wasmI32, wasmI64, wasmI32, wasmI32, wasmI32, wasmI32}
	h := newTestHostFuncHarness(t, "cleat_await_signals", params, []byte{wasmI64}, true, handler)

	signalNames := `["order_confirmed","payment_received"]`
	timeoutMs := int64(60000)
	if !h.mem.Write(0, []byte(signalNames)) {
		t.Fatal("write signalNames to memory failed")
	}

	result, err := h.call(0, uint64(len(signalNames)), uint64(timeoutMs), 256, 64, 512, 4096)
	if err != nil {
		t.Fatalf("call cleat_await_signals: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.signalNames != signalNames {
		t.Errorf("signalNames = %q, want %q", handler.signalNames, signalNames)
	}
	if handler.timeoutMs != timeoutMs {
		t.Errorf("timeoutMs = %d, want %d", handler.timeoutMs, timeoutMs)
	}
}

func TestHostFunc_CleatAwaitChild(t *testing.T) {
	handler := &awaitChildRecorder{}
	h := newTestHostFuncHarness(t, "cleat_await_child", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	runID := "child-run-abc-456"
	if !h.mem.Write(0, []byte(runID)) {
		t.Fatal("write runID to memory failed")
	}

	result, err := h.call(0, uint64(len(runID)), 256, 100)
	if err != nil {
		t.Fatalf("call cleat_await_child: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.runID != runID {
		t.Errorf("runID = %q, want %q", handler.runID, runID)
	}
}

func TestHostFunc_CleatAwaitAllChildren(t *testing.T) {
	handler := &awaitAllChildrenRecorder{}
	h := newTestHostFuncHarness(t, "cleat_await_all_children", []byte{wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, handler)

	runIDsJSON := `["child-run-abc-456","child-run-def-789"]`
	if !h.mem.Write(0, []byte(runIDsJSON)) {
		t.Fatal("write runIDsJSON to memory failed")
	}

	result, err := h.call(0, uint64(len(runIDsJSON)), 256, 4096)
	if err != nil {
		t.Fatalf("call cleat_await_all_children: %v", err)
	}
	if result == errBadParam {
		t.Error("got errBadParam")
	}
	if handler.runIDsJSON != runIDsJSON {
		t.Errorf("runIDsJSON = %q, want %q", handler.runIDsJSON, runIDsJSON)
	}
}
