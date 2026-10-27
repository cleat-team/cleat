//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// cleat#3429: awaiting a child that has not finished.
//
// The host records the wait, sets its suspension, and answers with a word the
// component world (`-> string`) could only flatten to 65536 zero bytes. The
// guest ran on: past a pending await_child it made the next side effect, and it
// finished and drained its own defers, so a cleanup fired while the run was only
// suspended. These drive real Python components and read what the HOST saw.
//
// The fix stops the guest by trapping, except where the instance is still
// needed to run defers: a host defer segment, and the guest's own drain. Each of
// those exceptions has a test, because a plain trap lost the cleanup (measured:
// ops=[] where the old code gave ops=[release]).

const pendingChildFixture = "pending_child_await_workflow.py"

var pendingChildInput = json.RawMessage(`{"pending":"run-pending"}`)

// pendingChildEngine returns an engine whose child store reports every child as
// still running.
func pendingChildEngine(t *testing.T, entry string, caller ServiceCaller, opts ...EngineOption) ([]byte, *Engine) {
	t.Helper()
	return pythonEntryEngine(t, pendingChildFixture, entry, caller,
		append([]EngineOption{WithChildWorkflowStore(&mockChildStore{})}, opts...)...)
}

func TestPythonGuestPendingChildAwaitStopsTheGuestBeforeItsNextSideEffect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	for _, entry := range []string{"await_child_pending", "await_all_children_pending"} {
		t.Run(entry, func(t *testing.T) {
			caller := &mockCaller{}
			wasmBytes, eng := pendingChildEngine(t, entry, caller)
			result, hist, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, entry, pendingChildInput)
			if err != nil {
				t.Fatalf("Execute: %v -- the run FAILED where it should have suspended", err)
			}
			if suspended == nil {
				t.Fatalf("the run completed with %q; with a child unfinished it should have suspended", result)
			}
			if ops := operationsCalled(caller); len(ops) != 0 {
				t.Errorf("the guest ran on past the pending await and made %v before the run suspended "+
					"(measured before the fix: await_child made side_effect)", ops)
			}
			if len(hist) != 1 {
				t.Errorf("history has %d events, want only the await: %+v", len(hist), hist)
			}
		})
	}
}

// Before the fix the guest ran on, finished, and drained its own defers, so the
// cleanup ran while the workflow was only suspended.
func TestPythonGuestPendingChildAwaitDoesNotFireTheCleanupOfASuspendedRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	const entry = "await_child_with_defer"
	caller := &mockCaller{}
	wasmBytes, eng := pendingChildEngine(t, entry, caller)
	_, _, suspended, _, _, err := eng.Execute(context.Background(), wasmBytes, entry, pendingChildInput)
	if err != nil || suspended == nil {
		t.Fatalf("err=%v suspended=%v, want a clean suspension", err, suspended != nil)
	}
	if ops := operationsCalled(caller); len(ops) != 0 {
		t.Errorf("a run that is only suspended made %v; before the fix this was [side_effect release]", ops)
	}
}

// A workflow terminated while it waits on a child is replayed as a defer
// segment to run its defers. The plain trap lost them.
func TestPythonGuestPendingChildAwaitInADeferSegmentStillRunsTheCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	const entry = "await_child_with_defer"
	wasmBytes, eng1 := pendingChildEngine(t, entry, &mockCaller{})
	_, hist, _, _, _, err := eng1.Execute(context.Background(), wasmBytes, entry, pendingChildInput)
	if err != nil {
		t.Fatalf("original execution: %v", err)
	}

	caller := &mockCaller{}
	_, eng2 := pendingChildEngine(t, entry, caller, WithDeferPhase())
	if _, _, _, _, _, err := eng2.Replay(context.Background(), wasmBytes, entry, pendingChildInput, hist); err != nil {
		t.Fatalf("defer segment: %v", err)
	}
	ops := operationsCalled(caller)
	if len(ops) != 1 || ops[0] != "release" {
		t.Errorf("the defer segment made %v, want exactly [release]: the cleanup must run and the body's "+
			"side effect must not. A plain trap here gave [] and lost the cleanup", ops)
	}
}

// The hole the coordinator named: a defer body that itself waits on a pending
// child while the GUEST drains its own defers after a failure. A trap there would
// end every defer queued after it. The host knows the guest is draining through
// durable-defer-phase (cleat#3428).
func TestPythonGuestsOwnDeferDrainSurvivesAPendingChildAwait(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	const entry = "defer_drain_survives_a_pending_await"
	caller := &mockCaller{}
	wasmBytes, eng := pendingChildEngine(t, entry, caller)
	if _, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes, entry, pendingChildInput); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []string{"body", "B", "A"}
	got := operationsCalled(caller)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("ops = %v, want %v: both defers must run, B (which waited on a pending child) and then A "+
			"queued before it", got, want)
	}
}

// What a defer body that awaits children that have not finished SEES. It is
// the empty answer, which Python reads three different ways, so it is pinned
// here rather than left to follow from the SDK.
func TestPythonGuestsDeferBodyAwaitingPendingChildrenSeesEmptyAnswers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	const entry = "defer_body_awaits_children"
	caller := &mockCaller{}
	wasmBytes, eng := pendingChildEngine(t, entry, caller)
	if _, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes, entry, pendingChildInput); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var saw *CallRecord
	for i := range caller.calls {
		if caller.calls[i].Op == "saw" {
			saw = &caller.calls[i]
		}
	}
	if saw == nil {
		t.Fatalf("the defer body never reported (ops=%v): a pending await inside it stopped the guest", operationsCalled(caller))
	}
	var got struct {
		Child    string `json:"child"`
		All      int    `json:"all"`
		AnyRaise bool   `json:"any_raised"`
	}
	if err := json.Unmarshal([]byte(saw.Request), &got); err != nil {
		t.Fatalf("report %q is not the fixture's document: %v", saw.Request, err)
	}
	if got.Child != "" {
		t.Errorf("await_child returned %q in a defer body, want the empty string", got.Child)
	}
	if got.All != 0 {
		t.Errorf("await_all_children returned %d result(s) in a defer body, want 0 (Python reads \"\" as [])", got.All)
	}
	if !got.AnyRaise {
		t.Error("await_any_child did not raise in a defer body (Python raises on an empty answer)")
	}
}
