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

// The instance timeout is charged for time the guest spends BLOCKED IN A HOST
// CALL, not just for time it spends executing. IMPROVEMENT-PLAN 3.90.
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
// which makes exactly one "work" call -- under a given instance timeout.
func runWithHostWait(t *testing.T, delay, timeout time.Duration) (string, error) {
	t.Helper()
	ctx := context.Background()

	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "deferfunc"))
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

	eng := NewEngine(rt, &hostWaitCaller{delay: delay},
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-host-wait"),
		WithWASMInstanceTimeout(timeout))

	res, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "defer_order", json.RawMessage(`{}`))
	return res, err
}

// TestTheInstanceTimeoutIsChargedForHostWait holds both runs, deliberately.
//
// The comparison IS the assertion, so it must not be possible to run half of
// it. The two runs differ in ONE variable -- the budget -- and share the same
// 5s of host wait and the same guest, so a difference in outcome cannot be
// explained by the guest being slow or by the delay itself being fatal.
//
// That pairing is not decoration; the obvious version of this test is wrong.
// The first control here was "no delay, same small budget, must complete", and
// it FAILED: a 1s budget is too small for this fixture whatever the caller
// does, so the trap it was controlling for happens either way and the
// experiment measured nothing. Measured 2026-09-03 on this tree: with no delay
// at all, a 1s budget traps and a 2s budget completes. Hence a control that
// varies the budget rather than the delay.
//
// Measured 2026-09-03, wasmtime, darwin/arm64. Re-derive with
//
//	go test ./engine/ -run TestTheInstanceTimeoutIsChargedForHostWait -v
//
// | delay | budget | outcome  |
// |-------|--------|----------|
// | 0     | 2s     | completes|
// | 4s    | 5s     | trap     |
// | 6s    | 8s     | completes|
// | 9s    | 8s     | trap     |
//
// which is host wait consuming the budget roughly 1:1 on top of the fixture's
// own sub-2s cost.
func TestTheInstanceTimeoutIsChargedForHostWait(t *testing.T) {
	const hostWait = 5 * time.Second

	// Budget below the wait. The wait alone exceeds it, so this traps on any
	// machine -- there is no timing race here, which is the point of choosing
	// a delay larger than the budget rather than a delay near it.
	res, err := runWithHostWait(t, hostWait, 3*time.Second)
	if err == nil {
		t.Fatalf("a %v host call completed under a 3s instance budget, returning %q.\n\n"+
			"That is the BEHAVIOUR WE WANT and this test asserts the broken one. "+
			"If the epoch deadline is now paused or extended across host calls, "+
			"the fence measures guest execution rather than wall clock: delete "+
			"this half, keep the control below, and revisit IMPROVEMENT-PLAN "+
			"3.90 and the in-host retry bound it constrains.", hostWait, res)
	}
	if !strings.Contains(err.Error(), "execution time limit exceeded") {
		t.Fatalf("expected the instance-timeout fence to fire, got: %v.\n\n"+
			"Some other failure means this run is no longer measuring what the "+
			"budget is charged for.", err)
	}

	// The control, and the only variable that changed is the budget. Same
	// caller, same delay, same guest. 5s of wait plus the fixture's own sub-2s
	// cost sits far inside 30s, so this completing is what makes the trap above
	// attributable to the budget rather than to the delay being fatal in itself.
	res, err = runWithHostWait(t, hostWait, 30*time.Second)
	if err != nil {
		t.Fatalf("the same %v host call failed under a 30s budget: %v\n\n"+
			"Without this run completing, the trap above says nothing -- it "+
			"would be consistent with the delay failing the workflow for some "+
			"other reason entirely. Do not delete this half.", hostWait, err)
	}
	if res == "" {
		t.Fatalf("the run under a 30s budget returned no result")
	}
}
