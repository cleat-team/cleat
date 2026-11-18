package typescript

// cleat#3922: the soft kill, in the four directions that matter beyond "the defers run".
//
//  1. the DEFAULT configuration (no --wasm-instruction-limit; --wasm-instance-timeout is the kill)
//     soft-kills too, because the epoch arm is the one an ordinary operator has;
//  2. a workflow that spends most of its time in a HOST CALL is not killed early -- the arm reads
//     the guest-execution budget the fence uses, not wall clock;
//  3. a small budget keeps its margin proportional (a tenth of it), so a workflow with a short
//     instruction limit or a short deadline is not stopped at its first poll;
//  4. the residue: a guest whose work never returns to the QuickJS bytecode loop is still reached by
//     the hard trap, and its defers still do not run. That is measured, not assumed, and it is the
//     row DIVERGENCES.md records.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// slowCaller answers every call, sleeping for `delay` on `slowService`.
type slowCaller struct {
	mu      sync.Mutex
	seen    []string
	delay   time.Duration
	service string
}

func (c *slowCaller) Call(_ context.Context, service, operation, _ string) (string, error) {
	c.mu.Lock()
	c.seen = append(c.seen, service+"/"+operation)
	c.mu.Unlock()
	if service == c.service {
		time.Sleep(c.delay)
	}
	return `{}`, nil
}

func (c *slowCaller) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func buildEntry(t *testing.T, src, entry string) []byte {
	t.Helper()
	root := needToolchain(t)
	res := build(t, root, src, entry)
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	b, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var errStopped = errors.New("stopped")

// TestTypeScriptSoftKillsWithASubSecondBudget: no instruction limit at all, and an 800ms instance
// timeout so the test is quick. The kill is the epoch fence, and it must still run the defers.
//
// THE SUB-SECOND BUDGET IS THE POINT, not an accident of test speed: at 800ms the budget/10 cap
// puts the reserve at 80ms, which is under the two-tick floor the epoch's rounding needs, and
// before that floor existed this test lost its defers 7 times in 20 (`-count=20` at cleat#3922's
// review). It is the configuration that exercises the floor.
func TestTypeScriptSoftKillsWithASubSecondBudget(t *testing.T) {
	wasmBytes := buildEntry(t, "tests/typescript/testdata/workflows/defer_then_spin.ts", "defer_then_spin")

	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })
	var logs strings.Builder
	logsAt := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// No instruction limit: this is the default configuration, and the only kill available is time.
	wt, err := engine.NewWasmtimeBackend(ctx,
		engine.WithWasmtimeExecutionTimeout(800*time.Millisecond),
		engine.WithWasmtimeLogger(logsAt))
	if err != nil {
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	caller := &recordingCaller{}
	e := engine.NewEngine(rt, caller,
		engine.WithWorkflowID("wf-ts-default-softkill"),
		engine.WithDefName("defer_then_spin"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
		engine.WithLogger(logsAt),
	)

	_, _, _, _, _, err = e.Execute(ctx, wasmBytes, "defer_then_spin", []byte(`{}`))
	if err == nil {
		t.Fatal("the spinning workflow returned without an error, so nothing was killed and this test measured nothing")
	}
	// The user must still see the kill they always saw: a time-limit error, not a bare trap.
	if !strings.Contains(err.Error(), "execution time limit exceeded") {
		t.Fatalf("the workflow was stopped but not reported as the time-limit kill it was: %v", err)
	}
	if got := caller.snapshot(); len(got) != 1 || got[0] != "inventory/cleanup" {
		t.Fatalf("the default configuration soft-killed the guest and its defer recorded %v, want one inventory/cleanup.\n\n"+
			"Logs:\n%s", got, logs.String())
	}
}

// TestTypeScriptSoftKillDoesNotChargeTheGuestForHostTime: a 1s host call under a 1.5s guest budget
// must leave the guest its full 1.5s afterwards. If the arm read wall clock instead of the
// guest-execution budget, it would fire on the first poll after the call.
func TestTypeScriptSoftKillDoesNotChargeTheGuestForHostTime(t *testing.T) {
	wasmBytes := buildEntry(t, "tests/typescript/testdata/workflows/slow_then_spin.ts", "slow_then_spin")

	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })
	const guestBudget = 1500 * time.Millisecond
	const hostDelay = 1000 * time.Millisecond
	wt, err := engine.NewWasmtimeBackend(ctx, engine.WithWasmtimeExecutionTimeout(guestBudget))
	if err != nil {
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	caller := &slowCaller{delay: hostDelay, service: "payments"}
	e := engine.NewEngine(rt, caller,
		engine.WithWorkflowID("wf-ts-softkill-hosttime"),
		engine.WithDefName("slow_then_spin"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	)

	start := time.Now()
	_, _, _, _, _, err = e.Execute(ctx, wasmBytes, "slow_then_spin", []byte(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the spinning workflow returned without an error, so nothing was killed")
	}
	if got := caller.snapshot(); len(got) != 1 || got[0] != "payments/very-slow" {
		t.Fatalf("the service call was not made (%v), so the host time was never part of this run", got)
	}
	// The guest gets its whole budget AFTER the call. Allow a healthy margin for a loaded runner,
	// but the failure this catches is a factor of two, not a few tens of milliseconds.
	if elapsed < hostDelay+guestBudget/2 {
		t.Fatalf("the workflow was killed after %s, with a %s host call and a %s guest budget: the soft kill "+
			"charged the guest for the host call, which --wasm-instance-timeout does not.", elapsed, hostDelay, guestBudget)
	}
}

// TestTypeScriptASmallBudgetKeepsAProportionalMargin: with a tiny instruction limit the reserve is a
// tenth of it, so an ordinary workflow still finishes.
func TestTypeScriptASmallBudgetKeepsAProportionalMargin(t *testing.T) {
	wasmBytes := buildEntry(t, "tests/typescript/testdata/workflows/answer.ts", "answer")

	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })
	wt, err := engine.NewWasmtimeBackend(ctx, engine.WithWasmtimeInstructionLimit(10_000_000))
	if err != nil {
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	caller := &recordingCaller{}
	e := engine.NewEngine(rt, caller,
		engine.WithWorkflowID("wf-ts-small-limit"),
		engine.WithDefName("answer"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	)
	_, _, susp, _, _, err := e.Execute(ctx, wasmBytes, "answer", []byte(`{}`))
	if err != nil || susp != nil {
		t.Fatalf("an ordinary workflow failed under a 10M instruction limit (err=%v susp=%v): the interrupt reserve "+
			"was not capped to the budget and the soft kill fired on its first poll.", err, susp)
	}
}

// TestTypeScriptAFuelArmKillDrainsThroughADeepUnwind: the fuel reserve has to cover the unwind's
// OWN fuel cost, not just the guest's ordinary running. A 500-frame stack descends before the loop,
// so the exception unwinds through 2000 frames; the measured ordinary unwind spends 40k fuel, this
// one spends more, and the reserve must still be enough that the hard trap does not fire mid-unwind.
func TestTypeScriptAFuelArmKillDrainsThroughADeepUnwind(t *testing.T) {
	wasmBytes := buildEntry(t, "tests/typescript/testdata/workflows/deep_then_spin.ts", "deep_then_spin")

	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })
	wt, err := engine.NewWasmtimeBackend(ctx, engine.WithWasmtimeInstructionLimit(200_000_000))
	if err != nil {
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	caller := &recordingCaller{}
	e := engine.NewEngine(rt, caller,
		engine.WithWorkflowID("wf-ts-deep-unwind"),
		engine.WithDefName("deep_then_spin"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	)
	_, _, _, _, _, err = e.Execute(ctx, wasmBytes, "deep_then_spin", []byte(`{}`))
	if err == nil {
		t.Fatal("the spinning workflow returned without an error, so nothing was killed")
	}
	if !strings.Contains(err.Error(), "instruction limit") {
		t.Fatalf("the workflow was not reported as an instruction-limit kill: %v", err)
	}
	if got := caller.snapshot(); len(got) != 1 || got[0] != "inventory/cleanup" {
		t.Fatalf("a kill that unwound a 500-frame stack recorded %v, want one inventory/cleanup: the fuel reserve "+
			"did not cover the unwind, so the hard trap fired mid-unwind.", got)
	}
}

// TestTypeScriptAKillTheGuestCannotObserveStillLosesItsDefers is the RESIDUE (see DIVERGENCES.md).
//
// A loop whose work is dominated by native string allocation never returns to the QuickJS bytecode
// loop, so the handler is never called and the soft kill cannot happen; the hard trap does, the
// instance is left borrowed, and the defer does not run. Measured, dated, and asserted so it cannot
// silently change in either direction.
func TestTypeScriptAKillTheGuestCannotObserveStillLosesItsDefers(t *testing.T) {
	wasmBytes := buildEntry(t, "tests/typescript/testdata/workflows/defer_then_string_spin.ts", "defer_then_string_spin")

	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })
	var logs strings.Builder
	logsAt := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	wt, err := engine.NewWasmtimeBackend(ctx,
		engine.WithWasmtimeInstructionLimit(200_000_000),
		engine.WithWasmtimeLogger(logsAt))
	if err != nil {
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	caller := &recordingCaller{}
	e := engine.NewEngine(rt, caller,
		engine.WithWorkflowID("wf-ts-residue"),
		engine.WithDefName("defer_then_string_spin"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
		engine.WithLogger(logsAt),
	)
	_, _, _, _, _, err = e.Execute(ctx, wasmBytes, "defer_then_string_spin", []byte(`{}`))
	if err == nil {
		t.Fatal("the string loop returned without an error, so nothing was killed and this test measured nothing")
	}
	if !strings.Contains(err.Error(), "instruction limit") {
		t.Fatalf("the workflow was not reported as an instruction-limit kill: %v", err)
	}
	if got := caller.snapshot(); len(got) != 0 {
		t.Fatalf("the residue changed: a string-dominated guest recorded %v, so the soft kill DID reach it.\n\n"+
			"That is an improvement, not a breakage -- update DIVERGENCES.md and this test.", got)
	}
	if !strings.Contains(logs.String(), "a killed workflow's defers could not be run") {
		t.Fatalf("the drain neither ran the defers nor reported failing to. Logs:\n%s", logs.String())
	}
}

// TestTypeScriptConcurrentSoftKillsHaveNoSharedState runs two soft kills AT THE SAME TIME. The
// interrupt bookkeeping is package-level, so two executions writing it is a data race unless it is
// atomic; `go test -race` on this test is the check. (It was a data race in the first version:
// plain fields written by every execution, reported at wasmtime_hostfuncs.go.)
func TestTypeScriptConcurrentSoftKillsHaveNoSharedState(t *testing.T) {
	wasmBytes := buildEntry(t, "tests/typescript/testdata/workflows/defer_then_spin.ts", "defer_then_spin")

	run := func(name string) ([]string, error) {
		ctx := context.Background()
		rt, err := engine.NewRuntime(ctx, 0, 0)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rt.Close(ctx) }()
		wt, err := engine.NewWasmtimeBackend(ctx, engine.WithWasmtimeExecutionTimeout(800*time.Millisecond))
		if err != nil {
			return nil, err
		}
		caller := &recordingCaller{}
		e := engine.NewEngine(rt, caller,
			engine.WithWorkflowID("wf-ts-concurrent-"+name),
			engine.WithDefName("defer_then_spin"),
			engine.WithDefVersion(1),
			engine.WithBackends(engine.WasmtimeLanguages, wt),
		)
		_, _, _, _, _, err = e.Execute(ctx, wasmBytes, "defer_then_spin", []byte(`{}`))
		return caller.snapshot(), err
	}

	type outcome struct {
		calls []string
		err   error
	}
	results := make(chan outcome, 2)
	for _, name := range []string{"a", "b"} {
		go func(name string) {
			calls, err := run(name)
			results <- outcome{calls: calls, err: err}
		}(name)
	}
	for n := 0; n < 2; n++ {
		r := <-results
		if r.err == nil {
			t.Fatal("a concurrent spinning workflow returned without an error, so nothing was killed")
		}
		if len(r.calls) != 1 || r.calls[0] != "inventory/cleanup" {
			t.Fatalf("a concurrent soft kill recorded %v, want one inventory/cleanup", r.calls)
		}
	}
}
