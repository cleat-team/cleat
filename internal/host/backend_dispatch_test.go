package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/wasm"
)

// ---------------------------------------------------------------------------
// mockBackend is a WasmBackend that records calls for test assertions.
// ---------------------------------------------------------------------------

type mockBackend struct {
	name          string
	executeCalled int
	closeCalled   int
}

func (m *mockBackend) Name() string { return m.name }

func (m *mockBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	m.executeCalled++
	return &ExecResult{Result: `{"mock":true}`, Suspended: false}, nil
}

func (m *mockBackend) Close(ctx context.Context) error {
	m.closeCalled++
	return nil
}

// ---------------------------------------------------------------------------
// wasmWithLanguage constructs a minimal valid WASM binary that has a
// "cleat.metadata" custom section carrying the given language string.
// ---------------------------------------------------------------------------

func wasmWithLanguage(lang string) []byte {
	meta := wasm.Metadata{
		WorkflowName:         "test-workflow",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
		Language:             lang,
	}
	// Start with a bare-bones WASM module (magic + version).
	bare := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic "\0asm"
		0x01, 0x00, 0x00, 0x00, // version 1
	}
	result, err := wasm.WriteMetadata(bare, &meta)
	if err != nil {
		panic("wasmWithLanguage: " + err.Error())
	}
	return result
}

// wasmWithoutLanguage returns a minimal WASM binary with no cleat.metadata
// section, so DetectLanguage falls back to its default ("go").
func wasmWithoutLanguage() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic "\0asm"
		0x01, 0x00, 0x00, 0x00, // version 1
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestNewEngineWithBackend verifies that NewEngine accepts WithBackend and
// that the registered backend is used for execution.
func TestNewEngineWithBackend(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend := &mockBackend{name: "test-backend"}
	caller := &mockCaller{}
	engine := NewEngine(rt, caller, WithBackend("go", backend))

	// DetectLanguage defaults to "go" for bare WASM, so the backend should
	// match and be invoked.
	wasmBytes := wasmWithoutLanguage()
	input := json.RawMessage(`{}`)
	_, _, _, _, _, err = engine.Execute(ctx, wasmBytes, "entry", input)
	if err != nil {
		// An error here is expected only if the mock backend errors.
		// The mock returns success, so this should be nil.
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.executeCalled != 1 {
		t.Errorf("expected backend.Execute to be called 1 time, got %d", backend.executeCalled)
	}
}

// TestNewEngineWithBackendPythonLanguage verifies that a WASM binary with
// language "python" in its cleat.metadata selects the correct backend.
func TestNewEngineWithBackendPythonLanguage(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	goBackend := &mockBackend{name: "go-backend"}
	pyBackend := &mockBackend{name: "py-backend"}
	caller := &mockCaller{}
	engine := NewEngine(rt, caller,
		WithBackend("go", goBackend),
		WithBackend("python", pyBackend),
	)

	// A WASM binary with language "python" should use the python backend.
	wasmBytes := wasmWithLanguage("python")
	input := json.RawMessage(`{}`)
	_, _, _, _, _, err = engine.Execute(ctx, wasmBytes, "entry", input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pyBackend.executeCalled != 1 {
		t.Errorf("expected python backend.Execute to be called 1 time, got %d", pyBackend.executeCalled)
	}
	if goBackend.executeCalled != 0 {
		t.Errorf("expected go backend.Execute to not be called, got %d", goBackend.executeCalled)
	}
}

// TestNewEngineWithBackendDefaultFallback verifies that when no backend
// matches the detected language, the default backend ("go") is used as
// fallback.
func TestNewEngineWithBackendDefaultFallback(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	pyBackend := &mockBackend{name: "py-backend"}
	caller := &mockCaller{}
	engine := NewEngine(rt, caller, WithBackend("python", pyBackend))

	// A bare WASM binary (language defaults to "go") should NOT match
	// "python", but should fall back to the default backend... Wait,
	// NewEngine sets defaultBackend to "go", and we only registered
	// "python". So the fallback won't find "go" either. The engine
	// should then use the legacy wazero path.
	wasmBytes := wasmWithoutLanguage()
	input := json.RawMessage(`{}`)
	_, _, _, _, _, err = engine.Execute(ctx, wasmBytes, "entry", input)
	if err == nil {
		// A bare WASM module with no exports will fail during
		// CallExport, so an error is expected from the legacy path.
		t.Log("engine.Execute fell through to legacy path (expected)")
	} else {
		t.Logf("engine.Execute error (expected from legacy path): %v", err)
	}
	// The python backend should NOT have been called.
	if pyBackend.executeCalled != 0 {
		t.Errorf("expected python backend to not be called, got %d", pyBackend.executeCalled)
	}
	if err == nil {
		t.Error("expected error from legacy path execution of bare WASM")
	}
}

// TestEngineBackendForWasm verifies that backendForWasm selects the correct
// backend based on language detection.
func TestEngineBackendForWasm(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	goBackend := &mockBackend{name: "go-backend"}
	pyBackend := &mockBackend{name: "py-backend"}
	caller := &mockCaller{}
	engine := NewEngine(rt, caller,
		WithBackend("go", goBackend),
		WithBackend("python", pyBackend),
	)

	// Test 1: Go WASM -> go backend.
	b1 := engine.backendForWasm(wasmWithoutLanguage())
	if b1 != goBackend {
		t.Errorf("expected go backend for default language, got %v", b1)
	}

	// Test 2: Python WASM -> python backend.
	b2 := engine.backendForWasm(wasmWithLanguage("python"))
	if b2 != pyBackend {
		t.Errorf("expected python backend for python language, got %v", b2)
	}

	// Test 3: Explicit go language -> go backend.
	b3 := engine.backendForWasm(wasmWithLanguage("go"))
	if b3 != goBackend {
		t.Errorf("expected go backend for go language, got %v", b3)
	}
}

// TestEngineBackendForWasmNoMatch verifies that when no backend matches and
// no default fallback exists, backendForWasm returns nil.
func TestEngineBackendForWasmNoMatch(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Register only the python backend; no "go" is registered.
	pyBackend := &mockBackend{name: "py-backend"}
	caller := &mockCaller{}
	engine := NewEngine(rt, caller, WithBackend("python", pyBackend))

	// Bare WASM (detected as "go") won't match "python", and no "go"
	// fallback is registered.
	b := engine.backendForWasm(wasmWithoutLanguage())
	if b != nil {
		t.Errorf("expected nil backend for unmatched language, got %v", b)
	}
}

// TestWasmDetectLanguagePython verifies that wasm.DetectLanguage correctly
// identifies Python WASM binaries.
func TestWasmDetectLanguagePython(t *testing.T) {
	// WASM with cleat.metadata language "python".
	wasmBytes := wasmWithLanguage("python")
	lang := wasm.DetectLanguage(wasmBytes)
	if lang != "python" {
		t.Errorf("expected language 'python', got %q", lang)
	}
}

// TestWasmDetectLanguageGo verifies that wasm.DetectLanguage defaults to "go"
// for bare WASM binaries without cleat.metadata.
func TestWasmDetectLanguageGo(t *testing.T) {
	wasmBytes := wasmWithoutLanguage()
	lang := wasm.DetectLanguage(wasmBytes)
	if lang != "go" {
		t.Errorf("expected language 'go', got %q", lang)
	}
}

// TestWasmDetectLanguageExplicitGo verifies that wasm.DetectLanguage returns
// "go" when cleat.metadata specifies "go".
func TestWasmDetectLanguageExplicitGo(t *testing.T) {
	wasmBytes := wasmWithLanguage("go")
	lang := wasm.DetectLanguage(wasmBytes)
	if lang != "go" {
		t.Errorf("expected language 'go', got %q", lang)
	}
}

// TestEngineDispatchReplay verifies that Replay also dispatches to backends.
func TestEngineDispatchReplay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend := &mockBackend{name: "test-backend"}
	caller := &mockCaller{}
	engine := NewEngine(rt, caller, WithBackend("go", backend))

	wasmBytes := wasmWithoutLanguage()
	input := json.RawMessage(`{}`)
	_, _, _, _, _, err = engine.Replay(ctx, wasmBytes, "entry", input, nil)
	if err != nil {
		t.Logf("Replay error (expected from empty module): %v", err)
	}
	// The backend should have been called for replay too.
	if backend.executeCalled != 1 {
		t.Errorf("expected backend.Execute to be called 1 time during Replay, got %d", backend.executeCalled)
	}
}

// ---------------------------------------------------------------------------
// compile-time check: mockBackend implements WasmBackend
// ---------------------------------------------------------------------------

var _ WasmBackend = (*mockBackend)(nil)

// ---------------------------------------------------------------------------
// Ensure wazeroBackend compiles and implements WasmBackend (also verified by
// compile-time check in backend_wazero.go), creating one verifies that
// NewWazeroBackend works end-to-end.
// ---------------------------------------------------------------------------

func TestNewWazeroBackend(t *testing.T) {
	ctx := context.Background()
	backend, err := NewWazeroBackend(ctx)
	if err != nil {
		t.Fatalf("NewWazeroBackend: %v", err)
	}
	defer func() {
		if cerr := backend.Close(ctx); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	if backend.Name() != "wazero" {
		t.Errorf("expected name 'wazero', got %q", backend.Name())
	}
}

// TestEngineWithWazeroBackend verifies that an Engine can be created with a
// wazeroBackend and execute a minimal WASM module (expecting call failure
// due to missing exports, but not a backend dispatch failure).
func TestEngineWithWazeroBackend(t *testing.T) {
	ctx := context.Background()
	wazeroB, err := NewWazeroBackend(ctx)
	if err != nil {
		t.Fatalf("NewWazeroBackend: %v", err)
	}
	defer wazeroB.Close(ctx)

	caller := &mockCaller{}

	// Need a Runtime for NewEngine (it is required even when using backends).
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, caller, WithBackend("go", wazeroB))

	wasmBytes := wasmWithoutLanguage()
	input := json.RawMessage(`{}`)
	_, _, _, _, _, err = engine.Execute(ctx, wasmBytes, "entry", input)
	// Expect an error because the bare module has no "entry" export.
	if err == nil {
		t.Error("expected error from executing a bare WASM module (no exports)")
	} else {
		t.Logf("Engine.Execute with wazeroBackend correctly failed: %v", err)
	}
}

func TestWasmDetectLanguageInvalidBinary(t *testing.T) {
	// An invalid WASM binary should return "go" (the default).
	lang := wasm.DetectLanguage([]byte{0x00, 0x00, 0x00, 0x00})
	if lang != "go" {
		t.Errorf("expected default 'go' for invalid binary, got %q", lang)
	}
}

// TestEngineNoBackends verifies that an engine with no backends registered
// falls through to the legacy wazero path.
func TestEngineNoBackends(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller) // no WithBackend options

	wasmBytes := wasmWithoutLanguage()
	input := json.RawMessage(`{}`)
	_, _, _, _, _, err = engine.Execute(ctx, wasmBytes, "entry", input)
	if err == nil {
		t.Error("expected error from legacy path (bare WASM with no exports)")
	} else {
		t.Logf("Legacy path correctly failed: %v", err)
	}
}

// TestEngineBackendErrorPropagation verifies that errors from backends are
// propagated correctly.
func TestEngineBackendErrorPropagation(t *testing.T) {
	expectedErr := errors.New("backend execution failed")
	errBackend := &errBackend{err: expectedErr}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller, WithBackend("go", errBackend))

	wasmBytes := wasmWithoutLanguage()
	input := json.RawMessage(`{}`)
	_, _, _, _, _, err = engine.Execute(ctx, wasmBytes, "entry", input)
	if err == nil {
		t.Fatal("expected error from backend")
	}
	if !strings.Contains(err.Error(), "backend execution failed") {
		t.Errorf("expected error to contain 'backend execution failed', got %q", err.Error())
	}
}

// errBackend is a WasmBackend that always returns the configured error.
type errBackend struct {
	err error
}

func (b *errBackend) Name() string { return "err-backend" }

func (b *errBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	return nil, b.err
}

func (b *errBackend) Close(ctx context.Context) error { return nil }
