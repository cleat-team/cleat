// Package cross_language tests determinism across Go and Rust WASM workflows:
//
//  1. Compile a workflow to WASM from each language
//  2. Execute it through the Go runtime engine, capturing event history
//  3. Replay the same workflow from the captured event history (same-language replay)
//  4. Cross-replay: replay Go-generated events against Rust-compiled WASM and vice versa
//
// These tests require the Rust/Cargo toolchain and the cleat Go build pipeline.
// They are NOT gated by testing.Short() — they are enabled in CI when toolchains
// are available. When a required toolchain is missing, the test skips with t.Skip.
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
	"github.com/cleat-team/cleat/engine"
)

// findProjectRoot walks up from the working directory to find the repo root.
func findProjectRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
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

// requireCargo skips the test if cargo is not installed.
func requireCargo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not installed — skipping Rust WASM cross-language test")
	}
}

// requireTinygo skips the test if tinygo is not installed.
func requireTinygo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not installed — skipping Go WASM cross-language test")
	}
}

// buildRustWasm compiles the Rust workflow crate to WASM and returns the
// path to the .wasm file.
func buildRustWasm(t *testing.T, projectRoot string) string {
	t.Helper()
	requireCargo(t)

	rustDir := filepath.Join(projectRoot, "examples", "rust-workflow")
	if _, err := os.Stat(rustDir); err != nil {
		t.Skipf("Rust workflow example not found at %s - skipping", rustDir)
	}

	cmd := exec.Command("cargo", "build", "--target", "wasm32-wasip1", "--release")
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
func buildGoWasm(t *testing.T, projectRoot, pkgPath string) string {
	t.Helper()
	requireTinygo(t)

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
// Same-language replay tests
// ---------------------------------------------------------------------------

// TestRustWorkflow_ExecuteAndReplay builds the Rust workflow, executes it
// through the Go engine, captures event history, and replays from that
// history to verify deterministic replay.
func TestRustWorkflow_ExecuteAndReplay(t *testing.T) {
	projectRoot := findProjectRoot(t)
	wasmPath := buildRustWasm(t, projectRoot)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	env := wasmtest.NewWasmTestEnv(t,
		wasmtest.WithDefName("rust-workflow"),
		wasmtest.WithDefVersion(1),
	)
	defer env.Close()

	inputJSON := `{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`

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

	expectedServices := []struct {
		svc string
		op  string
	}{
		{"inventory", "Reserve"},
		{"payments", "Charge"},
		{"shipping", "CreateShipment"},
		{"notifications", "SendEmail"},
	}

	var calls []struct{ svc, op string }
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

	replayResult, err := env.Replay(t, wasmBytes, "place_order", inputJSON, history)
	if err != nil {
		t.Fatalf("Replay Rust workflow: %v", err)
	}
	if result != replayResult {
		t.Errorf("Rust replay result mismatch:\n  execute=%q\n  replay= %q", result, replayResult)
	}

	t.Logf("Rust same-language replay: bit-identical result verified")
}

// TestRustWorkflow_ReplayEmptyHistory verifies that replaying with empty
// history triggers a fresh execution and produces the same result.
func TestRustWorkflow_ReplayEmptyHistory(t *testing.T) {
	projectRoot := findProjectRoot(t)
	wasmPath := buildRustWasm(t, projectRoot)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	env := wasmtest.NewWasmTestEnv(t)
	defer env.Close()

	inputJSON := `{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`

	result, history, err := env.Execute(t, wasmBytes, "place_order", inputJSON)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty history")
	}

	env2 := wasmtest.NewWasmTestEnv(t)
	defer env2.Close()

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
	projectRoot := findProjectRoot(t)
	wasmPath := buildRustWasm(t, projectRoot)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	env := wasmtest.NewWasmTestEnv(t)
	defer env.Close()

	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	inputJSON := json.RawMessage(`{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`)

	caller := &callRecorder{}
	engine := engine.NewEngine(rt, caller)
	result, history, _, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", inputJSON)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty history")
	}

	corrupted := make([]engine.EventRecord, len(history))
	copy(corrupted, history)
	for i := range corrupted {
		if corrupted[i].EventType == engine.EventTypeCall {
			corrupted[i].Service = "nonexistent"
			break
		}
	}

	_, _, _, _, _, replayErr := engine.Replay(ctx, wasmBytes, "place_order", inputJSON, corrupted)
	if replayErr == nil {
		t.Error("expected replay divergence error, got nil")
	} else {
		t.Logf("Divergence correctly detected: %v", replayErr)
	}

	_ = result
}

// TestGoWorkflow_ExecuteAndReplay builds a Go workflow using the cleat build
// pipeline and verifies deterministic execute-and-replay.
func TestGoWorkflow_ExecuteAndReplay(t *testing.T) {
	projectRoot := findProjectRoot(t)

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

	replayResult, err := env.Replay(t, wasmBytes, "place_order", inputJSON, history)
	if err != nil {
		t.Fatalf("Replay Go workflow: %v", err)
	}
	if result != replayResult {
		t.Errorf("Go replay result mismatch:\n  execute=%q\n  replay= %q", result, replayResult)
	}

	t.Log("Go same-language replay: bit-identical result verified")
}

// ---------------------------------------------------------------------------
// Cross-language replay tests
// ---------------------------------------------------------------------------

// expectedCrossLangCalls is the call sequence shared by the Go crosslang
// workflow and the Rust place_order entry point.
var expectedCrossLangCalls = []struct {
	svc string
	op  string
}{
	{"inventory", "Reserve"},
	{"payments", "Charge"},
	{"shipping", "CreateShipment"},
	{"notifications", "SendEmail"},
}

// TestCrossReplay_GoExec_RustReplay executes the Go crosslang workflow,
// captures its event history, then replays that history against the Rust
// WASM binary. The result must be bit-identical.
func TestCrossReplay_GoExec_RustReplay(t *testing.T) {
	projectRoot := findProjectRoot(t)

	// Build the Rust workflow.
	rustWasmPath := buildRustWasm(t, projectRoot)
	rustWasmBytes, err := os.ReadFile(rustWasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	// Build the matching Go crosslang workflow.
	goWasmPath := buildGoWasm(t, projectRoot, filepath.Join(projectRoot, "testdata", "crosslang"))
	goWasmBytes, err := os.ReadFile(goWasmPath)
	if err != nil {
		t.Fatalf("read Go WASM: %v", err)
	}

	// Execute the Go matching workflow to capture event history.
	goEnv := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("go-crosslang"))
	defer goEnv.Close()

	// Go crosslang uses Pattern B (single struct param "input"). Outer key
	// matches the Go parameter name; inner keys match snake_case struct tags.
	goInput := `{"input":{"user_id":"cross-test","cart":[{"sku":"SKU-001","quantity":2}]}}`
	goResult, goHistory, err := goEnv.Execute(t, goWasmBytes, "place_order", goInput)
	if err != nil {
		t.Fatalf("Execute Go crosslang workflow: %v", err)
	}
	if goResult == "" {
		t.Fatal("expected non-empty result from Go crosslang workflow")
	}
	if len(goHistory) == 0 {
		t.Fatal("expected non-empty event history from Go workflow")
	}
	t.Logf("Go crosslang execute: result=%q, events=%d", goResult, len(goHistory))

	// Verify Go call sequence.
	verifyCallSequence(t, goHistory, expectedCrossLangCalls)

	// Replay Go event history against Rust WASM.
	rustEnv := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("rust-workflow"))
	defer rustEnv.Close()

	// The Rust workflow expects snake_case input (serde default).
	rustInput := `{"user_id":"cross-test","cart":[{"sku":"SKU-001","quantity":2}]}`
	rustReplayResult, err := rustEnv.Replay(t, rustWasmBytes, "place_order", rustInput, goHistory)
	if err != nil {
		t.Fatalf("Replay Go history against Rust WASM: %v", err)
	}

	if goResult != rustReplayResult {
		t.Errorf("cross-language replay mismatch (Go exec → Rust replay):\n  Go execute result:  %q\n  Rust replay result: %q", goResult, rustReplayResult)
	} else {
		t.Logf("Cross-language replay verified: Go exec → Rust replay produces bit-identical result %q", goResult)
	}
}

// TestCrossReplay_RustExec_GoReplay executes the Rust workflow, captures its
// event history, then replays that history against the Go crosslang WASM
// binary. The result must be bit-identical.
func TestCrossReplay_RustExec_GoReplay(t *testing.T) {
	projectRoot := findProjectRoot(t)

	// Build the Rust workflow.
	rustWasmPath := buildRustWasm(t, projectRoot)
	rustWasmBytes, err := os.ReadFile(rustWasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	// Build the matching Go crosslang workflow.
	goWasmPath := buildGoWasm(t, projectRoot, filepath.Join(projectRoot, "testdata", "crosslang"))
	goWasmBytes, err := os.ReadFile(goWasmPath)
	if err != nil {
		t.Fatalf("read Go WASM: %v", err)
	}

	// Execute the Rust workflow to capture event history.
	rustEnv := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("rust-workflow"))
	defer rustEnv.Close()

	rustInput := `{"user_id":"cross-test","cart":[{"sku":"SKU-001","quantity":2}]}`
	rustResult, rustHistory, err := rustEnv.Execute(t, rustWasmBytes, "place_order", rustInput)
	if err != nil {
		t.Fatalf("Execute Rust workflow: %v", err)
	}
	if rustResult == "" {
		t.Fatal("expected non-empty result from Rust workflow")
	}
	if len(rustHistory) == 0 {
		t.Fatal("expected non-empty event history from Rust workflow")
	}
	t.Logf("Rust execute: result=%q, events=%d", rustResult, len(rustHistory))

	// Verify Rust call sequence matches expected.
	verifyCallSequence(t, rustHistory, expectedCrossLangCalls)

	// Replay Rust event history against Go crosslang WASM.
	goEnv := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("go-crosslang"))
	defer goEnv.Close()

	// Go crosslang uses Pattern B (single struct param "input").
	goInput := `{"input":{"user_id":"cross-test","cart":[{"sku":"SKU-001","quantity":2}]}}`
	goReplayResult, err := goEnv.Replay(t, goWasmBytes, "place_order", goInput, rustHistory)
	if err != nil {
		t.Fatalf("Replay Rust history against Go WASM: %v", err)
	}

	if rustResult != goReplayResult {
		t.Errorf("cross-language replay mismatch (Rust exec → Go replay):\n  Rust execute result: %q\n  Go replay result:    %q", rustResult, goReplayResult)
	} else {
		t.Logf("Cross-language replay verified: Rust exec → Go replay produces bit-identical result %q", rustResult)
	}
}

// TestCrossReplay_DivergenceDetection verifies that the engine detects
// replay divergence when cross-language history is corrupted.
func TestCrossReplay_DivergenceDetection(t *testing.T) {
	projectRoot := findProjectRoot(t)

	rustWasmPath := buildRustWasm(t, projectRoot)
	rustWasmBytes, err := os.ReadFile(rustWasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	env := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("rust-workflow"))
	defer env.Close()

	rustInput := `{"user_id":"test-user","cart":[{"sku":"SKU-001","quantity":2}]}`

	_, history, err := env.Execute(t, rustWasmBytes, "place_order", rustInput)
	if err != nil {
		t.Fatalf("Execute Rust workflow: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty history")
	}

	// Corrupt a service call in the history.
	corrupted := make([]engine.EventRecord, len(history))
	copy(corrupted, history)
	for i := range corrupted {
		if corrupted[i].EventType == engine.EventTypeCall {
			corrupted[i].Service = "nonexistent"
			break
		}
	}

	env2 := wasmtest.NewWasmTestEnv(t, wasmtest.WithDefName("rust-workflow"))
	defer env2.Close()

	_, replayErr := env2.Replay(t, rustWasmBytes, "place_order", rustInput, corrupted)
	if replayErr == nil {
		t.Error("expected cross-language replay divergence error, got nil")
	} else {
		t.Logf("Cross-language divergence correctly detected: %v", replayErr)
	}
}

// ---------------------------------------------------------------------------
// callRecorder
// ---------------------------------------------------------------------------

type callRecorder struct {
	calls []engine.EventRecord
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
	c.calls = append(c.calls, engine.EventRecord{
		EventType: engine.EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
		Response:  resp,
	})
	return resp, nil
}

// verifyCallSequence checks that the event history contains the expected
// service/operation calls in order.
func verifyCallSequence(t *testing.T, history []engine.EventRecord, expected []struct{ svc, op string }) {
	t.Helper()

	var calls []struct{ svc, op string }
	for _, ev := range history {
		if ev.Service != "" || ev.Op != "" {
			calls = append(calls, struct{ svc, op string }{ev.Service, ev.Op})
		}
	}
	if len(calls) < len(expected) {
		t.Fatalf("expected at least %d service calls, got %d", len(expected), len(calls))
	}
	for i, exp := range expected {
		if calls[i].svc != exp.svc || calls[i].op != exp.op {
			t.Errorf("call %d: expected %s.%s, got %s.%s", i, exp.svc, exp.op, calls[i].svc, calls[i].op)
		}
	}
}
