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

// cleat#4162, found by review of the first residency PR and measured there on real wasmtime: with the same 150ms ceiling and a 300ms sleep, residency
// OFF suspends in 2-3ms and residency ON held the run to 303ms and then FAILED it with "execution timed out". Two causes, and a unit test of the wake
// source sees neither: the wasmtime paths hand DurableSleep context.Background(), so the wait never saw the ceiling or a cancel; and admission never asked
// whether the wait falls due before the execution's own deadline.
//
// So this runs the real thing: a real Go guest on the real backend through Engine.Execute, which is where the ceiling is applied. The matrix is the
// point, because each row is only meaningful beside the others:
//
//	ceiling  residency  expected
//	150ms    off        suspends          (what a run always did)
//	150ms    on         suspends          (the defect: it failed with a timeout)
//	workflow 150ms, on  suspends          (--max-workflow-duration is a second context deadline; same defect)
//	5s       on         completes         (the known positive: the hold works at all, or the rows above prove nothing)
//	5s       off        suspends          (the control that says residency changed the outcome)
//	5s       on, cancel ends promptly     (a terminate or a cancel must not wait for the timer)

type residentRunOutcome int

const (
	residentRunCompleted residentRunOutcome = iota + 1
	residentRunSuspended
	residentRunTimedOut
	residentRunOther
)

func (o residentRunOutcome) String() string {
	return map[residentRunOutcome]string{residentRunCompleted: "completed", residentRunSuspended: "suspended", residentRunTimedOut: "timed out", residentRunOther: "failed another way"}[o]
}

func runResidentSleep(t *testing.T, rt *Runtime, wt *wasmtimeBackend, wasmBytes []byte, ceiling time.Duration, workflowTimeout time.Duration, resident bool, cancelAfter time.Duration) (residentRunOutcome, time.Duration, int) {
	t.Helper()
	return runResidentEntry(t, rt, wt, wasmBytes, "sleep_then_call", 0, ceiling, workflowTimeout, resident, cancelAfter)
}

func runResidentEntry(t *testing.T, rt *Runtime, wt *wasmtimeBackend, wasmBytes []byte, entry string, workDelay time.Duration, ceiling time.Duration, workflowTimeout time.Duration, resident bool, cancelAfter time.Duration) (residentRunOutcome, time.Duration, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newReplayMetrics(t)
	opts := []EngineOption{WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-resident-limits"), WithWasmWallClockCeiling(ceiling)}
	if workflowTimeout > 0 {
		opts = append(opts, WithDefaultWorkflowTimeout(workflowTimeout))
	}
	if resident {
		cfg := DefaultResidencyConfig()
		cfg.MaxRuns = 2
		cfg.Metrics = m
		set, err := NewResidentSet(cfg)
		if err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		opts = append(opts, WithResidency(set.ForRun(ResidentRunInfo{WorkflowID: "wf-resident-limits", Language: "go"})))
	}
	eng := NewEngine(rt, &hostWaitCaller{delay: workDelay}, opts...)
	eng.Metrics = m
	if cancelAfter > 0 {
		time.AfterFunc(cancelAfter, cancel)
	}
	t0 := time.Now()
	res, _, suspended, _, _, execErr := eng.Execute(ctx, wasmBytes, entry, json.RawMessage(`{}`))
	elapsed := time.Since(t0)
	holds := residentCounter(t, m, "cleat_resident_parks_total")

	switch {
	case execErr == nil && suspended == nil && strings.Contains(res, "ok"):
		return residentRunCompleted, elapsed, holds
	case execErr == nil && suspended != nil:
		return residentRunSuspended, elapsed, holds
	case execErr != nil && strings.Contains(execErr.Error(), "timed out"):
		return residentRunTimedOut, elapsed, holds
	}
	t.Logf("unclassified outcome: res=%q suspended=%v err=%v", res, suspended != nil, execErr)
	return residentRunOther, elapsed, holds
}

func TestAHeldRunMeetsItsOwnCeilingAndCancelOnRealWasmtime(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "residentsleep"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	// Warm up: the first run on a backend compiles the module, which takes longer than the short ceiling below. The ceiling must be what ends a run,
	// not a cold start, or every short-ceiling row measures the compiler. (The first version of this test did exactly that and read "timed out" for all.)
	if got, elapsed, _ := runResidentSleep(t, rt, wt, wasmBytes, 30*time.Second, 0, false, 0); got != residentRunSuspended {
		t.Fatalf("PRECONDITION FAILED: the warm-up run %s in %s, want it suspended at the sleep", got, elapsed)
	}
	rows := []struct {
		name        string
		ceiling     time.Duration
		workflowMax time.Duration // --max-workflow-duration
		resident    bool
		cancelAfter time.Duration
		want        residentRunOutcome
		wantHeld    bool // a hold was recorded
		maxElapsed  time.Duration
	}{
		{"short ceiling, residency off", 150 * time.Millisecond, 0, false, 0, residentRunSuspended, false, 0},
		{"short ceiling, residency on", 150 * time.Millisecond, 0, true, 0, residentRunSuspended, false, 0},
		{"short workflow duration, residency on", 5 * time.Second, 150 * time.Millisecond, true, 0, residentRunSuspended, false, 0},
		{"long ceiling, residency on: the hold works", 5 * time.Second, 0, true, 0, residentRunCompleted, true, 0},
		{"long ceiling, residency off", 5 * time.Second, 0, false, 0, residentRunSuspended, false, 0},
		{"long ceiling, residency on, cancelled while held", 5 * time.Second, 0, true, 100 * time.Millisecond, residentRunOther, true, 250 * time.Millisecond},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, elapsed, holds := runResidentSleep(t, rt, wt, wasmBytes, r.ceiling, r.workflowMax, r.resident, r.cancelAfter)
			t.Logf("%s: %s in %s, %d hold(s) recorded", r.name, got, elapsed.Round(time.Millisecond), holds)
			if r.cancelAfter > 0 {
				// A cancel ends the run by whatever route the engine reports it; what must hold is that it ENDS at the cancel and not at the timer.
				if got == residentRunCompleted {
					t.Errorf("a run cancelled at %s completed: the hold ran to its timer and the cancel was not seen", r.cancelAfter)
				}
				if elapsed > r.maxElapsed {
					t.Errorf("a run cancelled at %s took %s to end: the hold did not end at the cancel", r.cancelAfter, elapsed)
				}
			} else if got != r.want {
				t.Errorf("%s, want %s", got, r.want)
			}
			if r.wantHeld && holds == 0 {
				t.Errorf("PRECONDITION FAILED: no hold was recorded, so this row did not exercise residency")
			}
			if !r.wantHeld && holds != 0 {
				t.Errorf("%d hold(s) were recorded where none should be: a run too close to its limit must be refused, not held", holds)
			}
		})
	}
}

// KNOWN LIMIT, pinned so that the change that removes it has to flip this test (cleat#4162; the coordinator's rule is that nothing is turned on until it does).
//
// After a held wait the rest of that execution has what is left of the ceiling: ceiling - elapsed - held. With residency OFF the sleep suspends and the work
// after it runs in a FRESH execution with a fresh ceiling. So poll-then-act (do something, wait, then make a slow call) can fail with a timeout with
// residency on where it finishes with it off. Admission cannot size a margin for work it cannot see.
//
// Work 300ms, sleep 300ms, work 300ms is 900ms of one execution against an 800ms ceiling. The sleep IS admitted (it is due 300ms from a point with about
// 500ms left, which is outside the 100ms margin), and the run then times out. The name says what it is, and the assertion below is today's behaviour: PR 2 (held
// time is not charged to the ceiling) makes the "on" row complete, and is expected to change it to residentRunCompleted.
func TestKnownLimitAHeldWaitLeavesTheRestOfTheExecutionTheCeilingMinusTheHeldTime(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "residentsleep"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	if got, elapsed, _ := runResidentEntry(t, rt, wt, wasmBytes, "work_sleep_work", 0, 30*time.Second, 0, false, 0); got != residentRunSuspended {
		t.Fatalf("PRECONDITION FAILED: the warm-up run %s in %s, want it suspended at the sleep", got, elapsed)
	}
	const ceiling = 800 * time.Millisecond
	const work = 300 * time.Millisecond

	off, offElapsed, offHolds := runResidentEntry(t, rt, wt, wasmBytes, "work_sleep_work", work, ceiling, 0, false, 0)
	t.Logf("residency off: %s in %s, %d hold(s)", off, offElapsed.Round(time.Millisecond), offHolds)
	if off != residentRunSuspended {
		t.Errorf("residency off: %s, want suspended at the sleep (the work after it belongs to a fresh execution)", off)
	}

	on, onElapsed, onHolds := runResidentEntry(t, rt, wt, wasmBytes, "work_sleep_work", work, ceiling, 0, true, 0)
	t.Logf("residency on: %s in %s, %d hold(s)", on, onElapsed.Round(time.Millisecond), onHolds)
	if onHolds == 0 {
		t.Fatalf("PRECONDITION FAILED: the sleep was not held, so this row did not exercise the limit")
	}
	if on != residentRunTimedOut {
		t.Errorf("residency on: %s, want the KNOWN LIMIT (timed out). If this is now %q, the limit has been removed: flip this test and the docs that name it", on, residentRunCompleted)
	}
}
