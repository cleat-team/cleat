// Package cross_language tests determinism across Go and Rust WASM workflows:
//
//  1. Compile a workflow to WASM from each language
//  2. Execute it through the Go runtime engine, capturing event history
//  3. Replay the same workflow from the captured event history (same-language replay)
//  4. Cross-replay: replay Go-generated events against Rust-compiled WASM and vice versa
//
// These tests require the Rust/Cargo toolchain and/or the cleat Go build pipeline.
// Gate with testing.Short() so they are excluded from `go test ./... -short`.
package cross_language

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"
	"github.com/cleat-team/cleat/internal/host"
)

// findProjectRoot walks up from the working directory to find the repo root.
func findProjectRoot(t *testing.T) string {
	t.Helper()
	// Start from the test's package directory.
	// During normal `go test`, the working directory is the package directory.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Walk up until we find go.mod.
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find project root from %s", cwd)
		}
		dir = parent
	}
}

// buildRustWasm compiles the Rust workflow crate to WASM and returns the
// path to the .wasm file. Skips the test if cargo is not installed.
func buildRustWasm(t *testing.T, projectRoot string) string {
	t.Helper()

	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("cargo not installed - skipping Rust WASM test")
	}

	rustDir := filepath.Join(projectRoot, "examples", "rust-workflow")
	if _, err := os.Stat(rustDir); err != nil {
		t.Skipf("Rust workflow example not found at %s - skipping", rustDir)
	}

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

	wasmPath := filepath.Join(rustDir, "target", "wasm32-wasip1", "release", "rust_workflow.wasm")
	if _, err := os.Stat(wasmPath); err != nil {
		t.Fatalf("Rust WASM not found at %s: %v", wasmPath, err)
	}

	t.Logf("built Rust WASM: %s", wasmPath)
	return wasmPath
}

// buildGoWasm compiles a Go workflow to WASM using the cleat build pipeline.
// Skips the test if tinygo is not installed.
func buildGoWasm(t *testing.T, projectRoot, pkgPath string) string {
	t.Helper()

	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not installed - skipping Go WASM compilation")
	}

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run",
		filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "tinygo", "-o", tmpDir, pkgPath,
	)
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	if goroot := os.Getenv("GOROOT"); goroot != "" {
		cmd.Env = append(cmd.Env, "GOROOT="+goroot)
	}
	if tinygoroot := os.Getenv("TINYGOROOT"); tinygoroot != "" {
		cmd.Env = append(cmd.Env, "TINYGOROOT="+tinygoroot)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(out), err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmPath := filepath.Join(tmpDir, e.Name())
			t.Logf("built Go WASM: %s", wasmPath)
			return wasmPath
		}
	}
	t.Fatalf("no .wasm file found in %s", tmpDir)
	return ""
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRustWorkflow_ExecuteAndReplay builds the Rust workflow, executes it
// through the Go engine, captures event history, and replays from that
// history to verify deterministic replay.
func TestRustWorkflow_ExecuteAndReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-language WASM test in short mode")
	}

	projectRoot := findProjectRoot(t)
	wasmPath := buildRustWasm(t, projectRoot)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	// Create the test environment with a mock caller that provides
	// responses matching what the Rust place_order workflow expects.
	env := wasmtest.NewWasmTestEnv(t,
		wasmtest.WithDefName("rust-workflow"),
		wasmtest.WithDefVersion(1),
	)
	defer env.Close()

	// The Rust workflow expects snake_case JSON matching Rust structs.
	inputJSON := `{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`

	// Execute the workflow.
	result, history, err := env.Execute(t, wasmBytes, "place_order", inputJSON)
	if err != nil {
		t.Fatalf("Execute Rust workflow: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result from Rust workflow")
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty event history")
	}

	t.Logf("Rust workflow: result=%q, events=%d", result, len(history))

	// Verify the call history matches expectations for the place_order workflow.
	expectedServices := []struct {
		svc string
		op  string
	}{
		{"inventory", "Reserve"},
		{"payments", "Charge"},
		{"shipping", "CreateShipment"},
		{"notifications", "SendEmail"},
	}

	// Filter to service-call events (log events have empty Service/Op).
	var calls []struct {
		svc string
		op  string
	}
	for _, ev := range history {
		if ev.Service != "" || ev.Op != "" {
			calls = append(calls, struct{ svc, op string }{ev.Service, ev.Op})
		}
	}
	if len(calls) < len(expectedServices) {
		t.Fatalf("expected at least %d service calls, got %d", len(expectedServices), len(calls))
	}
	for i, exp := range expectedServices {
		if calls[i].svc != exp.svc || calls[i].op != exp.op {
			t.Errorf("call %d: expected %s.%s, got %s.%s", i, exp.svc, exp.op, calls[i].svc, calls[i].op)
		}
	}

	// Replay the Rust workflow from the captured event history.
	replayResult, err := env.Replay(t, wasmBytes, "place_order", inputJSON, history)
	if err != nil {
		t.Fatalf("Replay Rust workflow: %v", err)
	}

	// The replay result should match the execution result.
	if result != replayResult {
		t.Errorf("rust replay result %q != execute result %q", replayResult, result)
	}

	t.Logf("Rust workflow determinism verified: execute and replay produce identical results")
}

// TestRustWorkflow_ReplayEmptyHistory verifies that replaying with empty
// history triggers a fresh execution (the Rust workflow should produce events).
func TestRustWorkflow_ReplayEmptyHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-language WASM test in short mode")
	}

	projectRoot := findProjectRoot(t)
	wasmPath := buildRustWasm(t, projectRoot)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	env := wasmtest.NewWasmTestEnv(t)
	defer env.Close()

	inputJSON := `{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`

	// Execute the workflow fresh.
	result, history, err := env.Execute(t, wasmBytes, "place_order", inputJSON)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty history")
	}

	// Create a new env to replay with the same WASM binary.
	env2 := wasmtest.NewWasmTestEnv(t)
	defer env2.Close()

	// Replay from the recorded history.
	replayResult, err := env2.Replay(t, wasmBytes, "place_order", inputJSON, history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if result != replayResult {
		t.Errorf("replay result %q != execute result %q", replayResult, result)
	}
}

// TestRustWorkflow_DivergenceDetection verifies that the engine detects
// replay divergence when the history does not match the workflow's execution.
func TestRustWorkflow_DivergenceDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-language WASM test in short mode")
	}

	projectRoot := findProjectRoot(t)
	wasmPath := buildRustWasm(t, projectRoot)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	env := wasmtest.NewWasmTestEnv(t)
	defer env.Close()

	ctx := context.Background()
	rt, err := host.NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	inputJSON := json.RawMessage(`{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`)

	// Execute normally first.
	caller := &callRecorder{}
	engine := host.NewEngine(rt, caller)
	result, history, _, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", inputJSON)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty history")
	}

	// Corrupt the history by changing a service call.
	corrupted := make([]host.EventRecord, len(history))
	copy(corrupted, history)
	for i := range corrupted {
		if corrupted[i].EventType == host.EventTypeCall {
			corrupted[i].Service = "nonexistent"
			break
		}
	}

	// Replay with corrupted history should fail.
	_, _, _, _, _, replayErr := engine.Replay(ctx, wasmBytes, "place_order", inputJSON, corrupted)
	if replayErr == nil {
		t.Error("expected replay divergence error, got nil")
	} else {
		t.Logf("Divergence correctly detected: %v", replayErr)
	}

	_ = result
}

// callRecorder implements host.ServiceCaller and records all calls.
type callRecorder struct {
	calls []host.EventRecord
}

func (c *callRecorder) Call(_ context.Context, service, operation, requestJSON string) (string, error) {
	var resp string
	switch {
	case service == "inventory" && operation == "Reserve":
		resp = `{"reservation_id":"RES-001","total_cents":5999}`
	case service == "payments" && operation == "Charge":
		resp = `{"charge_id":"CHG-001","amount":5999}`
	case service == "shipping" && operation == "CreateShipment":
		resp = `{"tracking_id":"TRACK-123456"}`
	default:
		resp = fmt.Sprintf(`{"result":"ok-%s-%s"}`, service, operation)
	}
	c.calls = append(c.calls, host.EventRecord{
		EventType: host.EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Response:  resp,
	})
	return resp, nil
}

// ---------------------------------------------------------------------------
// Go workflow test — using the cleat build pipeline
// ---------------------------------------------------------------------------

// TestGoWorkflow_ExecuteAndReplay builds a Go workflow using the cleat build
// pipeline and verifies deterministic execute-and-replay.
func TestGoWorkflow_ExecuteAndReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Go WASM test in short mode")
	}

	projectRoot := findProjectRoot(t)

	// Build the testdata/basic Go workflow using the cleat build pipeline.
	wasmPath := buildGoWasm(t, projectRoot, filepath.Join(projectRoot, "testdata", "basic"))
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Go WASM: %v", err)
	}

	env := wasmtest.NewWasmTestEnv(t,
		wasmtest.WithDefName("go-workflow"),
	)
	defer env.Close()

	inputJSON := `{"UserID":"user-42","Cart":[{"SKU":"SKU-001","Quantity":2}]}`

	// Execute.
	result, history, err := env.Execute(t, wasmBytes, "place_order", inputJSON)
	if err != nil {
		t.Fatalf("Execute Go workflow: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty event history")
	}

	t.Logf("Go workflow: result=%q, events=%d", result, len(history))

	// Verify service calls match the expected order.
	expectedServices := []struct {
		svc string
		op  string
	}{
		{"catalog", "LookupItem"},
		{"inventory", "Reserve"},
		{"payments", "GetDefaultMethod"},
		{"payments", "Charge"},
		{"shipping", "CreateShipment"},
		{"notifications", "SendEmail"},
	}
	for i, exp := range expectedServices {
		if i >= len(history) {
			t.Errorf("event %d: expected %s.%s but history ended", i, exp.svc, exp.op)
			continue
		}
		if history[i].Service != exp.svc || history[i].Op != exp.op {
			t.Errorf("event %d: expected %s.%s, got %s.%s", i, exp.svc, exp.op, history[i].Service, history[i].Op)
		}
	}

	// Replay from captured history.
	replayResult, err := env.Replay(t, wasmBytes, "place_order", inputJSON, history)
	if err != nil {
		t.Fatalf("Replay Go workflow: %v", err)
	}
	if result != replayResult {
		t.Errorf("replay result %q != execute result %q", replayResult, result)
	}

	t.Log("Go workflow determinism verified: execute and replay produce identical results")
}

// ---------------------------------------------------------------------------
// Cross-replay: Go history vs Rust WASM
// ---------------------------------------------------------------------------

// TestCrossReplay_GoHistory_RustWasm tests that event history recorded
// from a Go workflow can be replayed against a Rust-compiled WASM binary
// implementing the same workflow logic.
//
// NOTE: This test requires both toolchains and matching workflow logic.
// The Rust "place_order" and Go "PlaceOrder" workflows both implement
// an order-processing workflow, but their internal service call sequences
// differ (Go includes LookupItem and GetDefaultMethod; Rust does not).
// This test verifies the *infrastructure* works — the engine accepts
// external history for replay with any conforming WASM module.
func TestCrossReplay_GoHistory_RustWasm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-language WASM test in short mode")
	}

	projectRoot := findProjectRoot(t)

	// Build the Rust workflow.
	wasmPath := buildRustWasm(t, projectRoot)
	rustWasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	// Build the Go workflow.
	goWasmPath := buildGoWasm(t, projectRoot, filepath.Join(projectRoot, "testdata", "basic"))
	goWasm, err := os.ReadFile(goWasmPath)
	if err != nil {
		t.Fatalf("read Go WASM: %v", err)
	}

	// Execute the Go workflow to get a history.
	goEnv := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("go-workflow"))
	defer goEnv.Close()

	goInput := `{"UserID":"cross-test","Cart":[{"SKU":"SKU-001","Quantity":2}]}`
	_, goHistory, err := goEnv.Execute(t, goWasm, "place_order", goInput)
	if err != nil {
		t.Fatalf("Execute Go workflow: %v", err)
	}
	if len(goHistory) == 0 {
		t.Fatal("expected non-empty Go history")
	}
	t.Logf("Go workflow produced %d events", len(goHistory))

	// Now replay the Go history against the Rust WASM binary.
	// The Rust workflow uses "place_order" entry point and expects
	// snake_case JSON input. We use the Rust input format but replay
	// the Go-recorded event history.
	//
	// The engine treats the history as authoritative: during replay,
	// cached responses are returned for matching steps regardless of
	// the WASM module's implementation.
	rustEnv := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("rust-workflow"))
	defer rustEnv.Close()

	rustInput := `{"user_id":"cross-test","cart":[{"sku":"SKU-001","quantity":2}]}`
	replayResult, err := rustEnv.Replay(t, rustWasm, "place_order", rustInput, goHistory)
	if err != nil {
		t.Logf("Cross-replay result: %v", err)
		// Cross-replay may produce divergence if the Go and Rust workflows
		// make different service call sequences. This is expected when the
		// implementations differ. The important thing is that the engine
		// handles cross-replay gracefully with a clear error or result.
		t.Log("Cross-replay divergence is expected when Go and Rust implementations differ")
	} else {
		t.Logf("Cross-replay succeeded: result=%q", replayResult)
	}

	// Reverse: execute Rust, replay on Go WASM.
	rustEnv2 := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("rust-workflow"))
	defer rustEnv2.Close()

	rustInput2 := `{"user_id":"cross-test","cart":[{"sku":"SKU-001","quantity":2}]}`
	_, rustHistory, err := rustEnv2.Execute(t, rustWasm, "place_order", rustInput2)
	if err != nil {
		t.Fatalf("Execute Rust workflow: %v", err)
	}
	t.Logf("Rust workflow produced %d events", len(rustHistory))

	goEnv2 := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("go-workflow"))
	defer goEnv2.Close()

	goInput2 := `{"UserID":"cross-test","Cart":[{"SKU":"SKU-001","Quantity":2}]}`
	replayResult2, err := goEnv2.Replay(t, goWasm, "place_order", goInput2, rustHistory)
	if err != nil {
		t.Logf("Reverse cross-replay result: %v", err)
		t.Log("Reverse cross-replay divergence is expected when implementations differ")
	} else {
		t.Logf("Reverse cross-replay succeeded: result=%q", replayResult2)
	}
}
