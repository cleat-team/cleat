//go:build cgo

package engine

import (
	"context"
	"os"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// The end-to-end half of IMPROVEMENT-PLAN 3.107, and what lets `rust` into
// deferSegmentLanguages. The last of the four core-module SDKs; Python is a
// Component Model guest and a different problem.
//
// #647 gave the Rust SDK `stop_requested` on the eight host calls the host can
// refuse, checked from both ends. Neither end runs a Rust guest, so the fence
// in Execute stayed closed.
//
// **The Rust fixture is the one that tests the weakest link on purpose.**
// `examples/rust-workflow`'s `defer_order` makes its body call as
//
//	h.cleat_call("inventory", "body", "{}");
//
// with the return value DISCARDED -- no `?`, no `let _ =` even. Six of the
// eight guarded functions return `(String, Option<String>)` rather than
// `Result`, so a body can drop the error half and carry on, and this one does.
// That is exactly why `stop_requested` routes through `suspend()` rather than
// constructing `CallError::Suspended` itself: `suspend()` sets the flag that
// `#[cleat_entry]` reads after the body returns, and that flag is what ends the
// segment when the body has thrown the error away. A test written against a
// fixture that used `?` would pass without ever exercising the backstop.
//
// The name begins with TestRust because e2e-cross-language.yml is the only job
// that installs the Rust wasm32-wasip1 toolchain for ./engine and selects with
// `-run "TestRust|TestPython|TestAssemblyScript|TestJava"`.

// TestRustDeferSegmentRunsOnlyTheDefers runs `defer_order` -- two defers, then
// one call of the workflow's own -- as a defer segment.
//
// The control is TestRustWorkflowDefersRun in engine/rust_workflow_test.go,
// which runs the SAME entry point with no defer phase and asserts
// [body second first]. That pairing separates "the stop is conditional on the
// segment" from "this guest stopped working".
func TestRustDeferSegmentRunsOnlyTheDefers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM integration test in short mode")
	}

	wasmBytes, err := os.ReadFile(buildRustWasm(t))
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	// deferSegmentLanguages is keyed by what the guest declares, and
	// DetectLanguage returns the module's own metadata verbatim (3.83).
	if lang := wasm.DetectLanguage(wasmBytes); lang != "rust" {
		t.Fatalf("the built module declares language %q, not \"rust\"", lang)
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

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-rust-defer-segment"),
		WithDeferPhase())

	res, _, susp, _, _, err := eng.Execute(ctx, wasmBytes,
		"defer_order", []byte(`{"user_id":"u1","cart":[]}`))
	if err != nil {
		t.Fatalf("the defer segment failed: %v", err)
	}
	if susp == nil {
		t.Fatalf("the Rust defer segment did not suspend; it returned %q.\n\n"+
			"It reported an outcome for a workflow whose outcome was already "+
			"decided. The fixture discards the Err from its body call, so the "+
			"only thing that can stop it is the flag `suspend()` sets and "+
			"`#[cleat_entry]` reads -- if stop_requested stopped calling "+
			"suspend() and returned the Err alone, this is what it would look "+
			"like. Operations recorded: %v", res, operationsCalled(caller))
	}

	got := operationsCalled(caller)
	for _, op := range got {
		if op == "body" {
			t.Fatalf("the Rust workflow body reached the ServiceCaller: %v.\n\n"+
				"This is the HOST half: stopBeforeNewWork refuses a body call "+
				"past the frontier before the ServiceCaller is reached, so "+
				"getting here means the refusal is gone rather than the guest "+
				"decode.", got)
		}
	}

	want := []string{"second", "first"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the Rust defer segment recorded %v, want exactly %v.\n\n"+
			"An empty list is 3.81's consumption: `#[cleat_entry]` drained the "+
			"table on its way out, so the host's own drain found nothing. The "+
			"wrapper must return SUSPEND_SENTINEL on the flag BEFORE it calls "+
			"run_deferred -- see crates/cleat-macro/src/entry.rs.\n"+
			"A list of one is the shape the AssemblyScript SDK had (3.106): a "+
			"drain that stops when it sees the suspension flag, reading a flag "+
			"that was set by the BODY rather than by a defer. Rust's "+
			"run_deferred deliberately does not stop, so it should not be "+
			"reachable here -- if it is, that reasoning is wrong.\n"+
			"Reversed order means the drain runs registration-order.", got, want)
	}
}
