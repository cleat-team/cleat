package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Configurable mock WasmBackend for testing executeWithBackend.
// ---------------------------------------------------------------------------

type configurableMockBackend struct {
	executeFn func(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error)

	// gotInstanceTimeout is the per-tenant instance timeout the engine passed
	// to PerExecution on the most recent execution. 0 means "the tenant set
	// none", which is also what an engine with no settings store resolves.
	gotInstanceTimeout time.Duration
}

func (b *configurableMockBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	if b.executeFn != nil {
		return b.executeFn(ctx, wasmBytes, entryPoint, input, session)
	}
	return &ExecResult{Result: `{"ok":true}`}, nil
}

func (b *configurableMockBackend) Close(ctx context.Context) error { return nil }
func (b *configurableMockBackend) Name() string                    { return "configurable-mock" }

// PerExecution records the per-tenant instance timeout it was handed, so a
// test can assert what the engine RESOLVED without needing a real wasmtime
// store. It returns the same backend rather than a copy, deliberately: the
// recorded value must survive for the assertion.
func (b *configurableMockBackend) PerExecution(d time.Duration) WasmBackend {
	b.gotInstanceTimeout = d
	return b
}

// ---------------------------------------------------------------------------
// backendForWasm tests.
// ---------------------------------------------------------------------------

func TestBackendForWasm_NilBackends(t *testing.T) {
	e := NewEngine(nil, nil)
	// Engine created without WithBackend — backends map is non-nil but empty.
	// Set backends to nil explicitly.
	e.backends = nil
	result := e.backendForWasm(minimalWasm())
	if result != nil {
		t.Errorf("expected nil for nil backends, got %v", result)
	}
}

func TestBackendForWasm_EmptyBackends(t *testing.T) {
	e := NewEngine(nil, nil)
	// backends is an empty map (no backends registered via WithBackend).
	// minimalWasm() detects as "go", which is not in the map.
	result := e.backendForWasm(minimalWasm())
	if result != nil {
		t.Errorf("expected nil for empty backends, got %v", result)
	}
}

func TestBackendForWasm_KnownLanguage(t *testing.T) {
	backend := &configurableMockBackend{}
	e := NewEngine(nil, nil, WithBackend("go", backend))
	// minimalWasm() detects as "go".
	result := e.backendForWasm(minimalWasm())
	if result == nil {
		t.Fatal("expected non-nil backend for known language")
	}
	if result.Name() != "configurable-mock" {
		t.Errorf("expected 'configurable-mock' backend, got %q", result.Name())
	}
}

func TestBackendForWasm_UnknownLanguage(t *testing.T) {
	backend := &configurableMockBackend{}
	e := NewEngine(nil, nil, WithBackend("rust", backend))
	// minimalWasm() detects as "go", but only "rust" is registered.
	result := e.backendForWasm(minimalWasm())
	if result != nil {
		t.Errorf("expected nil for unknown language, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// executeWithBackend tests.
// ---------------------------------------------------------------------------

func TestExecuteWithBackend_FreshExecution(t *testing.T) {
	backend := &configurableMockBackend{
		executeFn: func(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
			es := session.(*execSession)
			// Simulate host function calls recording events.
			es.history = append(es.history, EventRecord{
				Step: 0, EventType: EventTypeCall,
				Service: "my-svc", Op: "my-op",
				Request: `{"key":"val"}`, Response: `{"result":"ok"}`,
			})
			return &ExecResult{Result: `{"done":true}`}, nil
		},
	}
	e := NewEngine(nil, nil, WithBackend("go", backend))
	e.workflowID = "wf-exec-test"
	e.defName = "test-def"

	result, history, suspended, deferrals, queryState, err := e.executeWithBackend(
		context.Background(), backend, minimalWasm(), "test_entry", []byte(`{"input":"data"}`), nil,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"done":true}` {
		t.Errorf("expected result %q, got %q", `{"done":true}`, result)
	}
	if len(history) == 0 {
		t.Error("expected non-empty history")
	}
	if suspended != nil {
		t.Error("expected nil suspend result for successful execution")
	}
	if deferrals == nil {
		t.Error("expected non-nil deferrals map")
	}
	// queryState may be nil if no SetQueryState calls were made during execution.
	_ = queryState
}

func TestExecuteWithBackend_ReplayExecution(t *testing.T) {
	backend := &configurableMockBackend{
		executeFn: func(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
			es := session.(*execSession)
			if !es.isReplay {
				t.Error("expected session to be in replay mode")
			}
			if len(es.history) == 0 {
				t.Error("expected non-empty replay history")
			}
			return &ExecResult{Result: `{"replayed":true}`}, nil
		},
	}
	e := NewEngine(nil, nil, WithBackend("go", backend))
	e.workflowID = "wf-replay-test"
	e.defName = "test-def"

	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{}`, TimestampMs: 1000},
	}

	result, _, _, _, _, err := e.executeWithBackend(
		context.Background(), backend, minimalWasm(), "test_entry", []byte(`{}`), history,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"replayed":true}` {
		t.Errorf("expected result %q, got %q", `{"replayed":true}`, result)
	}
}

func TestExecuteWithBackend_ErrorPropagation(t *testing.T) {
	backend := &configurableMockBackend{
		executeFn: func(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
			return nil, context.DeadlineExceeded
		},
	}
	e := NewEngine(nil, nil, WithBackend("go", backend))
	e.workflowID = "wf-error-test"

	_, _, _, _, _, err := e.executeWithBackend(
		context.Background(), backend, minimalWasm(), "test", []byte(`{}`), nil,
	)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "execution failed") {
		t.Errorf("expected 'execution failed' in error, got %q", err.Error())
	}
}

func TestExecuteWithBackend_Timeout(t *testing.T) {
	backend := &configurableMockBackend{
		executeFn: func(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
			// Block until context is cancelled.
			<-ctx.Done()
			// Return nil error — the caller will check ctx.Err().
			return &ExecResult{}, nil
		},
	}
	e := NewEngine(nil, nil, WithBackend("go", backend))
	e.workflowID = "wf-timeout-test"
	e.defaultWorkflowTimeout = 50 * time.Millisecond

	_, _, _, _, _, err := e.executeWithBackend(
		context.Background(), backend, minimalWasm(), "test", []byte(`{}`), nil,
	)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected 'timed out' in error, got %q", err.Error())
	}
}

func TestExecuteWithBackend_Suspend(t *testing.T) {
	backend := &configurableMockBackend{
		executeFn: func(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
			es := session.(*execSession)
			// Set suspend error to simulate workflow suspension.
			es.suspendErr = &SuspendError{
				Reason: "test_suspend",
				Until:  time.Now().Add(time.Minute),
			}
			return &ExecResult{Suspended: true}, nil
		},
	}
	e := NewEngine(nil, nil, WithBackend("go", backend))
	e.workflowID = "wf-suspend-test"

	_, _, suspended, deferrals, _, err := e.executeWithBackend(
		context.Background(), backend, minimalWasm(), "test", []byte(`{}`), nil,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if suspended == nil {
		t.Fatal("expected non-nil suspend result")
	}
	if suspended.Reason != "test_suspend" {
		t.Errorf("expected reason 'test_suspend', got %q", suspended.Reason)
	}
	if deferrals == nil {
		t.Error("expected non-nil deferrals")
	}
}

// ---------------------------------------------------------------------------
// DispatchUpdate tests
// ---------------------------------------------------------------------------

func TestDispatchUpdate_NoHandler(t *testing.T) {
	e := NewEngine(nil, nil)
	_, err := e.DispatchUpdate(context.Background(), "my-update", `{}`)
	if err == nil {
		t.Fatal("expected error without update handler")
	}
}

func TestDispatchUpdate_WithHandler(t *testing.T) {
	e := NewEngine(nil, nil, WithUpdateHandler(func(name, payload string) (string, error) {
		if name != "my-update" {
			t.Errorf("expected name 'my-update', got %q", name)
		}
		if payload != `{"key":"val"}` {
			t.Errorf("expected payload %q, got %q", `{"key":"val"}`, payload)
		}
		return `{"result":"ok"}`, nil
	}))
	result, err := e.DispatchUpdate(context.Background(), "my-update", `{"key":"val"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"result":"ok"}` {
		t.Errorf("expected %q, got %q", `{"result":"ok"}`, result)
	}
}

// ---------------------------------------------------------------------------
// executeCompiled and Execute/ExecuteCompiled/ReplayCompiled tests
// ---------------------------------------------------------------------------

// TestExecute_CompilationFailure verifies that Execute returns a compilation
// error when given invalid WASM bytes.
func TestExecute_CompilationFailure(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, nil)
	engine.workflowID = "wf-compile-fail"

	// Invalid WASM bytes should fail at compilation.
	_, _, _, _, _, err = engine.Execute(ctx, []byte{0, 0, 0, 0}, "test", nil)
	if err == nil {
		t.Fatal("expected compilation error")
	}
	if !strings.Contains(err.Error(), "compile module") {
		t.Errorf("expected 'compile module' in error, got: %v", err)
	}
}

// TestExecuteCompiled_InstantiationFailure verifies that ExecuteCompiled
// returns an instantiation error when the compiled module imports from an
// unknown module (compiles but cannot be instantiated).
func TestExecuteCompiled_InstantiationFailure(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Craft a minimal WASM module with an import from an unknown module "x".
	// wazero's CompileModule validates binary structure (succeeds), but
	// InstantiateModule fails because "x" is not in the Runtime's store.
	wasmBytes := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version 1
		// Type section: 1 type, (func)
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		// Import section: 1 import, module="x" name="test", func type 0
		0x02, 0x0a, 0x01,
		0x01, 0x78,
		0x04, 0x74, 0x65, 0x73, 0x74,
		0x00, 0x00,
	}

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("CompileModule should succeed for structurally valid module: %v", err)
	}
	defer compiled.Close(ctx)

	engine := NewEngine(rt, nil)
	engine.workflowID = "wf-inst-fail"

	_, _, _, _, _, err = engine.ExecuteCompiled(ctx, compiled, "test", nil)
	if err == nil {
		t.Fatal("expected instantiation error for module with unresolved imports")
	}
	if !strings.Contains(err.Error(), "instantiate module") {
		t.Errorf("expected 'instantiate module' in error, got: %v", err)
	}
}

// TestReplayCompiled_ChecksumVerificationFailure verifies that ReplayCompiled
// returns an error when the workflow event verifier reports a checksum mismatch
// and failOnMismatch is true.
func TestReplayCompiled_ChecksumVerificationFailure(t *testing.T) {
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

	engine := NewEngine(rt, nil,
		WithWorkflowEventVerifier(func(ctx context.Context, workflowID string) error {
			return errors.New("checksum mismatch")
		}, true), // failOnMismatch = true
	)
	engine.workflowID = "wf-checksum-fail"

	// Non-empty history triggers replay verification in executeCompiled.
	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o", Request: "{}", Response: "{}", TimestampMs: 1000},
	}

	_, _, _, _, _, err = engine.ReplayCompiled(ctx, compiled, "test", nil, history)
	if err == nil {
		t.Fatal("expected checksum verification error")
	}
	if !strings.Contains(err.Error(), "checksum verification failed") {
		t.Errorf("expected 'checksum verification failed' in error, got: %v", err)
	}
}

// TestReplayCompiled_VersionValidationFailure verifies that ReplayCompiled
// returns an error when version validation fails and allowVersionMismatch is false.
func TestReplayCompiled_VersionValidationFailure(t *testing.T) {
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

	engine := NewEngine(rt, nil,
		WithVersionValidation(func() error {
			return errors.New("version mismatch")
		}),
		WithAllowVersionMismatch(false),
	)
	engine.workflowID = "wf-version-fail"

	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o", Request: "{}", Response: "{}", TimestampMs: 1000},
	}

	_, _, _, _, _, err = engine.ReplayCompiled(ctx, compiled, "test", nil, history)
	if err == nil {
		t.Fatal("expected version validation error")
	}
	if !strings.Contains(err.Error(), "version validation failed") {
		t.Errorf("expected 'version validation failed' in error, got: %v", err)
	}
}
