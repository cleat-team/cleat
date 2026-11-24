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

// cleat#4162. A run that is HELD on the worker through a wait must meet its own limits the way a suspended one does. These run the real thing: a real Go guest
// on the real backend through Engine.Execute, which is where the ceiling and the workflow deadline are applied. A unit test of the wake source cannot see
// the two defects this found, because the wasmtime paths hand DurableSleep context.Background() and the limits live on the session's execution context.
//
// The limits are per SEGMENT: "a workflow that suspends and resumes gets a fresh deadline each time" (--max-workflow-duration's own help). A hold has to give the
// same, which is why it PAUSES the limits for its duration and RESTARTS them when it ends, and not merely "does not charge the held time": excluding the
// held time would leave the work before the wait and the work after it adding up against one limit, where a suspension would have given the second a limit of
// its own. The "work 500 / wait / work 500 under 800ms" row is the one that tells the two apart.
//
// History of this file, because each row is only meaningful beside the others:
//
//	round 1 (first residency PR): a hold ran through the ceiling and the run FAILED with "execution timed out" (150ms ceiling, 300ms sleep: off suspends in 2-3ms,
//	  on held to 303ms and failed). Fixed by threading the context and refusing a wait due past the deadline.
//	round 2 (this file's current rows): a hold PAUSES the limits and restarts them, so that same case now COMPLETES (a hold is not charged to the run) and the
//	  KNOWN LIMIT pinned in round 1 (work 300 / wait 300 / work 300 under 800ms timed out at 904ms with residency on) is gone: it completes in about 905ms.

type residentRunOutcome int

const (
	residentRunCompleted residentRunOutcome = iota + 1
	residentRunSuspended
	residentRunTimedOut
	residentRunFenced
	residentRunOther
)

func (o residentRunOutcome) String() string {
	return map[residentRunOutcome]string{residentRunCompleted: "completed", residentRunSuspended: "suspended", residentRunTimedOut: "timed out", residentRunFenced: "killed by the execution fence", residentRunOther: "failed another way"}[o]
}

// phasedWorkCaller delays the "work" service by a different amount before and after the wait, told apart by the operation name the fixture uses.
type phasedWorkCaller struct{ before, after time.Duration }

func (c *phasedWorkCaller) Call(_ context.Context, service, op, _ string) (string, error) {
	if service == "work" {
		switch op {
		case "before_sleep":
			time.Sleep(c.before)
		case "after_sleep":
			time.Sleep(c.after)
		}
	}
	return `{"ok":true}`, nil
}

type residentRun struct {
	entry       string
	input       string
	before      time.Duration
	after       time.Duration
	ceiling     time.Duration
	workflowMax time.Duration // --max-workflow-duration
	resident    bool
	cancelAfter time.Duration
}

func runResident(t *testing.T, rt *Runtime, wt *wasmtimeBackend, wasmBytes []byte, r residentRun) (residentRunOutcome, time.Duration, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newReplayMetrics(t)
	opts := []EngineOption{WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-resident-limits"), WithWasmWallClockCeiling(r.ceiling)}
	if r.workflowMax > 0 {
		opts = append(opts, WithDefaultWorkflowTimeout(r.workflowMax))
	}
	if r.resident {
		cfg := DefaultResidencyConfig()
		cfg.MaxRuns = 2
		cfg.Metrics = m
		set, err := NewResidentSet(cfg)
		if err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		opts = append(opts, WithResidency(set.ForRun(ResidentRunInfo{WorkflowID: "wf-resident-limits", Language: "go", ClaimCheck: claimStillHeld})))
	}
	eng := NewEngine(rt, &phasedWorkCaller{before: r.before, after: r.after}, opts...)
	eng.Metrics = m
	if r.cancelAfter > 0 {
		time.AfterFunc(r.cancelAfter, cancel)
	}
	input := r.input
	if input == "" {
		input = `{}`
	}
	t0 := time.Now()
	res, _, suspended, _, _, execErr := eng.Execute(ctx, wasmBytes, r.entry, json.RawMessage(input))
	elapsed := time.Since(t0)
	holds := residentCounter(t, m, "cleat_resident_parks_total")

	switch {
	case execErr == nil && suspended == nil && res != "":
		return residentRunCompleted, elapsed, holds
	case execErr == nil && suspended != nil:
		return residentRunSuspended, elapsed, holds
	case execErr != nil && strings.Contains(execErr.Error(), "execution time limit exceeded"):
		return residentRunFenced, elapsed, holds
	case execErr != nil && strings.Contains(execErr.Error(), "timed out"):
		return residentRunTimedOut, elapsed, holds
	}
	t.Logf("unclassified outcome: res=%q suspended=%v err=%v", res, suspended != nil, execErr)
	return residentRunOther, elapsed, holds
}

func newResidentBackend(t *testing.T, fence time.Duration) (*Runtime, *wasmtimeBackend) {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(fence))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	return rt, wt
}

// warmUp runs the guest once with a long ceiling: the first run on a backend compiles the module, which takes longer than the short ceilings below. The
// ceiling must be what ends a run and not a cold start, or every short-ceiling row measures the compiler (the first version of these tests did exactly that).
func warmUp(t *testing.T, rt *Runtime, wt *wasmtimeBackend, wasmBytes []byte, entry, input string) {
	t.Helper()
	if got, elapsed, _ := runResident(t, rt, wt, wasmBytes, residentRun{entry: entry, input: input, ceiling: 30 * time.Second}); got != residentRunSuspended && got != residentRunCompleted {
		t.Fatalf("PRECONDITION FAILED: the warm-up run %s in %s", got, elapsed)
	}
}

func TestAHeldRunMeetsItsOwnLimitsAndCancelOnRealWasmtime(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "residentsleep"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, wt := newResidentBackend(t, 30*time.Second)
	warmUp(t, rt, wt, wasmBytes, "sleep_then_call", "")

	rows := []struct {
		name       string
		run        residentRun
		want       residentRunOutcome
		wantHeld   bool
		maxElapsed time.Duration // only for the cancel row
	}{
		{"150ms ceiling, residency off: suspends at the wait", residentRun{entry: "sleep_then_call", ceiling: 150 * time.Millisecond}, residentRunSuspended, false, 0},
		// The round-1 defect row. A 300ms wait under a 150ms ceiling used to be held to the ceiling and fail with a timeout. The limits are paused for the hold and restarted
		// at the wake now, so the run is held, wakes, and completes in a fresh 150ms segment.
		{"150ms ceiling, residency on: held, limits paused, completes", residentRun{entry: "sleep_then_call", ceiling: 150 * time.Millisecond, resident: true}, residentRunCompleted, true, 0},
		{"150ms workflow duration, residency on: the same", residentRun{entry: "sleep_then_call", ceiling: 5 * time.Second, workflowMax: 150 * time.Millisecond, resident: true}, residentRunCompleted, true, 0},
		{"5s ceiling, residency on: the hold works", residentRun{entry: "sleep_then_call", ceiling: 5 * time.Second, resident: true}, residentRunCompleted, true, 0},
		{"5s ceiling, residency off: suspends", residentRun{entry: "sleep_then_call", ceiling: 5 * time.Second}, residentRunSuspended, false, 0},
		// Cancellation is not a deadline: it reaches a held run at once whether or not the limits are paused.
		{"5s ceiling, residency on, cancelled at 100ms", residentRun{entry: "sleep_then_call", ceiling: 5 * time.Second, resident: true, cancelAfter: 100 * time.Millisecond}, residentRunOther, true, 250 * time.Millisecond},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, elapsed, holds := runResident(t, rt, wt, wasmBytes, r.run)
			t.Logf("%s: %s in %s, %d hold(s) recorded", r.name, got, elapsed.Round(time.Millisecond), holds)
			if r.run.cancelAfter > 0 {
				if got == residentRunCompleted {
					t.Errorf("a run cancelled at %s completed: the hold ran to its timer and the cancel was not seen", r.run.cancelAfter)
				}
				if elapsed > r.maxElapsed {
					t.Errorf("a run cancelled at %s took %s to end: the hold did not end at the cancel", r.run.cancelAfter, elapsed)
				}
			} else if got != r.want {
				t.Errorf("%s, want %s", got, r.want)
			}
			if r.wantHeld && holds == 0 {
				t.Errorf("PRECONDITION FAILED: no hold was recorded, so this row did not exercise residency")
			}
			if !r.wantHeld && holds != 0 {
				t.Errorf("%d hold(s) were recorded where none should be", holds)
			}
		})
	}
}

// The KNOWN LIMIT of round 1, now removed. Work 300ms, wait 300ms, work 300ms under an 800ms ceiling: with residency OFF the run suspends at the wait in about
// 303ms; with it ON the hold used to charge its 300ms to the ceiling, the work after the wake had 200ms left, and the run timed out at 904ms. It now completes.
//
// And the row that tells RESTART from the weaker "do not charge the held time": work 500, wait, work 500 under an 800ms ceiling. Excluding only the held time
// leaves 1000ms of WORK against one 800ms ceiling and times out; a restart gives the work after the wake a segment of its own, as the suspension it replaces
// would, and completes.
func TestAHeldWaitRestartsTheExecutionsLimitsNotJustExcludesTheHeldTimeOnRealWasmtime(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "residentsleep"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, wt := newResidentBackend(t, 30*time.Second)
	warmUp(t, rt, wt, wasmBytes, "work_sleep_work", "")
	const ceiling = 800 * time.Millisecond

	for _, c := range []struct {
		name          string
		before, after time.Duration
		resident      bool
		want          residentRunOutcome
		wantHeld      bool
	}{
		{"work 300 / wait / work 300, residency off", 300 * time.Millisecond, 300 * time.Millisecond, false, residentRunSuspended, false},
		{"work 500 / wait / work 500, residency off", 500 * time.Millisecond, 500 * time.Millisecond, false, residentRunSuspended, false},
		{"work 50 / wait / work 1000, residency off", 50 * time.Millisecond, 1000 * time.Millisecond, false, residentRunSuspended, false},
		{"work 300 / wait / work 300, residency on: the former known limit", 300 * time.Millisecond, 300 * time.Millisecond, true, residentRunCompleted, true},
		{"work 500 / wait / work 500, residency on: restart, not just exclusion", 500 * time.Millisecond, 500 * time.Millisecond, true, residentRunCompleted, true},
		// The limit still BINDS the segment after the wake. Without this row, "completes" could be a limit that was simply removed. 50ms of work, a wait, then
		// 1000ms of work under an 800ms ceiling must still time out, in the fresh segment.
		{"work 50 / wait / work 1000, residency on: the restarted limit still binds", 50 * time.Millisecond, 1000 * time.Millisecond, true, residentRunTimedOut, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, elapsed, holds := runResident(t, rt, wt, wasmBytes, residentRun{entry: "work_sleep_work", before: c.before, after: c.after, ceiling: ceiling, resident: c.resident})
			t.Logf("%s: %s in %s, %d hold(s)", c.name, got, elapsed.Round(time.Millisecond), holds)
			if got != c.want {
				t.Errorf("%s, want %s", got, c.want)
			}
			if c.wantHeld && holds == 0 {
				t.Errorf("PRECONDITION FAILED: the wait was not held, so this row did not exercise the restart")
			}
			if !c.wantHeld && holds != 0 {
				t.Errorf("%d hold(s) were recorded where none should be", holds)
			}
		})
	}
}
