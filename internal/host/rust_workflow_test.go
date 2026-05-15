package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildRustWasm compiles the Rust workflow crate to WASM and returns the path.
func buildRustWasm(t *testing.T) string {
	t.Helper()

	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("cargo not installed — skipping Rust WASM integration test")
	}

	// Check if wasm32-wasip1 target is available.
	if out, err := exec.Command("rustup", "target", "list", "--installed").Output(); err != nil || !strings.Contains(string(out), "wasm32-wasip1") {
		t.Skip("wasm32-wasip1 Rust target not installed — skipping Rust WASM integration test")
	}

	projectRoot := findProjectRoot(t)
	rustDir := filepath.Join(projectRoot, "examples", "rust-workflow")

	cmd := exec.Command(cargo, "build", "--target", "wasm32-wasip1", "--release")
	cmd.Dir = rustDir
	cmd.Env = append(os.Environ(),
		"HOME="+os.Getenv("HOME"),
		"PATH="+os.Getenv("PATH"),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cargo build failed:\n%s\n%v", string(out), err)
	}

	return filepath.Join(rustDir, "target", "wasm32-wasip1", "release", "rust_workflow.wasm")
}

func findProjectRoot(t *testing.T) string {
	t.Helper()
	cwd, _ := os.Getwd()
	if strings.HasSuffix(cwd, "internal/host") {
		return filepath.Dir(filepath.Dir(cwd))
	}
	return cwd
}

// TestRustWorkflowExecute runs the Rust place_order workflow through the
// Go host engine, proving that non-Go WASM modules work with the cleat runtime.
func TestRustWorkflowExecute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM integration test in short mode")
	}

	wasmPath := buildRustWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	// Rust workflow expects snake_case JSON fields matching Rust structs.
	input := []byte(`{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`)
	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute Rust workflow: %v", err)
	}
	_ = suspended
	if result == "" {
		t.Error("expected non-empty result from Rust workflow")
	}
	if len(history) == 0 {
		t.Error("expected non-empty history from Rust workflow")
	}

	// Filter durable_log events; the Rust workflow calls:
	// inventory.Reserve -> payments.Charge -> shipping.CreateShipment -> notifications.SendEmail
	var callHistory []EventRecord
	for _, rec := range history {
		if rec.EventType != EventTypeDurableLog {
			callHistory = append(callHistory, rec)
		}
	}
	expectedCalls := []string{"inventory", "payments", "shipping", "notifications"}
	for i, svc := range expectedCalls {
		if i >= len(callHistory) {
			t.Errorf("step %d: missing (expected %s)", i, svc)
			continue
		}
		if callHistory[i].Service != svc {
			t.Errorf("step %d: expected service %s, got %s", i, svc, callHistory[i].Service)
		}
	}

	t.Logf("Rust workflow result: %s, history: %d calls", result, len(history))
	for i, rec := range history {
		t.Logf("  step %d: %s.%s => %s (err=%s)", i, rec.Service, rec.Op, rec.Response, rec.Err)
	}
}

// TestRustWorkflowReplay verifies that the Rust workflow can be replayed
// from a recorded event history.
func TestRustWorkflowReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM integration test in short mode")
	}

	wasmPath := buildRustWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// First execution to get history
	caller1 := &mockCaller{}
	engine1 := NewEngine(rt, caller1)
	input := []byte(`{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`)
	result1, history, _, _, _, err := engine1.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute (first): %v", err)
	}

	// Replay with recorded history
	caller2 := &mockCaller{}
	engine2 := NewEngine(rt, caller2)
	result2, _, _, _, _, err := engine2.Replay(ctx, wasmBytes, "place_order", input, history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result1 != result2 {
		t.Errorf("replay result mismatch: %q vs %q", result1, result2)
	}
	if len(caller2.calls) > 0 {
		t.Errorf("replay made %d real calls (expected 0)", len(caller2.calls))
	}
	t.Logf("Rust workflow replay OK: %s", result2)
}

// TestRustWorkflowCancelOrder tests the cancel_order export from Rust.
func TestRustWorkflowCancelOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM integration test in short mode")
	}

	wasmPath := buildRustWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)
	input := []byte(`{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":1}]}`)
	result, history, _, _, _, err := engine.Execute(ctx, wasmBytes, "cancel_order", input)
	if err != nil {
		t.Fatalf("Execute cancel_order: %v", err)
	}
	t.Logf("cancel_order result: %s, history: %d calls", result, len(history))
	// cancel_order polls cancellation first, then runs the normal workflow
	if len(history) > 0 {
		t.Logf("  first call: %s.%s", history[0].Service, history[0].Op)
	}
}

// TestRustWorkflowCompensation verifies compensation when a step fails.
func TestRustWorkflowCompensation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM integration test in short mode")
	}

	wasmPath := buildRustWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Use a caller that fails on shipping to trigger compensation
	caller := &failingCaller{failService: "shipping", failOperation: "CreateShipment"}
	engine := NewEngine(rt, caller)
	input := []byte(`{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`)
	_, history, _, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)
	// Expect error from workflow
	if err == nil {
		t.Error("expected error from compensation path, got nil")
	}
	t.Logf("Compensation result: err=%v, history=%d calls", err, len(history))
	for i, rec := range history {
		t.Logf("  step %d: %s.%s (err=%s)", i, rec.Service, rec.Op, rec.Err)
	}

	// Should see inventory.Reserve, payments.Charge, shipping.CreateShipment(fail),
	// then compensation: payments.Refund, inventory.Release
	foundRefund := false
	foundRelease := false
	for _, rec := range history {
		if rec.Service == "payments" && rec.Op == "Refund" {
			foundRefund = true
		}
		if rec.Service == "inventory" && rec.Op == "Release" {
			foundRelease = true
		}
	}
	if !foundRefund {
		t.Error("expected payments.Refund compensation call")
	}
	if !foundRelease {
		t.Error("expected inventory.Release compensation call")
	}
}

// failingCaller returns errors for a specific service+operation.
type failingCaller struct {
	failService   string
	failOperation string
}

func (f *failingCaller) Call(_ context.Context, service, operation, requestJSON string) (string, error) {
	if service == f.failService && operation == f.failOperation {
		return "", &mockCallError{msg: "service unavailable"}
	}
	return mockResponse(service, operation), nil
}

type mockCallError struct{ msg string }

func (e *mockCallError) Error() string   { return e.msg }
func (e *mockCallError) Retryable() bool { return false }
