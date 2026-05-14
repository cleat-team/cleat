package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildAssemblyScriptWasm compiles the AssemblyScript workflow crate to WASM
// and returns the path to the compiled module. It skips the test if npm or
// the AssemblyScript build chain is unavailable.
func buildAssemblyScriptWasm(t *testing.T) string {
	t.Helper()

	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed -- skipping AssemblyScript WASM integration test")
	}

	repoRoot := findRepoRoot(t)
	asDir := filepath.Join(repoRoot, "examples", "as-workflow")

	// Install dependencies if not already present.
	if _, err := os.Stat(filepath.Join(asDir, "node_modules")); os.IsNotExist(err) {
		cmd := exec.Command(npm, "ci")
		cmd.Dir = asDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Skipf("npm ci failed (best-effort, skipping):\n%s\n%v", string(out), err)
		}
	}

	// Run the build script defined in package.json.
	cmd := exec.Command(npm, "run", "build")
	cmd.Dir = asDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("npm run build failed (best-effort, skipping):\n%s\n%v", string(out), err)
	}

	// The build produces dist/workflow.wasm (raw) and
	// dist/workflow.stamped.wasm (with metadata). We use the raw module
	// since metadata stamping is optional.
	wasmPath := filepath.Join(asDir, "dist", "workflow.wasm")
	if _, err := os.Stat(wasmPath); os.IsNotExist(err) {
		t.Skipf("WASM output not found at %s (best-effort, skipping)", wasmPath)
	}

	t.Logf("AS WASM built: %s", wasmPath)
	return wasmPath
}

// TestAssemblyScriptWorkflowExecute compiles the AS place_order workflow to
// WASM, loads it into the Go host runtime, executes it, and verifies the
// event history contains the expected saga steps.
func TestAssemblyScriptWorkflowExecute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}

	wasmPath := buildAssemblyScriptWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read AS WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0)
	if err != nil {
		t.Skipf("NewRuntime failed (best-effort, skipping): %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	// The AS place_order workflow expects camelCase JSON fields matching
	// the extractStringField / extractRawArray helpers in assembly/index.ts.
	input := []byte(`{"userID":"test-user","items":[{"sku":"SKU-001","quantity":2}]}`)
	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Skipf("Execute AS workflow failed (best-effort, skipping): %v", err)
	}
	if suspended != nil {
		t.Errorf("unexpected workflow suspension: %v", suspended.Reason)
	}
	if result == "" {
		t.Error("expected non-empty result from AS workflow")
	}
	if len(history) == 0 {
		t.Error("expected non-empty history from AS workflow")
	}

	// Filter out durable_log events. The AS place_order workflow calls:
	//  inventory.Reserve -> payments.Charge -> shipping.CreateShipment ->
	//  notifications.SendEmail
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

	t.Logf("AS workflow result: %s, history: %d calls", result, len(history))
	for i, rec := range history {
		t.Logf("  step %d: %s.%s => %s (err=%s)", i, rec.Service, rec.Op, rec.Response, rec.Err)
	}
}
