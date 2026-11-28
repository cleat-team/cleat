package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestPythonSideEffectFreezesAValueEndToEnd is cleat#4113's end-to-end test for the Python SDK: a REAL componentized guest against a REAL engine.
//
// The function must run exactly once across a first run and a replay, and the replay must return the recorded value. The fixture counts runs in
// the module (a fresh instance per execution), so the first run reports ran=1 and a replay that did not call the function reports ran=0.
func TestPythonSideEffectFreezesAValueEndToEnd(t *testing.T) {
	ctx := context.Background()

	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, pythonWasm.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	wasmPath := pythonWasm.compileWorkflow(t, "side_effect_workflow.py", "side_effect_workflow")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read compiled WASM: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	wt, wtErr := NewWasmtimeBackend(ctx)
	if wtErr != nil {
		t.Fatalf("NewWasmtimeBackend: %v (python routes here; there is no fallback to test)", wtErr)
	}
	engine := NewEngine(rt, &mockCaller{}, WithBackends(WasmtimeLanguages, wt))

	type outcome struct {
		Value string `json:"value"`
		Ran   int    `json:"ran"`
	}
	read := func(raw string) outcome {
		t.Helper()
		// A componentized guest returns a JSON STRING holding the JSON (see parsePythonCronResult).
		var inner string
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &inner); err != nil {
			inner = raw
		}
		var o outcome
		if err := json.Unmarshal([]byte(inner), &o); err != nil {
			t.Fatalf("result %q is not the fixture's JSON: %v", raw, err)
		}
		return o
	}

	first, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "run", json.RawMessage(`{}`))
	if err != nil || suspended != nil {
		t.Fatalf("first run: err=%v suspended=%v", err, suspended)
	}
	a := read(first)
	if a.Ran != 1 || a.Value != "computed-in-run-1" {
		t.Fatalf("first run = %+v, want the function run once", a)
	}

	replayed, _, rsuspended, _, _, err := engine.Replay(ctx, wasmBytes, "run", json.RawMessage(`{}`), history)
	if err != nil || rsuspended != nil {
		t.Fatalf("replay: err=%v suspended=%v", err, rsuspended)
	}
	b := read(replayed)
	if b.Value != a.Value {
		t.Errorf("the replay returned %q, want the recorded %q", b.Value, a.Value)
	}
	if b.Ran != 0 {
		t.Errorf("the function ran %d time(s) in the replay, want 0: across the first run and the replay it must run exactly once", b.Ran)
	}
}
