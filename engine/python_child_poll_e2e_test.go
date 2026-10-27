//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// cleat#3390: a Python guest's poll_child and await_any_child linked to the
// cbTypeDefault fallback, which wrote a u64 zero and called nothing. The tests
// that covered them ran against LocalHost, and the dispatcher tests in
// component_child_dispatch_test.go call the dispatchers directly, so neither
// crosses the boundary this defect lived at. This compiles a REAL component and
// runs it against a REAL engine, the way python_cron_e2e_test.go does for cron.
//
// It lives in ./engine/ for the reason that file gives: tier1-gate.yml and
// e2e-cross-language.yml already sweep ./engine/... with a TestPython pattern.

// pythonChildPollEngine compiles examples/child_poll_workflow.py:<entry> and
// returns the bytes with an engine whose child store reports run-done as
// completed and everything else as still running.
func pythonChildPollEngine(t *testing.T, entry string) ([]byte, *Engine) {
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

	wasmPath := pythonWasm.compileWorkflow(t, "child_poll_workflow.py", entry)
	wasmBytes, err := os.ReadFile(wasmPath)
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

	store := &mockChildStore{
		getChildResultFn: func(_ context.Context, runID string) (ChildOutcome, error) {
			if runID == "run-done" {
				return ChildOutcome{Completed: true, Result: `{"ok":true}`}, nil
			}
			return ChildOutcome{}, nil
		},
		// Long before the parent's clock, so PollChild's replay-safe "was it
		// terminal as of the parent's durable time" question answers yes.
		getChildCompletedAtMsFn: func(context.Context, string) (int64, bool, error) {
			return 1, true, nil
		},
	}
	return wasmBytes, NewEngine(rt, &mockCaller{},
		WithBackends(WasmtimeLanguages, wt),
		WithChildWorkflowStore(store),
		// The parent's durable clock. Without it a fresh execution is seeded
		// from the engine's cached wall clock, which is zero in a test process,
		// and PollChild answers "running" for a child that completed at any
		// positive time (completedAtMs > nowMs): the same replay-safety rule the
		// getChildCompletedAtMsFn comment below describes, seen from the other side.
		WithWorkflowStartTime(1_000_000),
	)
}

// unwrapPythonResult peels the JSON string encoding a componentized Python
// guest wraps its return value in (see parsePythonCronResult).
func unwrapPythonResult(t *testing.T, result string) string {
	t.Helper()
	raw := strings.TrimSpace(result)
	var unquoted string
	if err := json.Unmarshal([]byte(raw), &unquoted); err == nil {
		return unquoted
	}
	return raw
}

func TestPythonGuestPollsAndRacesChildrenThroughTheComponentBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	wasmBytes, eng := pythonChildPollEngine(t, "child_poll")

	result, _, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "child_poll",
		json.RawMessage(`{"done":"run-done","pending":"run-pending"}`))
	if err != nil {
		t.Fatalf("Execute: %v\na failure here is the point of the test: it means poll_child or "+
			"await_any_child did not complete from a componentized Python guest", err)
	}
	if suspended != nil {
		t.Fatalf("the guest suspended (%s) though one child had finished", suspended.Reason)
	}

	// A Python exception does not fail the run: the entry wrapper turns it into
	// a SUCCESSFUL result of the form {"error": "..."}. Without this check a
	// guest that raised decodes to a zero-valued struct below and fails four
	// field assertions that never say why (measured: ChildResult(**data) raised
	// "unexpected keyword argument 'status'" and the test reported only that
	// every field was empty).
	var asError struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(unwrapPythonResult(t, result)), &asError) == nil && asError.Error != "" {
		t.Fatalf("the guest raised instead of returning the fixture's document: %s", asError.Error)
	}
	var got struct {
		PolledResult  *string `json:"polled_result"`
		PolledError   *string `json:"polled_error"`
		RunningIsNone bool    `json:"running_is_none"`
		WinnerRunID   string  `json:"winner_run_id"`
		WinnerResult  string  `json:"winner_result"`
	}
	if err := json.Unmarshal([]byte(unwrapPythonResult(t, result)), &got); err != nil {
		t.Fatalf("workflow result %q is not the fixture's JSON document: %v", result, err)
	}

	if got.PolledResult == nil || *got.PolledResult != `{"ok":true}` {
		t.Errorf("poll_child on the finished child returned result %v, want {\"ok\":true}", got.PolledResult)
	}
	if got.PolledError != nil {
		t.Errorf("poll_child on the finished child returned error %q, want none", *got.PolledError)
	}
	if !got.RunningIsNone {
		t.Error("poll_child on the still-running child did not return None")
	}
	if got.WinnerRunID != "run-done" || got.WinnerResult != `{"ok":true}` {
		t.Errorf("await_any_child returned run_id=%q result=%q, want run-done with {\"ok\":true}",
			got.WinnerRunID, got.WinnerResult)
	}
}

// The pending case: no child has finished, the host records the wait and
// suspends the run. The WIT function is a bare string, so the guest sees an
// empty answer and raises; the suspension has to win over that error.
func TestPythonGuestAwaitingOnlyPendingChildrenSuspendsInsteadOfFailing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	wasmBytes, eng := pythonChildPollEngine(t, "child_await_pending")

	result, _, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, "child_await_pending",
		json.RawMessage(`{"pending":"run-pending"}`))
	if err != nil {
		t.Fatalf("Execute: %v -- the run FAILED where it should have suspended", err)
	}
	if suspended == nil {
		t.Fatalf("the run completed with %q; with no child finished it should have suspended", result)
	}
	if !strings.Contains(suspended.Reason, "await_any_child") {
		t.Errorf("the run suspended for %q, want an await_any_child wait", suspended.Reason)
	}
}
