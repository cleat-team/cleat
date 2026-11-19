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

// cleat#4194: a Python workflow that registers a defer and then SLEEPS is suspended, not finished, and its cleanup must not run at the
// suspension. Before the fix the component path drained a guest's defer table at every suspension, so the cleanup ran at the first sleep (and
// was recorded in history, where the resumed run's next call belonged). An ORDINARY execution, no WithDeferPhase: the segment variants above are
// where a drain is intended.
func TestPythonDeferRegisteredBeforeASleepDoesNotRunAtTheSuspension(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a Python workflow with componentize-py")
	}
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}
	wasmBytes, err := os.ReadFile(h.compileWorkflow(t, "defer_then_sleep_workflow.py", "defer_then_sleep"))
	if err != nil {
		t.Fatalf("read Python WASM: %v", err)
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
	eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-py-defer-sleep"))
	_, hist, susp, _, _, err := eng.Execute(ctx, wasmBytes, "run", []byte(`{}`))
	if err != nil || susp == nil {
		t.Fatalf("the run should suspend in the sleep: err=%v suspended=%v", err, susp != nil)
	}
	if ops := operationsCalled(caller); len(ops) != 1 || ops[0] != "before" {
		t.Fatalf("the run made calls %v, want exactly the one before the sleep: the cleanup ran while the workflow was only sleeping", ops)
	}
	// The resumed run: the recorded call looks older than the sleep, so it is served; the call after it is made and the cleanup runs last.
	for i := range hist {
		if hist[i].EventType == EventTypeCall {
			hist[i].TimestampMs -= 6000
		}
	}
	caller2 := &mockCaller{}
	eng2 := NewEngine(rt, caller2, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-py-defer-sleep"))
	res, _, susp2, _, _, err := eng2.Replay(ctx, wasmBytes, "run", []byte(`{}`), hist)
	if err != nil || susp2 != nil {
		t.Fatalf("the resumed run did not finish: err=%v suspended=%v result=%q", err, susp2 != nil, res)
	}
	ops := operationsCalled(caller2)
	if len(ops) != 2 || ops[0] != "after" || ops[1] != "cleanup" {
		t.Fatalf("the resumed run made calls %v, want the call after the sleep, then the cleanup once", ops)
	}
}
