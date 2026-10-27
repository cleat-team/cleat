//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// cleat#3428: durable-run-detached and durable-defer-phase were registered with
// the component linker and linked to the cbTypeDefault fallback. Neither raises:
// run_detached's WIT result is a u64, which the fallback's zero satisfies, and
// defer_phase returns nothing, so there was nothing for the fallback to get
// wrong. A Python component built and ran, and the host was never told.
//
// So these read what the HOST recorded, through a real component, the way
// python_child_poll_e2e_test.go does for the child calls.

// pythonEntryEngine compiles examples/<file>:<entry> and returns the bytes with
// an engine whose service caller and options the test supplies.
func pythonEntryEngine(t *testing.T, file, entry string, caller ServiceCaller, opts ...EngineOption) ([]byte, *Engine) {
	t.Helper()

	pythonWasm := newPythonWasmTestHelper(t)
	if !pythonWasm.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python, so this job "+
				"installs componentize-py/wasm-tools and treats Python as first-class: %s",
				requireToolchainEnv, pythonWasm.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + pythonWasm.missingTools())
	}

	wasmBytes, err := os.ReadFile(pythonWasm.compileWorkflow(t, file, entry))
	if err != nil {
		t.Fatalf("read compiled WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v (python routes here; there is no fallback to test)", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	return wasmBytes, NewEngine(rt, caller,
		append([]EngineOption{WithBackends(WasmtimeLanguages, wt)}, opts...)...)
}

// failIfThePythonGuestRaised fails on the {"error": ...} result an uncaught
// Python exception becomes (see cleat#3433), so a raise reads as what it is.
func failIfThePythonGuestRaised(t *testing.T, result string) {
	t.Helper()
	var doc struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(unwrapPythonResult(t, result)), &doc) == nil && doc.Error != "" {
		t.Fatalf("the guest raised instead of returning: %s", doc.Error)
	}
}

func TestPythonGuestRunDetachedStartsAWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}

	type start struct{ name, input string }
	var starts []start
	store := &mockChildStore{
		startChildFn: func(_ context.Context, _ /*parent*/, name, input string, _ int, _ string, _ int) (string, error) {
			starts = append(starts, start{name, input})
			return "detached-run-1", nil
		},
	}
	wasmBytes, eng := pythonEntryEngine(t, "detach_and_defer_phase_workflow.py", "detach_probe",
		&mockCaller{}, WithChildWorkflowStore(store))

	result, hist, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "detach_probe", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected suspension: %v", suspended.Reason)
	}
	failIfThePythonGuestRaised(t, result)

	if len(starts) != 1 {
		t.Fatalf("the child store saw %d start(s) %v, want exactly 1: a Python run_detached on the "+
			"component path must reach RunDetached, not the fallback that returns 0 and starts nothing",
			len(starts), starts)
	}
	if starts[0].name != "child-wf" || starts[0].input != `{"k": 1}` {
		t.Errorf("the store started %+v, want child-wf with {\"k\": 1} -- the arguments did not cross the boundary intact", starts[0])
	}

	var detached *EventRecord
	for i := range hist {
		if hist[i].EventType == EventTypeRunDetached {
			detached = &hist[i]
		}
	}
	if detached == nil {
		t.Fatalf("no run_detached event in the history (%d events): the start is not durable, so a replay would start it again", len(hist))
	}
	if detached.DetachedName != "child-wf" || detached.DetachedRunID != "detached-run-1" {
		t.Errorf("run_detached event = name %q run %q, want child-wf / detached-run-1", detached.DetachedName, detached.DetachedRunID)
	}
}

func TestPythonGuestDeferPhaseMarksTheDefersCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	wasmBytes, eng := pythonEntryEngine(t, "detach_and_defer_phase_workflow.py", "defer_phase_probe", &mockCaller{})

	result, hist, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "defer_phase_probe", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected suspension: %v", suspended.Reason)
	}
	failIfThePythonGuestRaised(t, result)

	inPhase := map[string]bool{}
	for _, rec := range hist {
		if rec.EventType == EventTypeCall && rec.Service != "" {
			inPhase[rec.Service] = rec.InDeferPhase
		}
	}
	work, haveWork := inPhase["work"]
	cleanup, haveCleanup := inPhase["cleanup"]
	if !haveWork || !haveCleanup {
		t.Fatalf("history is missing a call (work=%v cleanup=%v): %+v", haveWork, haveCleanup, hist)
	}
	if work {
		t.Error("the workflow body's call is marked in_defer_phase; the mark must only cover the defer drain")
	}
	if !cleanup {
		t.Error("the defer body's call is NOT marked in_defer_phase: the guest told the host it was draining " +
			"its defer table (cleat#1237) and the component linker dropped it, so the host classifies a " +
			"retry-exhausted run by its cleanup call (cleat#1155)")
	}
}
