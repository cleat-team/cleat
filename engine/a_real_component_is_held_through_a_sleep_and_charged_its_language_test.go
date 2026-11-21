//go:build cgo

package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/wasm"
)

// Part A of cleat#4162: a Python and a C# component are admitted for residency. The accounting is pinned without a guest in
// a_python_and_csharp_runs_are_held_and_charged_test.go; these rows run a REAL component on the worker's real backend and show three things the accounting cannot:
// that the component path holds through a durable sleep at all (a component is a different instantiation path from a core module), that what is charged is what
// DetectLanguage says the component is, and that the charge is on the set WHILE the run is held and gone after.
//
// WHAT IS NOT HERE: the memory these hold. Those figures are WS-cleat1's (cleat-internal/measurements/component-held-instances-2026-11-20), measured on arm64 with
// a harness that holds N instances; a test that asserted a number of megabytes here would be a different measurement on a different machine.

// timedCaller records when each call arrived, so a test can measure the hold between two of them.
type timedCaller struct {
	*mockCaller
	mu    sync.Mutex
	times []time.Time
}

func (c *timedCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	c.mu.Lock()
	c.times = append(c.times, time.Now())
	c.mu.Unlock()
	return c.mockCaller.Call(ctx, service, operation, requestJSON)
}

type heldRun struct {
	result    string
	suspended bool
	ops       []string
	// gap is the time from the guest's first outbound call to its second, which is the sleep between them and nothing else. The whole execution is NOT used: each
	// execution here builds its own backend, so it includes compiling the component (seconds for Python), which would swamp a 400 ms sleep and make any bound on it
	// vacuous. Zero when fewer than two calls were made.
	gap time.Duration
	// maxCharged is the most the set was charged at any sample while Execute ran. It is sampled, so it is a floor on what the set held: it can miss a hold that
	// was shorter than the sampling interval, which is why the caller asserts it only for a hold it knows lasts far longer than that.
	maxCharged uint64
	// claimChecks is how many times the held run asked whether it still owned its claim (once per wake).
	claimChecks int32
}

func executeAComponent(t *testing.T, wasmBytes []byte, backendLangs []string, input string, resident bool, component string) heldRun {
	t.Helper()
	ctx := context.Background()
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

	caller := &timedCaller{mockCaller: &mockCaller{}}
	var checks atomic.Int32
	var set *ResidentSet
	opts := []EngineOption{WithBackends(backendLangs, wt), WithWorkflowID("wf-held-component-" + component)}
	if resident {
		cfg := DefaultResidencyConfig()
		cfg.MaxRuns = 2
		set, err = NewResidentSet(cfg)
		if err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		opts = append(opts, WithResidency(set.ForRun(ResidentRunInfo{
			WorkflowID: "wf-held-component-" + component,
			Language:   wasm.DetectLanguage(wasmBytes),
			Component:  component,
			ClaimCheck: func(context.Context) error { checks.Add(1); return nil },
		})))
	}
	eng := NewEngine(rt, caller, opts...)

	var maxCharged atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	if set != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
					if c := chargedBytes(set); c > maxCharged.Load() {
						maxCharged.Store(c)
					}
				}
			}
		}()
	}
	res, _, susp, _, _, err := eng.Execute(ctx, wasmBytes, "run", []byte(input))
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("execute (resident=%v): %v", resident, err)
	}
	out := heldRun{result: res, suspended: susp != nil, ops: operationsCalled(caller.mockCaller), maxCharged: maxCharged.Load(), claimChecks: checks.Load()}
	if len(caller.times) >= 2 {
		out.gap = caller.times[1].Sub(caller.times[0])
	}
	if set != nil {
		if got := chargedBytes(set); got != 0 {
			t.Errorf("the set is still charged %d bytes after the execution ended: a charge was not given back", got)
		}
	}
	return out
}

// requireHeldInPlace asserts the two things that together say "held, not replayed": both calls ran in ONE execution, and the held run asked about its claim.
func requireHeldInPlace(t *testing.T, what string, off, on heldRun) {
	t.Helper()
	// The control: the same component and input without residency suspends after the first call. Without it, "both calls ran" is also what a guest that never
	// sleeps does.
	if !off.suspended || len(off.ops) != 1 {
		t.Fatalf("PRECONDITION FAILED (%s, residency off): suspended=%v ops=%v, want a suspension after exactly one call. Without that, a run that completes in place under residency says nothing.", what, off.suspended, off.ops)
	}
	if on.suspended || len(on.ops) != 2 {
		t.Fatalf("%s was not held through its sleep: suspended=%v ops=%v result=%q, want one execution that made both calls", what, on.suspended, on.ops, on.result)
	}
	if on.claimChecks != 1 {
		t.Errorf("%s: the claim was checked %d times, want once (a wake)", what, on.claimChecks)
	}
}

func TestCSharpComponentIsHeldThroughADurableSleepAndChargedAsCSharp(t *testing.T) {
	// The checked-in C# fixture is UNSTAMPED (DetectLanguage reads it as python), so it is stamped here the way `cleat build --target csharp` stamps a real build
	// (cmd/cleat/build_csharp.go, which refuses to write a component that does not read back as csharp). Without this the row would charge it as Python.
	b, err := os.ReadFile(filepath.Join("testdata", "csharp_defer_order", "defer_order.wasm"))
	if err != nil {
		t.Fatalf("read the C# fixture: %v", err)
	}
	b, err = wasm.WriteMetadata(b, &wasm.Metadata{Language: "csharp"})
	if err != nil {
		t.Fatalf("stamp the fixture: %v", err)
	}
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Fatalf("PRECONDITION FAILED: the stamped fixture is detected as %q, so the C# charge is not what is being tested", got)
	}
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	const input = `{"mode":"sleep"}` // the fixture's sleep is 400 ms

	// Off first: it also compiles and caches the component, so the run below is not paying for a cold compile inside its hold.
	off := executeAComponent(t, b, langs, input, false, "csharp-fixture@1")
	on := executeAComponent(t, b, langs, input, true, "csharp-fixture@1")
	requireHeldInPlace(t, "the C# component", off, on)

	want := defaultResidentInstanceBytes["csharp"] + defaultResidentFixedBytes["csharp"]
	if on.maxCharged != want {
		t.Errorf("while held the set was charged at most %d bytes, want the C# instance plus its fixed cost = %d. A figure of %d would be the Python charge: this run was charged as the wrong language.",
			on.maxCharged, want, defaultResidentInstanceBytes["python"]+defaultResidentFixedBytes["python"])
	}
	// A hold cannot be shorter than the sleep it waits out.
	if on.gap < 350*time.Millisecond {
		t.Errorf("the two calls were %s apart for a 400 ms sleep: the run did not wait", on.gap)
	}
}

func TestPythonComponentIsHeldThroughADurableSleepAndChargedAsPython(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Python WASM integration test in short mode")
	}
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}
	b, err := os.ReadFile(h.compileWorkflow(t, "held_sleep_workflow.py", "held_sleep"))
	if err != nil {
		t.Fatalf("read the compiled guest: %v", err)
	}
	if got := wasm.DetectLanguage(b); got != "python" {
		t.Fatalf("PRECONDITION FAILED: the compiled guest is detected as %q, not python", got)
	}

	short := executeAComponent(t, b, WasmtimeLanguages, `{"sleep_ms":400}`, false, "held-sleep@1") // compiles and caches the component
	shortOn := executeAComponent(t, b, WasmtimeLanguages, `{"sleep_ms":400}`, true, "held-sleep@1")
	requireHeldInPlace(t, "the Python component", short, shortOn)

	want := defaultResidentInstanceBytes["python"] + defaultResidentFixedBytes["python"]
	if shortOn.maxCharged != want {
		t.Errorf("while held the set was charged at most %d bytes, want the Python instance plus its fixed cost = %d", shortOn.maxCharged, want)
	}

	// THE SCALING CONTROL. A guest that ignores its input also completes and exits cleanly, so the sleep length is asked for twice and the gap between the guest's
	// two calls must follow it (the SDK binds input keys to parameters by name, and an unnamed key silently takes the default). A hold cannot be shorter than its
	// sleep, so the lower bounds cannot flake under load, and the 1200 ms ceiling on the SHORT request is three times its sleep, there only to catch a guest whose
	// default is the long one.
	longOn := executeAComponent(t, b, WasmtimeLanguages, `{"sleep_ms":1500}`, true, "held-sleep@1")
	if longOn.suspended || len(longOn.ops) != 2 {
		t.Fatalf("the 1500 ms run was not held in place: suspended=%v ops=%v", longOn.suspended, longOn.ops)
	}
	if shortOn.gap < 350*time.Millisecond {
		t.Errorf("a 400 ms sleep held the run for %s: it did not wait", shortOn.gap)
	}
	if shortOn.gap > 1200*time.Millisecond {
		t.Errorf("a 400 ms sleep held the run for %s: the guest slept for something other than the 400 ms it was asked for (sleep_ms binds by NAME)", shortOn.gap)
	}
	if longOn.gap < 1450*time.Millisecond {
		t.Errorf("a 1500 ms sleep held the run for only %s: the guest ignored its input (sleep_ms binds by NAME) or did not sleep", longOn.gap)
	}
}
