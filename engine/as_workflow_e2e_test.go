package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildAssemblyScriptWasm compiles the AssemblyScript workflow crate to WASM
// and returns the path to the compiled module. It skips the test if npm or
// the AssemblyScript build chain is unavailable.
func buildAssemblyScriptWasm(t *testing.T) string {
	t.Helper()

	// Same policy as buildRustWasm and buildJavaWasm: a job that declares it
	// provides the toolchain must fail when the toolchain is missing, because
	// that means its own setup step failed silently.
	npm, err := exec.LookPath("npm")
	if err != nil {
		if toolchainRequired("assemblyscript") {
			t.Fatalf("npm not installed, but %s declares assemblyscript, so this job installs Node -- its setup step must have failed silently: %v", requireToolchainEnv, err)
		}
		t.Skip("npm not installed -- skipping AssemblyScript WASM integration test (only e2e-cross-language.yml provisions Node for this test)")
	}

	repoRoot := findRepoRoot(t)
	asDir := filepath.Join(repoRoot, "examples", "as-workflow")

	// Install dependencies if not already present.
	if _, err := os.Stat(filepath.Join(asDir, "node_modules")); os.IsNotExist(err) {
		cmd := exec.Command(npm, "ci")
		cmd.Dir = asDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("npm ci:\n%s\n%v", string(out), err)
		}
	}

	// Run the build script defined in package.json.
	cmd := exec.Command(npm, "run", "build")
	cmd.Dir = asDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("npm run build:\n%s\n%v", string(out), err)
	}

	// The build produces dist/workflow.wasm (raw) and
	// dist/workflow.stamped.wasm (with metadata). We use the raw module
	// since metadata stamping is optional.
	wasmPath := filepath.Join(asDir, "dist", "workflow.wasm")
	if _, err := os.Stat(wasmPath); os.IsNotExist(err) {
		t.Fatalf("npm run build succeeded but WASM output not found at %s", wasmPath)
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
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	// The AS place_order workflow expects camelCase JSON fields matching
	// the extractStringField / extractRawArray helpers in assembly/index.ts.
	input := []byte(`{"userID":"test-user","items":[{"sku":"SKU-001","quantity":2}]}`)
	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute AS workflow: %v", err)
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

	// Select the calls, rather than deselecting durable_log: an exclusion filter
	// admits any event kind added later and shifts every index, which is how the
	// Java e2e read "step 0: expected accounts.Withdraw, got ." for a `defer`.
	var callHistory []EventRecord
	for _, rec := range history {
		if rec.EventType == EventTypeCall {
			callHistory = append(callHistory, rec)
		}
	}
	expected := []struct{ service, op string }{
		{"inventory", "Reserve"},
		{"payments", "Charge"},
		{"shipping", "CreateShipment"},
		{"notifications", "SendEmail"},
	}
	if len(callHistory) != len(expected) {
		t.Fatalf("expected %d calls, got %d", len(expected), len(callHistory))
	}
	for i, want := range expected {
		if callHistory[i].Service != want.service || callHistory[i].Op != want.op {
			t.Errorf("step %d: expected %s.%s, got %s.%s",
				i, want.service, want.op, callHistory[i].Service, callHistory[i].Op)
		}
	}

	// The saga has to carry data between its steps, and the call sequence above
	// cannot see whether it does. This test passed for its whole existence while
	// the answer was "no": examples/as-workflow read reservationID / totalCents /
	// chargeID out of responses that spell them reservation_id / total_cents /
	// charge_id, so every extract returned "" or 0. The four calls still happened,
	// in order, carrying nothing -- payments.Charge was invoked for zero.
	//
	// Each assertion below names a value that had to survive one hop.
	if want := `"amount_cents":3299`; !strings.Contains(callHistory[1].Request, want) {
		t.Errorf("payments.Charge request %q does not contain %s -- total_cents did not survive "+
			"the hop from inventory.Reserve's response", callHistory[1].Request, want)
	}
	if want := `"reservation_id":"resv_abc123"`; !strings.Contains(callHistory[2].Request, want) {
		t.Errorf("shipping.CreateShipment request %q does not contain %s", callHistory[2].Request, want)
	}
	if want := `"charge_id":"chg_xyz789"`; !strings.Contains(callHistory[2].Request, want) {
		t.Errorf("shipping.CreateShipment request %q does not contain %s -- charge_id did not "+
			"survive the hop from payments.Charge's response", callHistory[2].Request, want)
	}
	if want := `"reservation_id":"resv_abc123"`; !strings.Contains(result, want) {
		t.Errorf("workflow result %q does not contain %s", result, want)
	}

	t.Logf("AS workflow result: %s, history: %d calls", result, len(history))
	for i, rec := range history {
		t.Logf("  step %d: %s.%s => %s (err=%s)", i, rec.Service, rec.Op, rec.Response, rec.Err)
	}
}
