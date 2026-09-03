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

// The instance timeout bounds GUEST EXECUTION, and used not to.
// IMPROVEMENT-PLAN 3.90.
//
// --wasm-instance-timeout (default 30s, cmd/cleat-worker/config.go:117) is
// enforced by wasmtime epoch interruption. The mechanism is a free-running
// ticker: startEpochTicker (backend_wasmtime.go:168) increments the engine
// epoch every 50ms forever, and configureStore sets a per-invocation deadline
// of timeout/50ms ticks. Nothing pauses or extends that deadline around a host
// function, and there is no SetEpochDeadline call anywhere near one -- all
// three call sites are store setup. So the epoch advances at the same rate
// whether the guest is running or parked inside cleat_call waiting on a
// service, and a slow service consumes the guest's runaway budget.
//
// Why that is worth pinning rather than shrugging at: the flag it comes from is
// described in terms of runaway GUEST code ("bounds even a WASM module stuck in
// a tight loop that never calls back into the host"), the separate
// --wasm-instruction-limit fence really does measure only guest work (fuel
// decrements on guest instructions), and the error an operator sees is
// "execution time limit exceeded", which points at a runaway workflow. A
// workflow that makes three 12s service calls and almost no computation of its
// own trips a 30s fence, and the message sends whoever debugs it to the wrong
// half of the system.
//
// It also bounds the retry design: an in-host retry loop -- the one behind
// cleat_call_retry, which keeps the worker for the duration instead of
// suspending -- spends its whole backoff inside a host call, so it is spending
// the guest's budget. Anything longer than the remaining budget must suspend
// instead, or the deadline has to be extended across the wait.

// hostWaitCaller blocks for delay on the "work" service and answers everything
// else instantly, so a test can put a known amount of host wait into a run
// without changing what the guest does.
type hostWaitCaller struct{ delay time.Duration }

func (c *hostWaitCaller) Call(_ context.Context, service, _, _ string) (string, error) {
	if service == "work" {
		time.Sleep(c.delay)
	}
	return `{"ok":true}`, nil
}

// runWithHostWait executes the deferfunc fixture's defer_order entry point --
// which makes exactly one "work" call -- with the EPOCH fence set to budget and
// no context deadline at all.
//
// Isolating the epoch fence is the point, and getting this wrong is what the
// first version of this test did. --wasm-instance-timeout is applied TWICE:
// once as the epoch deadline (configureStore) and once as a context deadline
// (executor.go:163, `context.WithTimeout(execCtx, e.wasmInstanceTimeout)`).
// They bound the same wall clock by different means, so an end-to-end test
// through engine.WithWASMInstanceTimeout cannot say which one fired -- and with
// host wait excluded from the epoch, the context deadline fires instead and the
// run still fails, with a different message ("execution timed out" rather than
// "execution time limit exceeded").
//
// So the backend gets WithWasmtimeExecutionTimeout and the engine gets no
// instance timeout. What this measures is exactly the epoch fence.
func runWithHostWait(t *testing.T, delay, budget time.Duration) (string, error) {
	t.Helper()
	return runFixtureUnderEpochBudget(t, "deferfunc", "defer_order", delay, budget)
}

// runSpinningGuest executes a guest that loops forever without returning to the
// host, under the same isolated epoch budget.
//
// testdata/fencereentry's spin_forever is the repo's canonical runaway: it
// registers a defer and then never leaves the loop, which is the case
// --wasm-instance-timeout exists for and the case epoch interruption is
// documented to catch ("bounds even a WASM module stuck in a tight loop that
// never calls back into the host").
func runSpinningGuest(t *testing.T, budget time.Duration) error {
	t.Helper()
	_, err := runFixtureUnderEpochBudget(t, "fencereentry", "spin_forever", 0, budget)
	return err
}

func runFixtureUnderEpochBudget(t *testing.T, fixture, entryPoint string, delay, budget time.Duration) (string, error) {
	t.Helper()
	ctx := context.Background()

	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, fixture))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(budget))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	// No WithWASMInstanceTimeout: that would also install a context deadline,
	// and this test is about the epoch fence alone.
	eng := NewEngine(rt, &hostWaitCaller{delay: delay},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-epoch-budget"))

	res, _, _, _, _, err := eng.Execute(ctx, wasmBytes, entryPoint, json.RawMessage(`{}`))
	return res, err
}

// TestTheInstanceTimeoutExcludesHostWait is the fix, and its control is the
// half that keeps the fix honest.
//
// Both runs use the SAME budget and the SAME guest. The only difference is
// where the time goes: one spends it waiting on the host, the other spends it
// executing. The first must now complete and the second must still trap, and
// neither assertion means anything without the other -- a change that simply
// disabled the fence would pass the first alone.
//
// Before engine/wasmtime_hostbudget.go, measured 2026-09-03 on this tree:
//
//	| delay | budget | outcome   |
//	|-------|--------|-----------|
//	| 0     | 1s     | trap      |
//	| 0     | 2s     | completes |
//	| 4s    | 5s     | trap      |  <- the defect
//	| 6s    | 8s     | completes |
//	| 9s    | 8s     | trap      |
//
// The 4s/5s row is the one this test now inverts. Note the 0/1s row too: it is
// why the control varies where the time is spent rather than how much budget
// there is. An earlier version of this test controlled with "no delay, same
// small budget, must complete" and that FAILED -- the fixture cannot finish in
// 1s whatever the caller does, so the trap happened either way and the
// experiment measured nothing.
func TestTheInstanceTimeoutExcludesHostWait(t *testing.T) {
	const budget = 3 * time.Second

	// Host wait far exceeding the whole budget. The guest's own cost is under
	// 2s, so with host wait excluded this has room; with it charged, as before,
	// this is the case that trapped.
	res, err := runWithHostWait(t, 8*time.Second, budget)
	if err != nil {
		t.Fatalf("an 8s host call under a %v budget failed: %v\n\n"+
			"Host wait is not supposed to be charged to the guest. Either the "+
			"bracket in engine/wasmtime_hostbudget.go is not covering "+
			"cleat_call, or something re-set the epoch deadline after it. "+
			"IMPROVEMENT-PLAN 3.90.", budget, err)
	}
	if res == "" {
		t.Fatalf("the run completed but returned no result")
	}

	// The control: same budget, no host wait, a guest that simply does not
	// stop. The fence must still fire -- this is the runaway case the flag
	// exists for, and it is the assertion that a "fix" which merely widened or
	// disabled the deadline would fail.
	if err := runSpinningGuest(t, budget); err == nil {
		t.Fatalf("a guest spinning with no host call completed under a %v "+
			"budget.\n\n"+
			"The fence is gone. Excluding host wait must not stop the deadline "+
			"firing on a guest that never calls back into the host -- that "+
			"guest never enters a bracket, so its deadline should never be "+
			"re-armed. Without this half, the assertion above is satisfied by "+
			"simply removing the fence.", budget)
	} else if !strings.Contains(err.Error(), "execution time limit exceeded") {
		t.Fatalf("the spinning guest failed for the wrong reason: %v\n\n"+
			"Expected the instance-timeout fence. Some other failure means "+
			"this control is no longer measuring the fence at all.", err)
	}
}

// runEndToEnd executes under BOTH bounds the way a worker does: the epoch fence
// via the backend, and the wall-clock ceiling via the engine.
func runEndToEnd(t *testing.T, fixture, entryPoint string, delay, instanceTimeout, ceiling time.Duration) (string, error) {
	t.Helper()
	ctx := context.Background()

	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, fixture))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(instanceTimeout))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	eng := NewEngine(rt, &hostWaitCaller{delay: delay},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-two-bounds"),
		WithWASMInstanceTimeout(instanceTimeout),
		WithWasmWallClockCeiling(ceiling))

	res, _, _, _, _, err := eng.Execute(ctx, wasmBytes, entryPoint, json.RawMessage(`{}`))
	return res, err
}

// TestTheTwoBoundsAnswerDifferentQuestions is the deliverable, and all three
// runs share one instance timeout so the only thing varying is what consumes
// the time and how much ceiling there is.
//
// Before IMPROVEMENT-PLAN 3.90 there was only one number. --wasm-instance-timeout
// was both the epoch fence and a context deadline, so "this guest is runaway"
// and "this has been going too long" had the same answer, and a workflow
// waiting on slow services was killed as though it were spinning. Case 1 is
// what that cost: it failed before this change and is the whole point of it.
func TestTheTwoBoundsAnswerDifferentQuestions(t *testing.T) {
	const instance = 3 * time.Second

	// 1. Waiting far longer than the guest is allowed to RUN, but inside the
	//    ceiling. This is the ordinary case -- a couple of slow service calls
	//    -- and it must complete.
	res, err := runEndToEnd(t, "deferfunc", "defer_order", 8*time.Second, instance, 30*time.Second)
	if err != nil {
		t.Fatalf("an 8s host call under a %v instance timeout and a 30s ceiling failed: %v\n\n"+
			"This is the case 3.90 exists to fix. If the error says \"execution "+
			"timed out\" the ceiling is not being applied and the executor is "+
			"still using wasmInstanceTimeout as its context deadline; if it "+
			"says \"execution time limit exceeded\" the epoch bracket in "+
			"engine/wasmtime_hostbudget.go is not covering cleat_call.", instance, err)
	}
	if res == "" {
		t.Fatalf("case 1 completed but returned no result")
	}

	// 2. The ceiling still bounds waiting. Same host call, ceiling below it --
	//    a worker must not be held indefinitely by an unresponsive service, and
	//    this is the assertion that the fix did not simply remove a bound.
	if _, err := runEndToEnd(t, "deferfunc", "defer_order", 8*time.Second, instance, 4*time.Second); err == nil {
		t.Fatalf("an 8s host call completed under a 4s wall-clock ceiling.\n\n" +
			"The ceiling is not enforced, so nothing bounds an invocation " +
			"blocked in a host call. That is a worse failure than the one 3.90 " +
			"set out to fix.")
	} else if !strings.Contains(err.Error(), "execution timed out") {
		t.Fatalf("the ceiling fired with the wrong error: %v\n\n"+
			"Expected the executor's context-deadline path. \"execution time "+
			"limit exceeded\" would mean the epoch fence fired instead, which "+
			"would mean host wait is being charged to the guest again.", err)
	}

	// 3. And the epoch fence still bounds RUNNING, under a ceiling far above
	//    it. A generous wall-clock ceiling must not let a runaway guest live
	//    past the instance timeout -- otherwise raising the ceiling silently
	//    raises the runaway bound too.
	if _, err := runEndToEnd(t, "fencereentry", "spin_forever", 0, instance, 60*time.Second); err == nil {
		t.Fatalf("a spinning guest completed under a %v instance timeout.", instance)
	} else if !strings.Contains(err.Error(), "execution time limit exceeded") {
		t.Fatalf("the spinning guest was stopped by the wrong bound: %v\n\n"+
			"Expected the epoch fence at %v, not the 60s ceiling. If this is "+
			"\"execution timed out\" the guest ran for a full minute before "+
			"anything stopped it, and --wasm-instance-timeout no longer bounds "+
			"runaway guests at all.", err, instance)
	}
}
