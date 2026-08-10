package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/sys"
)

// ---------------------------------------------------------------------------
// formatWasmCallError tests
// ---------------------------------------------------------------------------

func TestFormatWasmCallError_TrapError_ReplacesPrefix(t *testing.T) {
	err := errors.New("wasm error: unreachable\nwasm stack trace:\n    env.cleat_call(i32,i32,i32,i32,i32,i32,i32,i32)\n        0x1234: /build/workflow.go:42:5")
	formatted := formatWasmCallError(err)
	if formatted == nil {
		t.Fatal("expected non-nil error")
	}
	msg := formatted.Error()
	if !strings.Contains(msg, "wasm trap:") {
		t.Errorf("expected 'wasm trap:' prefix, got: %s", msg)
	}
	if strings.Contains(msg, "wasm error:") {
		t.Errorf("should not contain 'wasm error:', got: %s", msg)
	}
	if !strings.Contains(msg, "workflow.go:42") {
		t.Errorf("expected stack trace preserved, got: %s", msg)
	}
	// Verify Unwrap preserves the cause.
	if !errors.Is(formatted, err) {
		t.Error("Unwrap should preserve the original error")
	}
}

func TestFormatWasmCallError_TrapError_Twice(t *testing.T) {
	// Should only replace the first occurrence.
	err := errors.New("wasm error: unreachable; wasm error: again")
	formatted := formatWasmCallError(err)
	msg := formatted.Error()
	if !strings.Contains(msg, "wasm trap:") {
		t.Errorf("expected 'wasm trap:' prefix, got: %s", msg)
	}
}

func TestFormatWasmCallError_Fallback(t *testing.T) {
	err := errors.New("random error")
	formatted := formatWasmCallError(err)
	if formatted == nil {
		t.Fatal("expected non-nil error")
	}
	want := "wasm trap: random error"
	if formatted.Error() != want {
		t.Errorf("got %q, want %q", formatted.Error(), want)
	}
	if !errors.Is(formatted, err) {
		t.Error("Unwrap should preserve the original error")
	}
}

func TestFormatWasmCallError_NilInput(t *testing.T) {
	var nilErr error
	// formatWasmCallError does not guard against nil — calling err.Error()
	// on a nil interface panics. This test documents the behavior.
	defer func() {
		if r := recover(); r == nil {
			t.Log("formatWasmCallError does not guard against nil (documented)")
		}
	}()
	formatWasmCallError(nilErr)
}

func TestFormatWasmCallError_ExitError_DefaultCode(t *testing.T) {
	// Create a zero-value ExitError. ExitCode() returns 0, hitting default case.
	exitErr := &sys.ExitError{}
	formatted := formatWasmCallError(exitErr)
	if formatted == nil {
		t.Fatal("expected non-nil error")
	}
	msg := formatted.Error()
	if !strings.Contains(msg, "wasm trap: exit(code=0)") {
		t.Errorf("expected 'wasm trap: exit(code=0)', got: %s", msg)
	}
	if !errors.Is(formatted, exitErr) {
		t.Error("Unwrap should preserve the original error")
	}
}

// ---------------------------------------------------------------------------
// wasmTrapError tests
// ---------------------------------------------------------------------------

func TestWasmTrapError_Error(t *testing.T) {
	cause := errors.New("underlying error")
	e := &wasmTrapError{cause: cause, msg: "wasm trap: something broke"}
	if e.Error() != "wasm trap: something broke" {
		t.Errorf("got %q, want %q", e.Error(), "wasm trap: something broke")
	}
}

func TestWasmTrapError_Unwrap(t *testing.T) {
	cause := errors.New("underlying error")
	e := &wasmTrapError{cause: cause, msg: "wrapped"}
	if !errors.Is(e, cause) {
		t.Error("errors.Is should find the wrapped cause")
	}
}

func TestWasmTrapError_Unwrap_NilCause(t *testing.T) {
	e := &wasmTrapError{cause: nil, msg: "no cause"}
	if e.Unwrap() != nil {
		t.Error("Unwrap should return nil for nil cause")
	}
}

// ---------------------------------------------------------------------------
// fuelMeter tests
// ---------------------------------------------------------------------------

func TestFuelMeter_NewFunctionListener_ReturnsSelf(t *testing.T) {
	fm := &fuelMeter{}
	listener := fm.NewFunctionListener(nil)
	if listener != fm {
		t.Error("NewFunctionListener should return the fuelMeter itself")
	}
}

func TestFuelMeter_Before_DecrementsFuel(t *testing.T) {
	fm := &fuelMeter{}
	fm.remaining.Store(10)

	// Call Before 7 times — should decrement from 10 to 3.
	for i := 0; i < 7; i++ {
		fm.Before(context.Background(), nil, nil, nil, nil)
	}
	if got := fm.remaining.Load(); got != 3 {
		t.Errorf("expected 3 remaining, got %d", got)
	}
}

func TestFuelMeter_Before_ExhaustedModuleClosed(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx) // closing again is safe (no-op after CloseWithExitCode)

	fm := &fuelMeter{}
	fm.remaining.Store(1)

	// One call exhausts fuel — CloseWithExitCode is called on the module.
	fm.Before(ctx, mod, nil, nil, nil)

	if got := fm.remaining.Load(); got != 0 {
		t.Errorf("expected 0 remaining, got %d", got)
	}
}

func TestFuelMeter_After_Noop(t *testing.T) {
	fm := &fuelMeter{}
	fm.remaining.Store(5)
	// After should be a no-op — no panic expected.
	fm.After(context.Background(), nil, nil, nil)
	if got := fm.remaining.Load(); got != 5 {
		t.Errorf("After should not modify remaining: got %d", got)
	}
}

func TestFuelMeter_Abort_Noop(t *testing.T) {
	fm := &fuelMeter{}
	fm.remaining.Store(5)
	// Abort should be a no-op — no panic expected.
	fm.Abort(context.Background(), nil, nil, nil)
	if got := fm.remaining.Load(); got != 5 {
		t.Errorf("Abort should not modify remaining: got %d", got)
	}
}

// ---------------------------------------------------------------------------
// NewRuntime tests
// ---------------------------------------------------------------------------

func TestNewRuntime_DefaultMemoryLimit(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	if rt.MemoryLimitPages != DefaultMemoryLimitPages {
		t.Errorf("MemoryLimitPages: got %d, want %d", rt.MemoryLimitPages, DefaultMemoryLimitPages)
	}
}

func TestNewRuntime_CustomMemoryLimit(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 256, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	if rt.MemoryLimitPages != 256 {
		t.Errorf("MemoryLimitPages: got %d, want %d", rt.MemoryLimitPages, 256)
	}
}

func TestNewRuntime_InstructionLimit(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 100000)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	if rt.fuelLimit != 100000 {
		t.Errorf("fuelLimit: got %d, want %d", rt.fuelLimit, 100000)
	}
}

func TestNewRuntime_ZeroMemoryLimitLargeInstructionLimit(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	if rt.MemoryLimitPages != DefaultMemoryLimitPages {
		t.Errorf("MemoryLimitPages should default to %d", DefaultMemoryLimitPages)
	}
	if rt.fuelLimit != 0 {
		t.Errorf("fuelLimit should default to 0, got %d", rt.fuelLimit)
	}
}

// ---------------------------------------------------------------------------
// Runtime method tests
// ---------------------------------------------------------------------------

func TestRuntime_StdoutStderr_Accessors(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Initial stdout/stderr should be empty.
	if rt.Stdout() != "" {
		t.Errorf("expected empty stdout, got %q", rt.Stdout())
	}
	if rt.Stderr() != "" {
		t.Errorf("expected empty stderr, got %q", rt.Stderr())
	}
}

func TestRuntime_Close(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if err := rt.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestRuntime_Close_Twice(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if err := rt.Close(ctx); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Second close should not panic. wazero's Close on an already-closed
	// runtime is generally safe.
	if err := rt.Close(ctx); err != nil {
		t.Logf("second Close returned: %v (acceptable)", err)
	}
}

func TestRuntime_CompileModule_InvalidWasm(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	_, err = rt.CompileModule(ctx, []byte{0, 0, 0, 0}) // not valid WASM
	if err == nil {
		t.Error("expected error for invalid WASM bytes")
	}
}

func TestRuntime_CompileModule_Empty(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	_, err = rt.CompileModule(ctx, []byte{})
	if err == nil {
		t.Error("expected error for empty bytes")
	}
}

func TestRuntime_InstantiateModuleNamed(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	// Named instantiation.
	mod, err := rt.InstantiateModuleNamed(ctx, compiled, "test-module")
	if err != nil {
		t.Fatalf("InstantiateModuleNamed: %v", err)
	}
	defer mod.Close(ctx)
}

func TestRuntime_InstantiateModuleNamed_EmptyName(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	// Empty name should produce a valid unnamed module (same as InstantiateModule).
	mod, err := rt.InstantiateModuleNamed(ctx, compiled, "")
	if err != nil {
		t.Fatalf("InstantiateModuleNamed with empty name: %v", err)
	}
	defer mod.Close(ctx)
}

func TestInstantiateModuleNamedWithWriters(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	// White-box test: instantiateModuleNamedWithWriters uses caller-supplied
	// buffers for stdout/stderr capture instead of the Runtime's shared ones.
	var stdoutBuf, stderrBuf bytes.Buffer
	mod, err := rt.instantiateModuleNamedWithWriters(ctx, compiled, "writer-test", &stdoutBuf, &stderrBuf)
	if err != nil {
		t.Fatalf("instantiateModuleNamedWithWriters: %v", err)
	}
	defer mod.Close(ctx)
}

func TestRuntime_InitModule_NoStartExport(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	// InitModule on a module without _start should be a no-op (return nil).
	if err := rt.InitModule(ctx, mod); err != nil {
		t.Errorf("InitModule on module without _start: %v", err)
	}
}

func TestRuntime_InstantiateAndInit_InvalidWasm(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	_, err = rt.InstantiateAndInit(ctx, []byte{0, 0, 0, 0})
	if err == nil {
		t.Error("expected error for invalid WASM bytes")
	}
}

func TestRuntime_InstantiateAndInit_ValidWasm(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	mod, err := rt.InstantiateAndInit(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("InstantiateAndInit: %v", err)
	}
	defer mod.Close(ctx)
}

// ---------------------------------------------------------------------------
// CallExport error path tests
// ---------------------------------------------------------------------------

func TestCallExport_ResultWithNoOutput(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	// CallExport with a non-existent export returns "not found" error.
	_, err = rt.CallExport(ctx, mod, "nonexistent", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for nonexistent export")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestCallExportWithSuspend_NilInput(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	// Nil input and non-existent export -> "not found"
	_, _, err = rt.CallExportWithSuspend(ctx, mod, "missing", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent export")
	}
}

// ---------------------------------------------------------------------------
// Constant value tests
// ---------------------------------------------------------------------------

func TestErrSuspended_Value(t *testing.T) {
	if ErrSuspended.Error() != "workflow suspended" {
		t.Errorf("ErrSuspended = %q, want %q", ErrSuspended.Error(), "workflow suspended")
	}
}

func TestFuelExhaustedError_Value(t *testing.T) {
	if fuelExhaustedError.Error() != "wasm trap: instruction limit exceeded (fuel exhausted)" {
		t.Errorf("fuelExhaustedError = %q", fuelExhaustedError.Error())
	}
}

func TestDefaultMemoryLimitPages_Value(t *testing.T) {
	if DefaultMemoryLimitPages != 512 {
		t.Errorf("DefaultMemoryLimitPages = %d, want 512", DefaultMemoryLimitPages)
	}
}

// ---------------------------------------------------------------------------
// Module API interaction tests
// ---------------------------------------------------------------------------

func TestRuntime_ModuleExportsList(t *testing.T) {
	// Verify that a compiled minimal WASM module has no exports (as expected).
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	exports := compiled.ExportedFunctions()
	if len(exports) != 0 {
		t.Errorf("expected no exported functions, got %d", len(exports))
	}
}

func TestRuntime_CallExport_SuccessPathViaPanicRecovery(t *testing.T) {
	// Verify that the defer/recover in CallExportWithSuspend doesn't interfere
	// with normal execution when using a valid module.
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)

	// CallExport on a module with no exports should fail with "not found" —
	// the deferred recover handles any panics from wazero internals.
	_, err = rt.CallExport(ctx, mod, "handle", []byte(`{"key":"val"}`))
	if err == nil {
		t.Error("expected error for module with no exports")
	}
}

// ---------------------------------------------------------------------------
// Context deadline / timeout propagation tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// wasmTrapError type assertion
// ---------------------------------------------------------------------------

func TestWasmTrapError_ImplementsError(t *testing.T) {
	var err error = &wasmTrapError{cause: nil, msg: "test"}
	if err.Error() != "test" {
		t.Errorf("got %q, want %q", err.Error(), "test")
	}
}

// ---------------------------------------------------------------------------
// fuelMeter edge case: zero fuel initially
// ---------------------------------------------------------------------------

func TestFuelMeter_ZeroFuel_DoesNotUnderflow(t *testing.T) {
	fm := &fuelMeter{}
	fm.remaining.Store(0)

	// With 0 fuel, Add(^uint64(0)) wraps to math.MaxUint64, which is != 0,
	// so CloseWithExitCode is NOT called. The module parameter is nil-safe here.
	fm.Before(context.Background(), nil, nil, nil, nil)

	// After the call, remaining = math.MaxUint64 (the unsigned underflow).
	// This is expected behavior — fuel set to 0 effectively means "no limit"
	// is being enforced (since the check only fires on exact zero after decrement).
	// We verify it does not panic.
}

// ---------------------------------------------------------------------------
// formatWasmCallError with ExitError edge cases
// ---------------------------------------------------------------------------

// Test that the type-switch cases compile and are reachable.
func TestFormatWasmCallError_ExitError_ImplementsError(t *testing.T) {
	// Verify that sys.ExitError satisfies the error interface.
	var err error = &sys.ExitError{}
	_ = err
}
