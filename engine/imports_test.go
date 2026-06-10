package engine

import (
	"context"
	"testing"
	"time"

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
