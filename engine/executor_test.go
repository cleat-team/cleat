package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Configurable mock WasmBackend for testing executeWithBackend.
// ---------------------------------------------------------------------------

type configurableMockBackend struct {
	executeFn func(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error)
}

func (b *configurableMockBackend) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error) {
	if b.executeFn != nil {
		return b.executeFn(ctx, wasmBytes, entryPoint, input, session)
	}
	return &ExecResult{Result: `{"ok":true}`}, nil
}

func (b *configurableMockBackend) Close(ctx context.Context) error { return nil }
func (b *configurableMockBackend) Name() string                    { return "configurable-mock" }
func (b *configurableMockBackend) PerExecution() WasmBackend       { return b }

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
