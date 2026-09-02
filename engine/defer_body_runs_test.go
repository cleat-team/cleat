//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A registered defer body never ran.
//
// Two separate reasons, and fixing either alone changes nothing:
//
//  1. DurableDeferFunc was not wired at all. It is not in the wasm package's
//     hostFunctions table, so codegen never set the HostCallsOptions field, so
//     the method returned "the HostCalls runtime was not initialized" -- an
//     error whose text says the workflow called it from the wrong place. It
//     did not. The runtime never had it.
//
//  2. Nothing could have run the body even if it had been stored. The host
//     invokes defers as an export named "cleat_defer_<id>" on a FRESH
//     instance. A closure is in the memory of the instance that registered
//     it, and the ID is minted by the host at runtime so no export can be
//     named after it at compile time. Both halves fail. IMPROVEMENT-PLAN 3.70
//     measured the result: the lookup returned err=<nil> and the worker logged
//     "defer completed".
//
// The guest now runs its own defers, in the instance that has the closures, at
// the moment the entry point finishes. These tests use a real compiled Go SDK
// guest through the wasmtime backend, because both defects were in generated
// code and neither is visible to a hand-written WAT module.

// deferEngine builds the fixture and an engine wired to wasmtime, the backend
// a worker uses.
func deferEngine(t *testing.T, workflowID string) ([]byte, *Engine, *mockCaller) {
	t.Helper()
	ctx := context.Background()

	wasmPath := buildFixtureWasm(t, "deferfunc")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

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
		WithWorkflowID(workflowID))
	return wasmBytes, eng, caller
}

// operationsCalled returns the operation of each recorded call. The fixture
// puts the identifying name there -- "body", "first", "second" -- so the
// sequence is both the evidence a defer ran and the evidence of its position.
func operationsCalled(c *mockCaller) []string {
	out := make([]string, 0, len(c.calls))
	for _, rec := range c.calls {
		out = append(out, rec.Op)
	}
	return out
}

// TestDeferBodiesRunInLIFOOrder is the regression test.
//
// It asserts the full sequence rather than mere presence. "The defers ran" is
// satisfied by running them in registration order, which would be wrong in the
// way that matters: a defer releases what the defer before it acquired, so
// FIFO unwinds the workflow inside-out.
func TestDeferBodiesRunInLIFOOrder(t *testing.T) {
	wasmBytes, eng, caller := deferEngine(t, "wf-defer-order")

	_, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_order", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("defer_order: %v", err)
	}

	got := operationsCalled(caller)
	want := []string{"body", "second", "first"}
	if len(got) != len(want) {
		t.Fatalf("recorded %d calls %v, want %d %v.\n\n"+
			"Two of these come from defer bodies. If only \"body\" is present the "+
			"defers did not run at all, which is the state IMPROVEMENT-PLAN 3.70 "+
			"describes.", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d is %q, want %q (full sequence %v, want %v).\n\n"+
				"Order is not cosmetic: defers unwind, so the last one registered "+
				"must run first.", i, got[i], want[i], got, want)
		}
	}
}

// TestDeferBodyRunsWhenTheWorkflowFails is the case a defer is FOR.
//
// A defer that only runs on the happy path is close to useless -- cleanup
// exists for the run that did not finish the way it meant to. This is also the
// half most easily lost by placing the runner after the error branch, where it
// reads as correct and never executes.
func TestDeferBodyRunsWhenTheWorkflowFails(t *testing.T) {
	wasmBytes, eng, caller := deferEngine(t, "wf-defer-on-error")

	_, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_on_error", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("defer_on_error was expected to fail; if it now succeeds the " +
			"fixture changed and this test no longer covers the error path")
	}
	if !strings.Contains(err.Error(), "the workflow failed") {
		t.Fatalf("failed for a different reason than the fixture's own error, so "+
			"the defer may not have been reached at all: %v", err)
	}

	if got := operationsCalled(caller); len(got) != 1 || got[0] != "on_error" {
		t.Fatalf("recorded %v, want exactly [on_error].\n\n"+
			"The entry point returned an error after registering cleanup. The "+
			"cleanup still has to run.", got)
	}
}

// TestDefersDoNotRunOnSuspension is the control, and it is the half most
// likely to be wrong.
//
// "Run the defers when the entry point returns" is easy to implement as "run
// them whenever the entry point stops running", which fires every cleanup at
// the first sleep -- releasing locks and refunding payments in the middle of a
// workflow that has not finished and is about to continue. The failure is
// silent: the workflow still completes, it just cleaned up too early.
//
// The two halves are one test on purpose. Not running the defers on suspension
// is only correct if the segment that DOES finish runs them, and an
// implementation that never runs them at all satisfies the first half alone.
func TestDefersDoNotRunOnSuspension(t *testing.T) {
	wasmBytes, eng, caller := deferEngine(t, "wf-defer-suspend")
	ctx := context.Background()

	_, history, suspended, _, _, err := eng.Execute(ctx, wasmBytes,
		"defer_survives_suspension", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if suspended == nil {
		t.Fatal("segment 1 did not suspend at the sleep, so this test is not " +
			"exercising suspension at all. If sleep semantics changed, adjust the " +
			"fixture rather than deleting the assertion.")
	}
	if got := operationsCalled(caller); len(got) != 0 {
		t.Fatalf("the suspended segment ran %v.\n\n"+
			"The workflow has not exited -- it is asleep and will continue. Running "+
			"its defers here fires every cleanup mid-workflow.", got)
	}

	// Segment 2, past the sleep deadline. The entry point replays, which
	// re-registers the same defer, and this time it completes.
	caller2 := &mockCaller{}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer wt.Close(ctx)

	resumeAt := suspended.SuspendUntil.UnixMilli() + 1
	eng2 := NewEngine(rt, caller2,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-defer-suspend"),
		WithClock(func() int64 { return resumeAt }))

	_, _, suspended2, _, _, err := eng2.Replay(ctx, wasmBytes,
		"defer_survives_suspension", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("segment 2: %v", err)
	}
	if suspended2 != nil {
		t.Fatalf("segment 2 suspended again until %v; the sleep has elapsed, so "+
			"the workflow never reaches the point where its defers run",
			suspended2.SuspendUntil)
	}

	got := operationsCalled(caller2)
	want := []string{"body", "after_sleep"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("segment 2 recorded %v, want %v.\n\n"+
			"The defer registered before the suspension has to run in the segment "+
			"that finishes -- after the body, because it is a defer.", got, want)
	}
}

// TestDurableDeferFuncReturnsTheHostsID checks the registration contract.
//
// The ID is not decorative. It is the key the body is stored under, and it is
// the same ID the host records in the workflow's deferrals map -- so a body
// keyed by anything else would run, but could never be correlated with what
// the host thinks it registered.
func TestDurableDeferFuncReturnsTheHostsID(t *testing.T) {
	wasmBytes, eng, _ := deferEngine(t, "wf-defer-id")

	result, history, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_registration", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("defer_registration: %v", err)
	}

	var got struct {
		DeferID string `json:"defer_id"`
	}
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("result %q is not the object the fixture returns: %v", result, err)
	}
	if got.DeferID == "" {
		t.Fatal("DurableDeferFunc returned an empty ID. Before this change it " +
			"returned an error instead -- \"the HostCalls runtime was not " +
			"initialized\" -- because codegen never set the field.")
	}

	// The host's own record has to agree, or the two sides are keying the same
	// defer differently.
	var hostID string
	for _, rec := range history {
		if rec.EventType == EventTypeDefer {
			hostID = rec.DeferID
		}
	}
	if hostID == "" {
		t.Fatalf("the host recorded no defer event, so the guest's ID %q came "+
			"from somewhere else", got.DeferID)
	}
	if hostID != got.DeferID {
		t.Errorf("the guest keyed the body under %q but the host registered %q",
			got.DeferID, hostID)
	}
}
