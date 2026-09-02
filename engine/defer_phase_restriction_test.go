package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// IMPROVEMENT-PLAN §3.35 phase 4: what a defer body may not do.
//
// Both restrictions were measured on 2026-09-02 before either existed, and
// both produced the same shape of failure -- a workflow that reported SUCCESS
// while writing a durable record that could not be honoured. That is §3.70's
// defect (the host records a defer nothing can run) arrived at by a different
// road, so it is worth pinning rather than documenting.
//
// The assertions are on the HOST's durable record, not on the guest's return
// value. A guest-side check that refuses without also keeping the event out of
// the history would leave the defect exactly where it was.

// deferEventCount counts durable defer registrations in a history.
func deferEventCount(hist []EventRecord) int {
	n := 0
	for _, r := range hist {
		if r.EventType == EventTypeDefer {
			n++
		}
	}
	return n
}

func asDeferPhaseEngine(t *testing.T) (*Engine, *mockCaller, []byte) {
	t.Helper()
	wasmBytes, err := os.ReadFile(buildAssemblyScriptWasm(t))
	if err != nil {
		t.Fatalf("read AS WASM: %v", err)
	}
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	caller := &mockCaller{}
	return NewEngine(rt, caller, WithWorkflowID("wf-defer-phase")), caller, wasmBytes
}

// TestADeferBodyCannotRegisterAnotherDefer.
//
// Measured before the fix: the host recorded TWO defer events -- `defer-0
// "outer"` and `defer-3 "registered from inside a defer body"` -- and the
// inner body never ran, because `runDeferred` drains the table before the
// first body starts, so the new entry landed in a table nobody walks again.
// The workflow returned `{"ok":true}` with a pending defer in its history that
// nothing anywhere could execute.
func TestADeferBodyCannotRegisterAnotherDefer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}
	eng, caller, wasmBytes := asDeferPhaseEngine(t)

	_, hist, _, deferrals, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_registers_defer", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("defer_registers_defer: %v", err)
	}

	// Control: the outer defer must actually have run, or everything below is
	// vacuous -- an SDK that registered nothing would satisfy every assertion.
	ops := operationsCalled(caller)
	if !containsOp(ops, "outer_defer_ran") {
		t.Fatalf("the outer defer body never ran (calls: %v), so this test "+
			"proves nothing about what a defer body may do", ops)
	}

	if !containsOp(ops, "inner_defer_refused") {
		t.Errorf("the inner deferFunc did not report an error (calls: %v).\n\n"+
			"A defer body registering another defer must be refused, not "+
			"silently dropped: silently dropped is what it already was.", ops)
	}

	// The durable record is the point. One registration, not two.
	if n := deferEventCount(hist); n != 1 {
		t.Errorf("history has %d durable defer events, want 1.\n\n"+
			"The refusal has to happen BEFORE the host call. Checking after "+
			"h.defer() leaves the event behind, which is the whole defect: a "+
			"completed workflow whose history carries a defer nothing can run.", n)
	}
	if len(deferrals) != 1 {
		t.Errorf("host recorded %d deferrals %v, want 1", len(deferrals), deferrals)
	}
}

// TestADeferBodyCannotContinueAsNew.
//
// Measured before the fix: a `continue_as_new` event was recorded at step 3
// with `newInput={"round":2}` AND the wrapper went on to report the workflow's
// already-decided result. One history, two contradictory terminal facts. The
// worker stores `done` because a result is present, so the continuation is
// silently dropped -- a whole workflow run that never happens, with nothing
// anywhere saying so.
func TestADeferBodyCannotContinueAsNew(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AssemblyScript WASM integration test in short mode")
	}
	eng, caller, wasmBytes := asDeferPhaseEngine(t)

	_, hist, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_continues_as_new", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("defer_continues_as_new: %v", err)
	}

	ops := operationsCalled(caller)
	if !containsOp(ops, "defer_ran") {
		t.Fatalf("the defer body never ran (calls: %v), so this test proves "+
			"nothing", ops)
	}
	if !containsOp(ops, "continue_as_new_refused") {
		t.Errorf("continueAsNew from a defer body did not report an error "+
			"(calls: %v)", ops)
	}

	for _, r := range hist {
		if r.EventType == EventTypeContinueAsNew {
			t.Fatalf("a continue_as_new event was recorded from a defer body "+
				"(step %d, newInput=%q).\n\n"+
				"The workflow's result is already decided by the time defers "+
				"run, so the worker stores 'done' and this continuation is "+
				"never taken. Refusing after the host call would leave exactly "+
				"this record behind.", r.Step, r.NewInput)
		}
	}
}

func containsOp(ops []string, want string) bool {
	for _, op := range ops {
		if op == want {
			return true
		}
	}
	return false
}

// The Go SDK arm of the same two restrictions.
//
// Go is not inferred from AssemblyScript here, and the reason is structural
// rather than cautious: the AS guard is hand-written in defer.ts, while Go's
// is emitted by codegen into every generated module (wasm/exports.go,
// wasm/adapter_metadata.go). They are different code with the same contract,
// so one passing says nothing about the other.
func TestAGoDeferBodyCannotRegisterAnotherDefer(t *testing.T) {
	wasmBytes, eng, caller := deferEngine(t, "wf-go-defer-registers")

	_, hist, _, deferrals, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_registers_defer", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("defer_registers_defer: %v", err)
	}

	ops := operationsCalled(caller)
	if !containsOp(ops, "outer_defer_ran") {
		t.Fatalf("the outer defer body never ran (calls: %v), so this test "+
			"proves nothing about what a defer body may do", ops)
	}
	if !containsOp(ops, "inner_defer_refused") {
		t.Errorf("the inner DurableDeferFunc did not return an error (calls: %v)", ops)
	}
	if containsOp(ops, "inner_defer_body") {
		t.Errorf("the inner defer body RAN (calls: %v) -- the guard let the "+
			"registration through and something executed it", ops)
	}
	if n := deferEventCount(hist); n != 1 {
		t.Errorf("history has %d durable defer events, want 1.\n\n"+
			"The guard is in the generated adapter's PreStmts, before the "+
			"cleat_defer host call. If it moved after, the event would survive "+
			"the refusal -- which is the defect, not the fix.", n)
	}
	if len(deferrals) != 1 {
		t.Errorf("host recorded %d deferrals %v, want 1", len(deferrals), deferrals)
	}
}

func TestAGoDeferBodyCannotContinueAsNew(t *testing.T) {
	wasmBytes, eng, caller := deferEngine(t, "wf-go-defer-can")

	_, hist, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_continues_as_new", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("defer_continues_as_new: %v", err)
	}

	ops := operationsCalled(caller)
	if !containsOp(ops, "defer_ran") {
		t.Fatalf("the defer body never ran (calls: %v), so this test proves nothing", ops)
	}
	if !containsOp(ops, "continue_as_new_refused") {
		t.Errorf("ContinueAsNew from a defer body did not return an error (calls: %v)", ops)
	}
	for _, r := range hist {
		if r.EventType == EventTypeContinueAsNew {
			t.Fatalf("a continue_as_new event was recorded from a defer body "+
				"(step %d, newInput=%q)", r.Step, r.NewInput)
		}
	}
}
