//go:build cgo

package engine

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// The guest exports a defer runner the host can name.
//
// IMPROVEMENT-PLAN 3.35 phase 4. #544 and #548 established that a Go guest
// killed by the fence, the instruction limit or an out-of-memory is still
// re-enterable, and that its outstanding defers still run -- but they
// demonstrated it by calling one of the workflow's OWN entry points, because
// codegen emits _cleatRunDeferred at the end of every export.
//
// That is fine for a measurement and wrong for production: it runs that entry
// point's body as well, so cleaning up after a dead workflow would execute a
// step of it. __cleat_run_deferred is the export that does only the cleanup.

// callDeferRunner invokes the dedicated export. It takes no arguments, unlike
// the four an entry point takes, so it does not go through callExport.
func (r *reentryRig) callDeferRunner(t *testing.T) (int64, error) {
	t.Helper()
	fn := r.instance.GetFunc(r.store, "__cleat_run_deferred")
	if fn == nil {
		t.Fatal("the guest exports no __cleat_run_deferred. Codegen emits it into " +
			"every module, so this means the emission was removed or renamed -- and " +
			"the host has no way to run a dead workflow's defers without it.")
	}
	var res any
	var callErr error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				callErr = fmt.Errorf("wasmtime panic calling __cleat_run_deferred: %v", rec)
			}
		}()
		res, callErr = fn.Call(r.store)
	}()
	if callErr != nil {
		return 0, callErr
	}
	n, _ := res.(int64)
	return n, nil
}

// TestTheDeferRunnerExportRunsDefersWithoutRunningAWorkflowStep is the point of
// having a dedicated export at all.
//
// The two assertions are a pair. That the defer ran is necessary; that the
// entry point's body did NOT is what makes this different from #544's
// borrowed-entry-point demonstration, and it is the half a careless
// implementation loses -- reusing an existing export would satisfy the first
// assertion perfectly while silently executing a step of a dead workflow.
func TestTheDeferRunnerExportRunsDefersWithoutRunningAWorkflowStep(t *testing.T) {
	rig := newReentryRig(t, fenceReentryWasm(t), 2*time.Second)

	err := rig.runStart(t, "spin_forever", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("the spinning entry point returned instead of being fenced, so " +
			"nothing below is about a killed workflow")
	}
	if limitErr := rig.backend.resourceLimitError(err, 2*time.Second); limitErr == nil {
		t.Fatalf("the guest stopped, but not on a resource limit: %v", err)
	}
	if got := operationsCalled(rig.caller); len(got) != 0 {
		t.Fatalf("the fenced entry point reached the host %v before being stopped; "+
			"it registers a defer and then spins without entering the host, so the "+
			"assertions below would be reading its leftovers", got)
	}

	rig.caller.calls = nil
	rig.store.SetEpochDeadline(600)

	ran, callErr := rig.callDeferRunner(t)
	if callErr != nil {
		t.Fatalf("calling __cleat_run_deferred on a fenced guest failed: %v", callErr)
	}

	if !rig.sawOp("the_fenced_workflows_defer") {
		t.Fatalf("the defer runner returned (ran=%d) but the dead workflow's defer "+
			"never reached the host (calls: %v).\n\n"+
			"A returned count is not evidence -- the host call is.", ran,
			operationsCalled(rig.caller))
	}

	// The half that a reused entry point would fail.
	if rig.reachedHost() {
		t.Fatalf("__cleat_run_deferred also ran an entry point body: %v.\n\n"+
			"It must run cleanup and nothing else. Executing a workflow step while "+
			"cleaning up after a workflow the host has already killed is worse than "+
			"not cleaning up at all.", operationsCalled(rig.caller))
	}

	if ran != 1 {
		t.Errorf("ran=%d, want 1 -- the fixture registers exactly one defer", ran)
	}
}

// TestTheDeferRunnerIsSafeToCallTwice covers the case the host will actually
// hit: it cannot always know whether the guest already ran its own defers.
//
// A workflow that completed normally drains the table in its entry point
// wrapper. If the host then calls the runner anyway, it must be a no-op rather
// than a second round of cleanup -- releasing a lock twice or refunding a
// payment twice is the failure this prevents.
//
// Note for anyone mutating this to check it still bites: idempotence rests on
// TWO independent mechanisms in _cleatRunDeferred -- `_cleatDeferIDs = nil` and
// `delete(_cleatDeferFuncs, ...)` -- and either one alone is sufficient.
// Removing just one leaves this test passing, which makes the other look like
// dead code it is safe to drop. Both have to go before this fails (measured
// 2026-09-02). They are kept because they answer different questions: the nil
// guards a defer that registers another defer mid-walk, the delete releases the
// closure.
func TestTheDeferRunnerIsSafeToCallTwice(t *testing.T) {
	// 2s, not the 30s default: spin_forever runs until the fence stops it, so
	// the budget IS this test's runtime.
	rig := newReentryRig(t, fenceReentryWasm(t), 2*time.Second)

	err := rig.runStart(t, "spin_forever", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("spin_forever returned; the fixture changed")
	}
	rig.caller.calls = nil
	rig.store.SetEpochDeadline(600)

	first, callErr := rig.callDeferRunner(t)
	if callErr != nil {
		t.Fatalf("first call: %v", callErr)
	}
	if first != 1 {
		t.Fatalf("first call ran %d defers, want 1", first)
	}
	if !rig.sawOp("the_fenced_workflows_defer") {
		t.Fatal("the first call did not actually run the defer, so the second " +
			"call below is not testing re-entrancy")
	}

	rig.caller.calls = nil
	second, callErr := rig.callDeferRunner(t)
	if callErr != nil {
		t.Fatalf("second call: %v", callErr)
	}
	if second != 0 {
		t.Errorf("second call ran %d defers, want 0", second)
	}
	if got := operationsCalled(rig.caller); len(got) != 0 {
		t.Fatalf("calling the defer runner twice ran the cleanup twice: %v.\n\n"+
			"The table must be drained by the first call. A defer that releases a "+
			"lock or refunds a payment must not run again because the host was "+
			"unsure whether the guest had already cleaned up.", got)
	}
}
