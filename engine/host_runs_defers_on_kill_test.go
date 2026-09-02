//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The host runs the defers of a workflow it killed. IMPROVEMENT-PLAN 3.35
// phase 4.
//
// 3.70 made the guest run its own defers when its entry point finishes. These
// are the workflows where it never finishes -- stopped by the fence, by the
// instruction limit, or by an unrecoverable runtime failure -- whose cleanup
// therefore never happened at all: the lock stayed held, the charge stayed
// uncompensated.
//
// #544 and #548 measured that the instance survives all three and its defer
// closures with it. #550 added the export. This is the host making the call.
//
// These tests go through Engine.Execute rather than the backend, because the
// composed behaviour is what a worker gets: the workflow still fails, with the
// error it failed for, AND its cleanup ran.

func killEngine(t *testing.T, opts ...WasmtimeOption) (*Engine, *mockCaller, []byte) {
	t.Helper()
	ctx := context.Background()
	wasmBytes := fenceReentryWasm(t)

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })

	wt, err := NewWasmtimeBackend(ctx, opts...)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-killed"))
	return eng, caller, wasmBytes
}

// ranTheDefer reports whether the fixture's cleanup reached the host.
func ranTheDefer(c *mockCaller) bool {
	for _, rec := range c.calls {
		if rec.Op == "the_fenced_workflows_defer" {
			return true
		}
	}
	return false
}

// TestTheHostRunsDefersOfAFencedWorkflow is the headline case: a runaway
// workflow stopped by the execution fence still releases what it acquired.
func TestTheHostRunsDefersOfAFencedWorkflow(t *testing.T) {
	eng, caller, wasmBytes := killEngine(t, WithWasmtimeExecutionTimeout(2*time.Second))

	_, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"spin_forever", json.RawMessage(`{}`))

	// The workflow still failed, and for the reason it failed.
	if err == nil {
		t.Fatal("a fenced workflow was reported as succeeding")
	}
	if !strings.Contains(err.Error(), "execution time limit exceeded") {
		t.Fatalf("failed, but not on the fence: %v", err)
	}

	if !ranTheDefer(caller) {
		t.Fatalf("the fence killed the workflow and its defer never ran (calls: %v).\n\n"+
			"This is 3.35 phase 4: the cleanup a defer exists for is exactly what a "+
			"killed workflow does not get, and a held lock outlives the workflow "+
			"that took it.", operationsCalled(caller))
	}
}

// TestTheHostRunsDefersOfAnOOMKilledWorkflow is the arm whose stop mechanism is
// not a trap at all -- the Go runtime calls proc_exit(2) from its fatal path
// (3.71) -- so it reaches the defer pass by a different route from the fence.
//
// It is also the arm where the memory ceiling is deliberately not raised: the
// export takes no arguments and so needs no scratch space, which is what makes
// it safe to run cleanup for a guest that just exhausted its memory.
func TestTheHostRunsDefersOfAnOOMKilledWorkflow(t *testing.T) {
	eng, caller, wasmBytes := killEngine(t, WithWasmtimeMemoryLimits(64<<20, -1, -1))

	_, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"allocate_forever", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("an OOM-killed workflow was reported as succeeding; §3.71 regressed")
	}
	if !ranTheDefer(caller) {
		t.Fatalf("the workflow was killed for exhausting memory and its defer never "+
			"ran (calls: %v)", operationsCalled(caller))
	}
}

// TestTheHostRunsDefersOfAFuelKilledWorkflow covers the arm that needs a fuel
// grant as well as a wall-clock one. Without SetFuel the runner traps
// instantly and nothing is cleaned up, which was measured before the budget
// refresh was written.
func TestTheHostRunsDefersOfAFuelKilledWorkflow(t *testing.T) {
	eng, caller, wasmBytes := killEngine(t,
		WithWasmtimeInstructionLimit(200_000_000))

	_, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"spin_forever", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a fuel-exhausted workflow was reported as succeeding")
	}
	if !strings.Contains(err.Error(), "instruction limit exceeded") {
		t.Fatalf("failed, but not on the instruction limit: %v", err)
	}
	if !ranTheDefer(caller) {
		t.Fatalf("the instruction limit killed the workflow and its defer never ran "+
			"(calls: %v).\n\n"+
			"A wall-clock grant alone is not enough here: the store is out of fuel, "+
			"so the runner traps on its first instruction unless SetFuel is called "+
			"too.", operationsCalled(caller))
	}
}

// TestTheHostDoesNotRunDefersTwiceForAWorkflowThatFinished is the control: a
// workflow that fails NORMALLY has already run its own defers in the guest
// (3.70), and its cleanup must not run a second time. Releasing a lock twice
// or refunding a charge twice is worse than the bug phase 4 fixes.
//
// Read carefully WHICH layer holds this up, because it is not the obvious one
// and I asserted the wrong one first. Two independent layers prevent double
// cleanup:
//
//  1. the host only runs its pass on the KILL paths, not on the
//     GuestReturnedError path a normally-failed workflow takes; and
//  2. _cleatRunDeferred is idempotent -- the guest drained the table already,
//     so a second call runs nothing (#550).
//
// Measured 2026-09-02: adding a runGuestDefersAfterKill call to the
// GuestReturnedError branch leaves this test PASSING. Layer 2 absorbs it. So
// this test is held up by guest-side idempotence, and the host-side gating it
// appears to check is defence in depth rather than the load-bearing part.
//
// That is worth knowing before trusting it: deleting the gating would not fail
// anything here. It is kept because relying on idempotence alone means every
// future defer body has to stay safely re-runnable, and because running a
// cleanup pass on a workflow that already cleaned up is wasted work on the
// worker's hot path.
func TestTheHostDoesNotRunDefersTwiceForAWorkflowThatFinished(t *testing.T) {
	eng, caller, _ := killEngine(t)
	deferWasm, readErr := os.ReadFile(buildFixtureWasm(t, "deferfunc"))
	if readErr != nil {
		t.Fatalf("reading the deferfunc fixture: %v", readErr)
	}

	_, _, _, _, _, err := eng.Execute(context.Background(), deferWasm,
		"defer_on_error", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("defer_on_error was expected to fail; the fixture changed")
	}

	// Exactly one, not two. The guest ran it; the host must not.
	n := 0
	for _, rec := range caller.calls {
		if rec.Op == "on_error" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the cleanup ran %d times, want exactly 1 (calls: %v).\n\n"+
			"A workflow that returns an error has already run its own defers in "+
			"the guest. The host's kill-path pass must not fire for it -- it is "+
			"gated on the guest having been KILLED, not on the workflow having "+
			"failed.", n, operationsCalled(caller))
	}
}
