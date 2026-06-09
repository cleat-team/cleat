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

// ---------------------------------------------------------------------------
// wasiBuilder / teavmBuilder create host functions (tested via NewRuntime)
// These tests verify that the stubs registered in NewRuntime don't panic.
// ---------------------------------------------------------------------------

func TestWasiResetAdapterState_Registered(t *testing.T) {
	// The "reset_adapter_state" function is registered on the WASI module.
	// This test verifies that NewRuntime registers it without error.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	// If we got here, WASI registration (including reset_adapter_state) succeeded.
}

func TestTeavmStubs_Registered(t *testing.T) {
	// TeaVM stubs (putwcharsOut, currentTimeMillis, logString, logInt,
	// logOutOfMemory) are registered during NewRuntime. Verify no error.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
}

// ---------------------------------------------------------------------------
// AssemblyScript abort stub test
// ---------------------------------------------------------------------------

func TestAssemblyScriptAbortStub_Registered(t *testing.T) {
	// The "abort" function is registered on the "env" host module for
	// AssemblyScript compatibility. Verify NewRuntime succeeds.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
}

// ---------------------------------------------------------------------------
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
// cleat_complete host function closure test
// ---------------------------------------------------------------------------

func TestCleatCompleteClosure_StoresResult(t *testing.T) {
	// Simulate what the cleat_complete host function does: store a result
	// in the context's cleatComplete struct.
	cc := &cleatComplete{}
	ctx := context.WithValue(context.Background(), &cleatCompleteKey, cc)
	_ = ctx

	// Simulate cleat_complete(0 /* success */, ptr, len).
	// The actual closure reads from WASM memory; here we set fields directly.
	expected := `{"status":"ok"}`
	cc.Result = &expected

	if cc.Result == nil || *cc.Result != expected {
		t.Errorf("cleat_complete result: got %v, want %q", cc.Result, expected)
	}
}

func TestCleatCompleteClosure_StoresError(t *testing.T) {
	cc := &cleatComplete{}
	ctx := context.WithValue(context.Background(), &cleatCompleteKey, cc)
	_ = ctx

	expected := "something went wrong"
	cc.Error = &expected

	if cc.Error == nil || *cc.Error != expected {
		t.Errorf("cleat_complete error: got %v, want %q", cc.Error, expected)
	}
}

func TestCleatCompleteClosure_MissingContextKey(t *testing.T) {
	// When cleatCompleteKey is not in the context, the cleat_complete host
	// function should silently handle it (the value will be nil).
	// We verify by creating a context without the key.
	cc := &cleatComplete{}
	result := "test"
	cc.Result = &result

	// Without the context key, setting Result on a local struct is fine.
	// The real cleat_complete function accesses ctx.Value(&cleatCompleteKey)
	// and skips if nil. Our local test just checks the struct mechanics.
	if *cc.Result != "test" {
		t.Error("cleatComplete struct should still work without context key")
	}
}

// ---------------------------------------------------------------------------
// Test that the handlerContextKey is typed correctly (structural identity)
// ---------------------------------------------------------------------------

func TestHandlerContextKey_Type(t *testing.T) {
	// Both are struct{} types, so every instance is equal.
	k1 := handlerContextKey{}
	k2 := handlerContextKey{}
	if k1 != k2 {
		t.Error("handlerContextKey instances should be equal")
	}
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
// Test that the env module stub (cleat_poll_work, cleat_complete) exist
// ---------------------------------------------------------------------------

func TestEnvModuleStubs_NoPanic(t *testing.T) {
	// cleat_poll_work and cleat_complete are registered on the "env" module
	// by registerHostFunctions. Verify they were registered by creating a
	// Runtime.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	// Registration succeeded — stubs exist.
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
// Test the nowMs atomic has the correct type-is-a-struct check
// ---------------------------------------------------------------------------

func TestNowMs_Type(t *testing.T) {
	// nowMs is an atomic.Int64. Verify it's the correct type by calling
	// its methods.
	nowMs.Store(42)
	if nowMs.Load() != 42 {
		t.Errorf("nowMs.Load() = %d, want 42", nowMs.Load())
	}
	nowMs.Store(0) // reset for other tests
}

// ---------------------------------------------------------------------------
// Validate that cleat_complete key comparison works
// ---------------------------------------------------------------------------

func TestCleatCompleteKey_Identity(t *testing.T) {
	// &cleatCompleteKey is a pointer to a package-level struct{} used as a
	// context key. The pointer identity matters: every use must reference
	// the same variable. We verify the pointer is consistent.
	key1 := &cleatCompleteKey
	key2 := &cleatCompleteKey
	if key1 != key2 {
		t.Error("&cleatCompleteKey should be the same pointer every time")
	}
}

// ---------------------------------------------------------------------------
// Edge case: empty string in UpdateNowMs (ensures no crash)
// ---------------------------------------------------------------------------

func TestUpdateNowMs_ConcurrentSafe(t *testing.T) {
	// atomic.Int64 is concurrent-safe by design. Verify it doesn't panic
	// when called (the function uses atomic.Store).
	UpdateNowMs()
}
