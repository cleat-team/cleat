//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// cleat#4113, Rust guest: a side effect FREEZES a value. examples/rust-workflow's side_effect_probe takes one side effect and reports its value
// and how often its function ran in THIS execution (each execution is a fresh WASM instance). The function must run exactly once across the first
// run and a replay, and the replay must return the recorded value.
func TestRustSideEffectFunctionRunsOnceAcrossFirstRunAndReplay(t *testing.T) {
	path := buildRustWasm(t) // skips only when cargo or wasm32-wasip1 is absent
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	eng := NewEngine(rt, &mockCaller{}, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-rust-side-effect"))

	type outcome struct {
		Value string `json:"value"`
		Ran   int    `json:"ran"`
	}
	read := func(raw string) outcome {
		t.Helper()
		// The Rust entry returns a string holding JSON; accept the bare object too, the shape is not what this test is about.
		var inner string
		if json.Unmarshal([]byte(raw), &inner) != nil {
			inner = raw
		}
		var o outcome
		if err := json.Unmarshal([]byte(inner), &o); err != nil {
			t.Fatalf("result %q is not the probe's JSON: %v", raw, err)
		}
		return o
	}

	input := json.RawMessage(`{"user_id":"u","cart":[]}`)
	first, history, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "side_effect_probe", input)
	if err != nil || suspended != nil {
		t.Fatalf("first run: err=%v suspended=%v", err, suspended)
	}
	a := read(first)
	if a.Ran != 1 || a.Value != "computed-in-run-1" {
		t.Fatalf("first run = %+v, want the function run once", a)
	}

	replayed, _, rsuspended, _, _, err := eng.Replay(ctx, wasmBytes, "side_effect_probe", input, history)
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
