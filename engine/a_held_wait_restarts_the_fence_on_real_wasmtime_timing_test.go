//go:build cgo && timing

package engine

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// THE TIMING ROW, kept out of the gating jobs on purpose. It sizes a spin from a measurement on the machine it runs on and then needs two spins to be killed by a
// fence that one spin fits, so it is sensitive to machine load: at a load of about 40 it can end PRECONDITION FAILED (never a pass, by construction), and a required
// Tier 1 job must not be redded by a loaded runner. So it sits behind the `timing` build tag, which also means it emits NO skip for the Tier 1 gate to count: it is not
// compiled there at all.
//
// It is run by the "Chaos full sweep" job (nightly, and on demand with workflow_dispatch), which is not a pull-request job:
//
//	go test -tags timing -run 'TestAHeldWaitRestartsTheExecutionFenceOnRealWasmtime' ./engine/
//
// What stays in Tier 1 is everything about the fence restart that needs no clock: the arithmetic of restartFence (a_held_wait_restart_of_the_fence_is_arithmetic_test.go)
// and the structural test that every builder of the fence budget registers it (execution_limits_test.go). This row is the only place that shows, on a real guest, that
// the epoch deadline is re-armed from what restartFence set.

// The execution FENCE (--wasm-instance-timeout) is a separate budget, charged only for GUEST execution and seeded from the context's remaining time, and it is
// restarted with the limits: otherwise a guest that computes before and after a held wait is killed for the total of its two spins where a suspension would have
// given each segment its own. The guest's work is sized on THIS machine first (a fixed iteration count would be a different test on a slower one), then the fence
// is set to 1.7 times one spin: one spin fits, two do not.
func TestAHeldWaitRestartsTheExecutionFenceOnRealWasmtime(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "residentsleep"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	spinInput := func(n int) string { return fmt.Sprintf(`{"n":%d}`, n) }

	// Size the guest's work and the fence, then PROVE the fence is the right size with a control: one spin must fit it. That control is the load-sensitive part
	// (the first version failed it with another suite running on the same machine: one spin timed 306ms and then took 483ms against a fence of 520ms), and a test that
	// fails on machine load is a test that gets disabled. So the sizing and the control are retried, a few times, and only a run where the control passes goes on.
	// The fence is NEVER loosened to make the control pass: a looser fence could let the row below pass without the fix, which is the one thing it must not do.
	var (
		rt2   *Runtime
		wt2   *wasmtimeBackend
		n     int
		d     time.Duration
		fence time.Duration
	)
	sized := false
	for attempt := 1; attempt <= 8 && !sized; attempt++ {
		rt, wt := newResidentBackend(t, 60*time.Second)
		timeSpin := func(n int) time.Duration {
			start := time.Now()
			runResident(t, rt, wt, wasmBytes, residentRun{entry: "spin_only", input: spinInput(n), ceiling: 60 * time.Second})
			return time.Since(start)
		}
		timeSpin(1000) // warm-up compile
		n = 50_000_000
		d = timeSpin(n)
		if d < 5*time.Millisecond {
			t.Fatalf("PRECONDITION FAILED: %d iterations took %s, too fast to size the work from", n, d)
		}
		n = int(float64(n) * float64(300*time.Millisecond) / float64(d)) // aim for about 300ms of guest work
		d = timeSpin(n)
		if d < 100*time.Millisecond || d > 1500*time.Millisecond {
			t.Logf("attempt %d: could not size the guest's work: %d iterations took %s (want about 300ms); retrying", attempt, n, d)
			continue
		}
		fence = time.Duration(float64(d) * 1.7)
		rt2, wt2 = newResidentBackend(t, fence)
		warmUp(t, rt2, wt2, wasmBytes, "spin_sleep_spin", spinInput(1000))
		if got, el, _ := runResident(t, rt2, wt2, wasmBytes, residentRun{entry: "spin_only", input: spinInput(n), ceiling: 60 * time.Second}); got != residentRunCompleted {
			t.Logf("attempt %d: the control failed: one spin %s in %s against a fence of %s (sized from %s); the machine is too loaded to size it, retrying", attempt, got, el, fence, d)
			continue
		}
		if got, el, _ := runResident(t, rt2, wt2, wasmBytes, residentRun{entry: "spin_twice", input: spinInput(n), ceiling: 60 * time.Second}); got != residentRunFenced {
			t.Logf("attempt %d: two spins in ONE segment were %s in %s against a fence of %s: the fence was sized from a slow measurement and is too generous to tell a restart from none; retrying", attempt, got, el, fence)
			continue
		}
		sized = true
	}
	if !sized {
		t.Fatalf("PRECONDITION FAILED after 8 attempts: one spin never fit a fence sized from it. The machine is too loaded for this row to measure anything; it is reported as a failure and not a pass, because a pass here would be a green that measured nothing")
	}
	t.Logf("one spin is about %s on this machine; the fence is %s", d.Round(time.Millisecond), fence.Round(time.Millisecond))

	// A run that was killed by the fence BEFORE it reached the wait measured nothing about the restart (a load spike made the first spin longer than the fence), so
	// it is run again. A run killed AFTER a hold was recorded is the real failure and is NOT retried.
	runToTheWait := func(resident bool) (residentRunOutcome, time.Duration, int) {
		for attempt := 1; attempt <= 8; attempt++ {
			// The known positive, immediately before: right now, two spins in one segment are killed by this fence. Without it a pass below could be a fence that drifted
			// generous since it was sized (this row went green once with the restart removed, which is what this guards).
			if two, el, _ := runResident(t, rt2, wt2, wasmBytes, residentRun{entry: "spin_twice", input: spinInput(n), ceiling: 60 * time.Second}); two != residentRunFenced {
				t.Logf("attempt %d (resident=%v): two spins in one segment were %s in %s: the fence is not tight right now, so nothing would be measured; retrying", attempt, resident, two, el.Round(time.Millisecond))
				continue
			}
			got, el, holds := runResident(t, rt2, wt2, wasmBytes, residentRun{entry: "spin_sleep_spin", input: spinInput(n), ceiling: 60 * time.Second, resident: resident})
			if got == residentRunFenced && holds == 0 {
				t.Logf("attempt %d (resident=%v): killed by the fence in %s before reaching the wait: a load spike on the first spin, so nothing was measured; retrying", attempt, resident, el.Round(time.Millisecond))
				continue
			}
			return got, el, holds
		}
		t.Fatalf("PRECONDITION FAILED: the first spin was killed by the fence on 8 attempts in a row (resident=%v): the machine is too loaded for this row to measure the restart", resident)
		return 0, 0, 0
	}
	off, _, offHolds := runToTheWait(false)
	if off != residentRunSuspended || offHolds != 0 {
		t.Errorf("residency off: %s with %d hold(s), want suspended at the wait (the first segment's spin fits the fence)", off, offHolds)
	}
	on, el, onHolds := runToTheWait(true)
	t.Logf("residency on: %s in %s, %d hold(s)", on, el.Round(time.Millisecond), onHolds)
	if onHolds == 0 {
		t.Fatalf("PRECONDITION FAILED: the wait was not held, so this row did not exercise the fence (outcome %s)", on)
	}
	if on != residentRunCompleted {
		t.Errorf("residency on: %s, want completed. Two spins are 2 x %s against a fence of %s: the fence was not restarted when the held wait ended, so the guest was charged for the work of both segments", on, d.Round(time.Millisecond), fence.Round(time.Millisecond))
	}
}
