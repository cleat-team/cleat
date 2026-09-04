//go:build cgo

package engine

import (
	"context"
	"os"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// pythonDeferSegment builds examples/defer_order_workflow.py, runs it through
// the engine, and returns what the ServiceCaller saw.
//
// deferPhase selects the segment: true runs it as a defer segment
// (WithDeferPhase), false as an ordinary execution. Both go through the same
// helper on purpose -- the pair is what separates "the stop is conditional on
// the segment" from "this guest stopped working at all".
func pythonDeferSegment(t *testing.T, wfID string, deferPhase bool) (
	result string, susp *SuspendResult, ops []string,
) {
	t.Helper()

	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s",
				requireToolchainEnv, pythonWasm.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	wasmPath := pythonWasm.compileWorkflow(t, "defer_order_workflow.py", "defer_order")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Python WASM: %v", err)
	}

	// deferSegmentLanguages is keyed by what the guest declares, and
	// DetectLanguage returns the module's own metadata verbatim (3.83).
	if lang := wasm.DetectLanguage(wasmBytes); lang != "python" {
		t.Fatalf("the built module declares language %q, not \"python\"", lang)
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
	opts := []EngineOption{
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID(wfID),
	}
	if deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	eng := NewEngine(rt, caller, opts...)

	// "run" is the WIT world's only export; the @cleat_entry wrapper
	// dispatches inside the guest.
	res, _, suspended, _, _, err := eng.Execute(ctx, wasmBytes, "run", []byte(`{}`))
	if err != nil {
		t.Fatalf("execute (deferPhase=%v) failed: %v", deferPhase, err)
	}
	return res, suspended, operationsCalled(caller)
}

// TestPythonDeferSegmentRunsOnlyTheDefers runs defer_order -- two defers, then
// one call of the workflow's own -- as a defer segment.
func TestPythonDeferSegmentRunsOnlyTheDefers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}

	res, susp, got := pythonDeferSegment(t, "wf-python-defer-segment", true)

	if susp == nil {
		t.Fatalf("the Python defer segment did not suspend; it returned %q.\n\n"+
			"Operations recorded: %v", res, got)
	}
	for _, op := range got {
		if op == "body" {
			t.Fatalf("the Python workflow body reached the ServiceCaller: %v", got)
		}
	}
	want := []string{"second", "first"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the Python defer segment recorded %v, want exactly %v", got, want)
	}
}

// TestPythonOrdinarySegmentRunsTheBody is the control for the test above: the
// same entry point, the same fixture, no defer phase.
func TestPythonOrdinarySegmentRunsTheBody(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}

	res, susp, got := pythonDeferSegment(t, "wf-python-ordinary", false)

	if susp != nil {
		t.Fatalf("the ordinary execution suspended: %v", susp.Reason)
	}
	want := []string{"body", "second", "first"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("the ordinary execution recorded %v, want exactly %v (result %q)", got, want, res)
	}
}
