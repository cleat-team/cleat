package main

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2942, the deferred half. deregisterExecution is what executeWorkflow's
// deferred teardown calls, and it must remove only THIS execution's own
// entries: the same same-worker re-claim that makes the fence cancel the wrong
// generation also puts a successor's registrations under the same wf.ID, so an
// id-only Delete would tear a live successor down when the predecessor merely
// finished.
//
// The map handles are driven directly because executeWorkflow itself needs a
// real WASM guest and a real database (see defer_phase_execute_test.go). The
// function under test is the production one, not a copy of its body.
func TestDeregisteringAnExecutionLeavesASuccessorsRegistrationAlone(t *testing.T) {
	w := newTestWorker(&mockStore{})

	// The predecessor: claimed at generation 1, now tearing down.
	pred := &engine.WorkflowInstance{ID: "run-1", DefName: "test-def", Generation: 1}
	_, predCancel := context.WithCancel(context.Background())
	predReg := &execRegistration{generation: 1, cancel: predCancel}
	w.inflight.Store("run-1", pred)
	w.fencedRuns.Store("run-1", int64(1))
	w.execCancel.Store("run-1", predReg)
	defer predCancel()

	// The successor: a same-worker re-claim Stored over the same id, exactly as
	// the store double in a_reclaim_races_the_heartbeat_test.go does.
	succ := &engine.WorkflowInstance{ID: "run-1", DefName: "test-def", Generation: 2}
	succCtx, succCancel := context.WithCancel(context.Background())
	succReg := &execRegistration{generation: 2, cancel: succCancel}
	w.inflight.Store("run-1", succ)
	w.fencedRuns.Store("run-1", int64(2))
	w.execCancel.Store("run-1", succReg)
	defer succCancel()

	w.deregisterExecution(pred, predReg)

	if got, _ := w.inflight.Load("run-1"); got != succ {
		t.Errorf("inflight[\"run-1\"] = %v, want the successor's instance -- the predecessor's teardown removed it", got)
	}
	if got, _ := w.execCancel.Load("run-1"); got != succReg {
		t.Errorf("execCancel[\"run-1\"] = %v, want the successor's registration", got)
	}
	if !w.runIsFenced("run-1") {
		t.Error("runIsFenced(\"run-1\") = false -- the predecessor's teardown cleared the successor's marker")
	}
	if succCtx.Err() != nil {
		t.Error("the successor's context was cancelled by the predecessor's teardown")
	}
}

// The control, without which the test above is satisfied by a deregistration
// that removes nothing at all -- the "condition that never decides anything"
// shape this fix's first half also had to avoid.
func TestDeregisteringAnExecutionClearsItsOwnEntries(t *testing.T) {
	w := newTestWorker(&mockStore{})

	wf := &engine.WorkflowInstance{ID: "run-1", DefName: "test-def", Generation: 3}
	_, cancel := context.WithCancel(context.Background())
	reg := &execRegistration{generation: 3, cancel: cancel}
	defer cancel()
	w.inflight.Store("run-1", wf)
	w.fencedRuns.Store("run-1", int64(3))
	w.execCancel.Store("run-1", reg)

	w.deregisterExecution(wf, reg)

	if _, ok := w.inflight.Load("run-1"); ok {
		t.Error("inflight[\"run-1\"] still present after its own deregistration")
	}
	if _, ok := w.execCancel.Load("run-1"); ok {
		t.Error("execCancel[\"run-1\"] still present after its own deregistration")
	}
	if w.runIsFenced("run-1") {
		t.Error("runIsFenced(\"run-1\") = true after its own deregistration")
	}
}
