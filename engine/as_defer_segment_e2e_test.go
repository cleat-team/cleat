//go:build cgo

package engine

import (
	"context"
	"os"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// The end-to-end half of IMPROVEMENT-PLAN 3.106, and what lets
// `assemblyscript` into deferSegmentLanguages.
//
// #645 gave the AS SDK `stopRequested` and put it on the nine host calls the
// host can refuse, checked from both ends. Neither end runs an AS guest, so
// the fence in Execute stayed closed.
//
// **This language was expected to be the one that could not pass.** §3.106
// recorded the shape honestly: AssemblyScript builds `--runtime stub` and has
// no exceptions, so a stop is a FLAG, not an unwind, and a workflow body that
// ignores the flag keeps running. Three of the four SDKs end their segment by
// unwinding out of the call; this one returns from it normally. The predicted
// consequence was that the generated wrapper would reach `runDeferred(h)` on
// its ordinary path and consume the table with every body's call refused --
// 3.81's destruction, in the one language that cannot prevent it by unwinding.
//
// That is not what happens, and the reason is worth stating rather than
// leaving to the green tick. The transformer already emits the suspension
// check BEFORE the drain (`packages/cleat-as/transform/index.js`, "Step 4"
// then "Step 4b"), for 3.73's reason rather than this one: a workflow that
// suspends at a `cleatSleep` has not exited and must not fire its cleanup. The
// same ordering is exactly what a defer segment needs. So the guarantee here
// is weaker than the other three -- the body runs on, and burns instructions
// doing it -- but it is weaker in a way that costs nothing durable: every call
// it makes past the stop is refused host-side by `stopBeforeNewWork`, and the
// defer table survives to be drained by the host.
//
// The test names begin with TestAssemblyScript because e2e-cross-language.yml
// is the only job that installs Node and selects with
// `-run "TestRust|TestPython|TestAssemblyScript|TestJava"`. A cross-language
// test named anything else skips in every job that runs it and never runs in
// the one that could. See engine/java_defer_segment_e2e_test.go, where that
// mistake was made and caught.

// TestAssemblyScriptDeferSegmentRunsOnlyTheDefers runs the AS `defer_order`
// fixture -- two defers, then one call of the workflow's own -- as a defer
// segment.
//
// The control is TestAssemblyScriptWorkflowDefersRun in
// engine/as_workflow_defer_test.go, which runs the SAME entry point with no
// defer phase and asserts [body second first]. That pairing is what separates
// "the stop is conditional on the segment" from "this guest stopped working",
// so if this test is ever changed, check that one still exercises the fixture.
func TestAssemblyScriptDeferSegmentRunsOnlyTheDefers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}

	wasmBytes, err := os.ReadFile(buildAssemblyScriptWasm(t))
	if err != nil {
		t.Fatalf("read AS WASM: %v", err)
	}

	// deferSegmentLanguages is keyed by what the guest declares, and
	// DetectLanguage returns the module's own metadata field verbatim (3.83).
	// Assert it rather than assume it: a module that stopped declaring
	// "assemblyscript" would take this test past a fence that never fired.
	if lang := wasm.DetectLanguage(wasmBytes); lang != "assemblyscript" {
		t.Fatalf("the built module declares language %q, not \"assemblyscript\"", lang)
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
		WithWorkflowID("wf-as-defer-segment"),
		WithDeferPhase())

	res, _, susp, _, _, err := eng.Execute(ctx, wasmBytes,
		"defer_order", []byte(`{"userID":"u1"}`))
	if err != nil {
		t.Fatalf("the defer segment failed: %v", err)
	}
	if susp == nil {
		t.Fatalf("the AssemblyScript defer segment did not suspend; it returned %q.\n\n"+
			"It reported an outcome for a workflow whose outcome was already "+
			"decided. In this SDK that is the transformer's Step 4 check -- if "+
			"`isWorkflowSuspended()` no longer runs before the wrapper writes its "+
			"result, nothing else stops it, because there is no exception to "+
			"unwind on. Operations recorded: %v", res, operationsCalled(caller))
	}

	got := operationsCalled(caller)
	for _, op := range got {
		if op == "body" {
			t.Fatalf("the AssemblyScript workflow body reached the ServiceCaller: %v.\n\n"+
				"This is the HOST half: stopBeforeNewWork refuses a body call past "+
				"the frontier before the ServiceCaller is reached, so getting here "+
				"means the refusal is gone, not that the AS SDK stopped decoding "+
				"the sentinel.", got)
		}
	}

	want := []string{"second", "first"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the AssemblyScript defer segment recorded %v, want exactly %v.\n\n"+
			"An empty list is 3.81's consumption, and in THIS SDK it is one line "+
			"away at all times: the transformer emits `runDeferred(h)` immediately "+
			"after the `isWorkflowSuspended()` check, and moving the drain above "+
			"the check takes the whole table with every body's call refused. "+
			"There is no unwind here to skip the drain for you.\n"+
			"Reversed order means the drain runs registration-order, and a defer "+
			"releases what the defer before it acquired.", got, want)
	}
}
