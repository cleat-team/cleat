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

	// Size the guest's work and the fence, then PROVE the fence is the right size with a control: one spin must fit it. The fence is NEVER loosened to make a check pass: a looser fence
	// could let the row below pass without the fix, which is the one thing it must not do.
	//
	// cleat#4402. This used to size the fence as 1.7 x one measured spin and fail when two spins did not exceed it. Measured 2026-11-26 that ratio is not a constant of the guest: two spins
	// cost 1.57 to 1.89 x one while the machine was still speeding up (one spin 244, 213, 193, 179 ms in a row) and 1.98 to 2.01 once it was steady, with no fixed cost per run, and later in
	// a run the same machine moved by a third and stayed there (two spins 378 ms at calibration, 252 ms for the next four seconds). So: (1) WAIT until the speed is steady, (2) measure one
	// spin and two spins back to back, (3) put the fence BETWEEN what was measured, as a whole number of epoch ticks (fenceBetweenOneAndTwo: the guest is stopped by an epoch deadline of
	// floor(fence/50ms) ticks, which fires anywhere in the tick before it, so a fence of 242 ms is really 150 to 200 ms, and the one that failed killed a 179 ms spin at about 170 ms), with
	// spins long enough (about 500 ms) that a tick and a third of drift still leave room, (4) RECALIBRATE when a later check shows the speed moved, and (5) if the numbers cannot be
	// separated, say UNMEASURED with them, because that is a failure of the check and not a finding about the engine. The control and the known positive still decide, on the fence as set.
	const spinTarget = 500 * time.Millisecond
	var (
		rt2   *Runtime
		wt2   *wasmtimeBackend
		n     int
		d     time.Duration // the median one-spin run in the last calibration
		d2    time.Duration // the median two-spin run in the last calibration
		fence time.Duration
	)
	var sizeFailed, settleFailed, notSeparable, controlFailed, tooGenerous int
	timeRun := func(rt *Runtime, wt *wasmtimeBackend, entry string, n int) time.Duration {
		start := time.Now()
		runResident(t, rt, wt, wasmBytes, residentRun{entry: entry, input: spinInput(n), ceiling: 60 * time.Second})
		return time.Since(start)
	}
	// calibrate measures this machine NOW, with the fence out of the way, and sets the fence between what it saw. It reports why it could not, so the failure says which precondition.
	calibrate := func(rt *Runtime, wt *wasmtimeBackend, n int) (ok bool, why string) {
		wt.limits.executionTimeout = 60 * time.Second
		var settle []time.Duration
		for i := 0; i < 12 && !speedSettled(settle); i++ {
			settle = append(settle, timeRun(rt, wt, "spin_only", n))
		}
		if !speedSettled(settle) {
			settleFailed++
			return false, fmt.Sprintf("the speed never settled over %d one-spin runs (%v)", len(settle), settle)
		}
		// One spin and two spins, INTERLEAVED so a drift in speed reaches both.
		var once, twice []time.Duration
		for i := 0; i < 3; i++ {
			once = append(once, timeRun(rt, wt, "spin_only", n))
			twice = append(twice, timeRun(rt, wt, "spin_twice", n))
		}
		f, sep := fenceBetweenOneAndTwo(once, twice, epochTickInterval)
		if !sep {
			notSeparable++
			return false, fmt.Sprintf("one spin %v and two spins %v are not far enough apart to put a whole %s epoch tick and a margin on each side between them", once, twice, epochTickInterval)
		}
		fence, d, d2 = f, median(once), median(twice)
		// PerExecution copies the root backend's limits on every call, so setting the fence here is what every run below sees.
		wt.limits.executionTimeout = fence
		return true, ""
	}
	sized := false
	for attempt := 1; attempt <= 8 && !sized; attempt++ {
		rt, wt := newResidentBackend(t, 60*time.Second)
		timeRun(rt, wt, "spin_only", 1000) // warm-up compile
		n = 50_000_000
		first := timeRun(rt, wt, "spin_only", n)
		if first < 5*time.Millisecond {
			t.Fatalf("PRECONDITION FAILED: %d iterations took %s, too fast to size the work from", n, first)
		}
		n = int(float64(n) * float64(spinTarget) / float64(first)) // aim for about 500ms of guest work, from a cold measurement that overstates it
		if ok, why := calibrate(rt, wt, n); !ok {
			t.Logf("attempt %d: %s; retrying", attempt, why)
			continue
		}
		if d < spinTarget/3 || d > 3*spinTarget {
			sizeFailed++
			t.Logf("attempt %d: could not size the guest's work: %d iterations took at most %s (want about %s); retrying", attempt, n, d, spinTarget)
			continue
		}
		rt2, wt2 = rt, wt
		warmUp(t, rt2, wt2, wasmBytes, "spin_sleep_spin", spinInput(1000))
		if got, el, _ := runResident(t, rt2, wt2, wasmBytes, residentRun{entry: "spin_only", input: spinInput(n), ceiling: 60 * time.Second}); got != residentRunCompleted {
			controlFailed++
			t.Logf("attempt %d: the control failed: one spin %s in %s against a fence of %s (one spin measured at about %s); the speed changed since the calibration, retrying", attempt, got, el, fence, d)
			continue
		}
		if got, el, _ := runResident(t, rt2, wt2, wasmBytes, residentRun{entry: "spin_twice", input: spinInput(n), ceiling: 60 * time.Second}); got != residentRunFenced {
			tooGenerous++
			t.Logf("attempt %d: two spins in ONE segment were %s in %s against a fence of %s (two spins measured at about %s): the speed changed since the calibration, retrying", attempt, got, el, fence, d2)
			continue
		}
		sized = true
	}
	unmeasured := func(during string) {
		t.Helper()
		t.Fatalf("UNMEASURED %s: this is a failure of the check, not a finding about the engine, and the held wait's fence restart was NOT tested. "+
			"Calibration attempts ended at: the speed never settled %d time(s); the work could not be sized %d time(s); one spin and two spins were too close to separate %d time(s) (a whole %s epoch tick plus a margin on each side must fit between them); "+
			"the control (one spin must fit the fence) failed %d time(s); and two spins did NOT exceed the fence %d time(s). The last two mean the speed changed after the calibration. "+
			"It is reported as a failure and not a pass, because a pass here would be a green that measured nothing",
			during, settleFailed, sizeFailed, notSeparable, epochTickInterval, controlFailed, tooGenerous)
	}
	if !sized {
		unmeasured("after 8 attempts")
	}
	t.Logf("one spin is about %s and two spins about %s on this machine; the fence is %s", d.Round(time.Millisecond), d2.Round(time.Millisecond), fence.Round(time.Millisecond))

	// A run that was killed by the fence BEFORE it reached the wait measured nothing about the restart (the first spin was longer than the fence: the machine slowed), and a known positive that
	// is no longer killed means the machine sped up. Either way the fence is STALE, and retrying the same fence cannot help (the machine moved by a third and stayed there): recalibrate.
	// A run killed AFTER a hold was recorded is the real failure and is NOT retried.
	recalibrations := 0
	recalibrate := func(resident bool, why string) {
		t.Helper()
		recalibrations++
		if recalibrations > 4 {
			unmeasured(fmt.Sprintf("after 4 recalibrations (the last reason: %s)", why))
		}
		t.Logf("resident=%v: %s; recalibrating (%d of 4)", resident, why, recalibrations)
		if ok, why2 := calibrate(rt2, wt2, n); !ok {
			unmeasured(fmt.Sprintf("while recalibrating: %s", why2))
		}
		t.Logf("recalibrated: one spin is about %s and two spins about %s; the fence is %s", d.Round(time.Millisecond), d2.Round(time.Millisecond), fence.Round(time.Millisecond))
	}
	runToTheWait := func(resident bool) (residentRunOutcome, time.Duration, int) {
		for attempt := 1; attempt <= 8; attempt++ {
			// The known positive, immediately before: right now, two spins in one segment are killed by this fence. Without it a pass below could be a fence that drifted
			// generous since it was sized (this row went green once with the restart removed, which is what this guards).
			if two, el, _ := runResident(t, rt2, wt2, wasmBytes, residentRun{entry: "spin_twice", input: spinInput(n), ceiling: 60 * time.Second}); two != residentRunFenced {
				recalibrate(resident, fmt.Sprintf("attempt %d: two spins in one segment were %s in %s, so the fence is not tight right now", attempt, two, el.Round(time.Millisecond)))
				continue
			}
			got, el, holds := runResident(t, rt2, wt2, wasmBytes, residentRun{entry: "spin_sleep_spin", input: spinInput(n), ceiling: 60 * time.Second, resident: resident})
			if got == residentRunFenced && holds == 0 {
				recalibrate(resident, fmt.Sprintf("attempt %d: killed by the fence in %s before reaching the wait, so the first spin no longer fits it", attempt, el.Round(time.Millisecond)))
				continue
			}
			return got, el, holds
		}
		unmeasured(fmt.Sprintf("after 8 attempts to reach the wait (resident=%v)", resident))
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
		t.Errorf("residency on: %s, want completed. Two spins take at least %s against a fence of %s: the fence was not restarted when the held wait ended, so the guest was charged for the work of both segments", on, d2.Round(time.Millisecond), fence.Round(time.Millisecond))
	}
}
