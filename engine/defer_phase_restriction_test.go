package engine

import (
	"context"
	"encoding/json"
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

// The flag cleat#1155 added has to survive the whole path: a real Go guest, the
// generated defer table, the cleat_defer_phase import, the host handler, and
// recordEvent. Every other test of it works on hand-built histories, which
// proves the RULE and says nothing about whether the signal ever arrives.
//
// This one asserts the arrival, and it asserts both halves. Only checking that
// the defer's event is flagged would pass just as well if the host flagged
// EVERYTHING from the first defer registration onward -- so the body's own call
// is checked to be unflagged in the same run. A flag that is always on carries
// no information, and is exactly what a careless implementation produces.
func TestADeferBodysCallIsRecordedAsDeferPhaseAndTheBodysIsNot(t *testing.T) {
	wasmBytes, eng, caller := deferEngine(t, "wf-go-defer-phase-flag")

	_, hist, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_registers_defer", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("defer_registers_defer: %v", err)
	}

	ops := operationsCalled(caller)
	if !containsOp(ops, "outer_defer_ran") {
		t.Fatalf("the defer body never ran (calls: %v), so this test proves nothing "+
			"about how its events were recorded", ops)
	}

	var deferPhase, bodyPhase []string
	for _, rec := range hist {
		if rec.EventType != EventTypeCall {
			continue
		}
		if rec.InDeferPhase {
			deferPhase = append(deferPhase, rec.Op)
		} else {
			bodyPhase = append(bodyPhase, rec.Op)
		}
	}

	var sawDeferCall bool
	for _, op := range deferPhase {
		if op == "outer_defer_ran" {
			sawDeferCall = true
		}
	}
	if !sawDeferCall {
		t.Errorf("the defer body's call was not recorded as defer-phase.\n"+
			"  flagged:   %v\n  unflagged: %v\n\n"+
			"The guest reports the boundary with cleat_defer_phase from its "+
			"generated defer table (wasm/exports.go). If nothing is flagged, "+
			"either the import was not emitted into the module or the handler "+
			"is not setting the session flag -- and dead-lettering is back to "+
			"being unable to tell cleanup from the workflow carrying on.",
			deferPhase, bodyPhase)
	}

	// The other half: the flag must be OFF for the body's own work.
	if len(bodyPhase) == 0 {
		t.Errorf("every recorded call was flagged as defer-phase (%v).\n\n"+
			"A flag that is never off distinguishes nothing. This is what a "+
			"set-and-never-cleared implementation looks like, and it would "+
			"dead-letter workflows on the strength of their body calls.",
			deferPhase)
	}
}
