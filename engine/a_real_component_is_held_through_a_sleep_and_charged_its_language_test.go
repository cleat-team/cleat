//go:build cgo

package engine

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/monitoring/prometheus"
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
	// callTimes is when each outbound call arrived, in order.
	callTimes []time.Time
	// maxCharged is the most the set was charged at any sample while Execute ran. It is sampled, so it is a floor on what the set held: it can miss a hold that
	// was shorter than the sampling interval, which is why the caller asserts it only for a hold it knows lasts far longer than that.
	maxCharged uint64
	// wokeAt is when the held run's claim check ran, which is the wake (zero if it never woke); endedAt is when Execute returned.
	wokeAt, endedAt time.Time
	// claimChecks is how many times the held run asked whether it still owned its claim (once per wake).
	claimChecks int32
	// refusals is what the resident set recorded as REASONS it did not hold a wait, scraped from its own metric after the execution ("none recorded" when it refused nothing, and
	// when residency was off). A row that fails with "was not held" has only two possible stories, a hold that was refused and one that never reached the set, and this is the
	// field that tells them apart: without it a once-in-a-while failure under load has to be reproduced to be explained.
	refusals string
}

// residentRefusals reads the refusal counter back through the same /metrics handler an operator scrapes, so it is the production counter and not a second record kept for the
// test. It returns "kind=sleep reason=memory x1" for each recorded (kind, reason), sorted, or "none recorded".
func residentRefusals(t *testing.T, m *prometheus.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		return fmt.Sprintf("UNMEASURED: /metrics answered %d", rec.Code)
	}
	var found []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "cleat_resident_refusals_total{") {
			continue // comment lines mention the name too; only a sample line starts with it
		}
		labels := line[strings.Index(line, "{")+1 : strings.LastIndex(line, "}")]
		count := strings.TrimSpace(line[strings.LastIndex(line, "}")+1:])
		var kind, reason string
		for _, kv := range strings.Split(labels, ",") {
			switch {
			case strings.HasPrefix(kv, "kind="):
				kind = strings.Trim(strings.TrimPrefix(kv, "kind="), `"`)
			case strings.HasPrefix(kv, "reason="):
				reason = strings.Trim(strings.TrimPrefix(kv, "reason="), `"`)
			}
		}
		found = append(found, fmt.Sprintf("kind=%s reason=%s x%s", kind, reason, count))
	}
	if len(found) == 0 {
		return "none recorded"
	}
	sort.Strings(found)
	return strings.Join(found, "; ")
}

func executeAComponent(t *testing.T, wasmBytes []byte, backendLangs []string, input string, resident bool, component string) heldRun {
	t.Helper()
	out, err := tryExecuteAComponent(t, wasmBytes, backendLangs, input, resident, component, nil)
	if err != nil {
		t.Fatalf("execute (resident=%v): %v", resident, err)
	}
	return out
}

// tryExecuteAComponent is executeAComponent for a caller that expects the execution to be able to fail (the fence rows), with backend options and a bound on the
// whole workflow so that a fence that does NOT fire fails the row in seconds instead of hanging it.
func tryExecuteAComponent(t *testing.T, wasmBytes []byte, backendLangs []string, input string, resident bool, component string, backendOpts []WasmtimeOption, engineOpts ...EngineOption) (heldRun, error) {
	t.Helper()
	return tryExecuteAComponentUnder(t, wasmBytes, backendLangs, input, resident, component, backendOpts, nil, engineOpts...)
}

// tryExecuteAComponentUnder is tryExecuteAComponent with a hook to reshape the residency policy the run is admitted under (nil keeps the default), which is how a row makes the set
// refuse on purpose and checks that the refusal is the thing reported.
func tryExecuteAComponentUnder(t *testing.T, wasmBytes []byte, backendLangs []string, input string, resident bool, component string, backendOpts []WasmtimeOption, shape func(*ResidencyConfig), engineOpts ...EngineOption) (heldRun, error) {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, backendOpts...)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	caller := &timedCaller{mockCaller: &mockCaller{}}
	var checks atomic.Int32
	var wokeAt atomic.Int64
	var set *ResidentSet
	var refusalMetrics *prometheus.Metrics
	opts := []EngineOption{WithBackends(backendLangs, wt), WithWorkflowID("wf-held-component-" + component)}
	if resident {
		cfg := DefaultResidencyConfig()
		cfg.MaxRuns = 2
		refusalMetrics, err = prometheus.New(prometheus.Config{})
		if err != nil {
			t.Fatalf("prometheus.New: %v", err)
		}
		cfg.Metrics = refusalMetrics
		if shape != nil {
			shape(&cfg)
		}
		set, err = NewResidentSet(cfg)
		if err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		opts = append(opts, WithResidency(set.ForRun(ResidentRunInfo{
			WorkflowID: "wf-held-component-" + component,
			Language:   wasm.DetectLanguage(wasmBytes),
			ClaimCheck: func(context.Context) error {
				wokeAt.CompareAndSwap(0, time.Now().UnixNano())
				checks.Add(1)
				return nil
			},
		})))
	}
	eng := NewEngine(rt, caller, append(opts, engineOpts...)...)

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
	endedAt := time.Now()
	close(stop)
	wg.Wait()
	out := heldRun{result: res, suspended: susp != nil, ops: operationsCalled(caller.mockCaller), maxCharged: maxCharged.Load(), claimChecks: checks.Load(), endedAt: endedAt, refusals: "none recorded"}
	if refusalMetrics != nil {
		out.refusals = residentRefusals(t, refusalMetrics)
	}
	if w := wokeAt.Load(); w != 0 {
		out.wokeAt = time.Unix(0, w)
	}
	out.callTimes = append([]time.Time(nil), caller.times...)
	if len(caller.times) >= 2 {
		out.gap = caller.times[1].Sub(caller.times[0])
	}
	if err != nil {
		return out, err
	}
	if set != nil {
		if got := chargedBytes(set); got != 0 {
			t.Errorf("the set is still charged %d bytes after the execution ended: a charge was not given back", got)
		}
	}
	return out, nil
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
		t.Fatalf("%s was not held through its sleep: suspended=%v ops=%v result=%q, want one execution that made both calls. The resident set recorded these refusals: %s", what, on.suspended, on.ops, on.result, on.refusals)
	}
	if on.claimChecks != 1 {
		t.Errorf("%s: the claim was checked %d times, want once (a wake)", what, on.claimChecks)
	}
}

func TestPythonComponentIsHeldThroughADurableSleepAndChargedAsPython(t *testing.T) {
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

	want := defaultResidentInstanceBytes["python"]
	if shortOn.maxCharged != want {
		t.Errorf("while held the set was charged at most %d bytes, want the Python instance charge = %d (the compiled component is deliberately not charged)", shortOn.maxCharged, want)
	}

	// THE SCALING CONTROL. A guest that ignores its input also completes and exits cleanly, so the sleep length is asked for twice and the gap between the guest's
	// two calls must follow it (the SDK binds input keys to parameters by name, and an unnamed key silently takes the default). A hold cannot be shorter than its
	// sleep, so the lower bounds cannot flake under load, and the 1200 ms ceiling on the SHORT request is three times its sleep, there only to catch a guest whose
	// default is the long one.
	longOn := executeAComponent(t, b, WasmtimeLanguages, `{"sleep_ms":1500}`, true, "held-sleep@1")
	if longOn.suspended || len(longOn.ops) != 2 {
		t.Fatalf("the 1500 ms run was not held in place: suspended=%v ops=%v. The resident set recorded these refusals: %s", longOn.suspended, longOn.ops, longOn.refusals)
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

// A RUNAWAY LOOP AFTER A HOLD IS STOPPED BY THE FENCE, ON THE COMPONENT PATH (the case residency creates: a held run waking into a runaway loop). The time is measured
// from the claim check, which runs at the wake, to the moment Execute returned with the fence's own error, and it is asserted from BELOW (a stopped guest can only be
// stopped late, so a lower bound cannot flake under load): the guest must have been given about a whole fence after the wake and not stopped at once, which is what
// a fence that kept charging during the 400 ms hold would do.
//
// WHAT THIS DOES NOT SHOW, AND IT WAS TESTED: that the fence is RESTARTED at the wake, as opposed to re-armed. Neutering the restart itself (restartFence's
// budget.restart) leaves this row GREEN: 1.936 s after the wake against a fence of 2 s, as with it (1.959 s). A restart differs from a re-arm only for a guest that had
// ALREADY used part of its fence when the hold began, and this one has not (componentize-py pre-initialises the interpreter at build time, so there is almost no guest
// time before the first host call; a hypothesis about this guest, not a measurement). A row that does tell them apart is a FOLLOW-UP, not yet in the tree: a component twin of the core
// spin_sleep_spin timing row (a guest that computes before AND after the hold, sized to the machine, nightly, Python only).
//
// A WARNING ABOUT THE OBVIOUS MUTANT, which this comment used to describe wrongly: removing only the component builder's onLimitsRestart registration (component_cgo.go) is
// an EQUIVALENT mutant. Execute registers the same restart (backend_wasmtime.go) BEFORE it dispatches to the component path, and the closure reads b.budget when it runs,
// by which time it is the component's budget, so the component's own registration is redundant and removing it changes nothing. Mutate restartFence, or remove both.

func TestPythonRunawayLoopAfterAHoldIsStoppedByTheFenceAWholeFenceAfterTheWake(t *testing.T) {
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}
	b, err := os.ReadFile(h.compileWorkflow(t, "held_sleep_then_spin_workflow.py", "held_sleep_then_spin"))
	if err != nil {
		t.Fatalf("read the compiled guest: %v", err)
	}
	const fence = 2 * time.Second
	fenceOpt := []WasmtimeOption{WithWasmtimeExecutionTimeout(fence)}
	bound := WithDefaultWorkflowTimeout(60 * time.Second) // a backstop only: a missing fence fails in a minute instead of hanging
	const input = `{"sleep_ms":400}`

	// CONTROL: residency off. The run suspends at the sleep after one call and never reaches the loop, so the loop only runs after a hold.
	off, err := tryExecuteAComponent(t, b, WasmtimeLanguages, input, false, "spin-after-hold@1", fenceOpt, bound)
	if err != nil || !off.suspended || len(off.ops) != 1 {
		t.Fatalf("PRECONDITION FAILED: with residency off the run did not simply suspend after one call (err=%v suspended=%v ops=%v)", err, off.suspended, off.ops)
	}

	on, err := tryExecuteAComponent(t, b, WasmtimeLanguages, input, true, "spin-after-hold@1", fenceOpt, bound)
	if err == nil {
		t.Fatalf("a runaway loop after a hold ran to completion under a %s fence", fence)
	}
	want := fmt.Sprintf("execution time limit exceeded (%s wall-clock budget", fence)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("the run was stopped, but not by the fence (want the message to contain %q): %v", want, err)
	}
	if on.claimChecks != 1 || on.wokeAt.IsZero() || len(on.ops) != 1 {
		t.Fatalf("PRECONDITION FAILED: the run was not held and woken (claim checks %d, ops %v), so the post-wake time below is not a wake-to-kill time", on.claimChecks, on.ops)
	}
	postWake := on.endedAt.Sub(on.wokeAt)
	t.Logf("held 400 ms; the loop after the wake was stopped %s after the claim check, against a fence of %s", postWake.Round(time.Millisecond), fence)
	if postWake < fence-150*time.Millisecond {
		t.Errorf("the loop after the wake was stopped only %s after it, against a fence of %s: the guest was not given about a whole fence after the wake: the hold was charged to it, or the fence was cut short", postWake, fence)
	}
	if postWake > fence+10*time.Second {
		t.Errorf("the loop after the wake ran %s against a fence of %s", postWake, fence)
	}
}
