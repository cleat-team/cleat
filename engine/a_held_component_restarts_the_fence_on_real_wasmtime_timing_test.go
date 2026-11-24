//go:build cgo && timing

package engine

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// THE COMPONENT TWIN OF THE CORE ROW (a_held_wait_restarts_the_fence_on_real_wasmtime_timing_test.go), kept out of the gating jobs for the same reason and run by the
// same step of the nightly "Chaos full sweep" job, which installs componentize-py:
//
//	go test -tags timing -run 'TestPythonAHeldWaitRestartsTheExecutionFenceOnAComponent' ./engine/
//
// WHY IT EXISTS. The earlier component row (a_real_component_is_held_through_a_sleep_and_charged_its_language_test.go: a runaway loop after a hold is stopped about one
// fence after the wake) PASSED WITH THE RESTART REMOVED: a guest that spins at once after the wake is stopped one fence later whether the fence was restarted or merely
// re-armed. A restart differs from a re-arm only for a guest that had already used most of its fence when the hold began, and componentize-py pre-initialises the
// interpreter, so a Python guest has almost no guest time before its first host call unless it makes some. This one does: it spins for about 60% of a fence BEFORE the
// hold and again AFTER it. With the restart the second spin fits a whole fresh fence; without it the second spin is killed for the work of both, where a suspended run (the
// residency-off control) would have started its next segment fresh. THAT is the parity the restart exists for, and this row must go RED with the component builder's
// onLimitsRestart registration (component_cgo.go) removed.
//
// PYTHON ONLY. The restart is registered by the one component builder shared by every language, so one language discriminates it, but only Python is run here; a C# twin
// would need a new .NET build of the fixture.
//
// The spin is sized on THIS machine first (a fixed iteration count would be a different test on a slower one), measured as the gap between the guest's first two host
// calls, which is the loop and not the component compile. The fence is then set to 1.7 times one spin: one spin fits, two do not. As in the core row, the sizing and both
// controls are retried a few times and the fence is NEVER loosened to make a control pass.

func TestPythonAHeldWaitRestartsTheExecutionFenceOnAComponent(t *testing.T) {
	h := newPythonWasmTestHelper(t)
	if !h.toolsAvailable() {
		if toolchainRequired("python") {
			t.Fatalf("Python WASM prerequisites not met, but %s declares python: %s", requireToolchainEnv, h.missingTools())
		}
		t.Skip("Python WASM prerequisites not met: " + h.missingTools())
	}
	b, err := os.ReadFile(h.compileWorkflow(t, "held_spin_sleep_spin_workflow.py", "held_spin_sleep_spin"))
	if err != nil {
		t.Fatalf("read the compiled guest: %v", err)
	}
	const sleepMs = 400
	bound := WithDefaultWorkflowTimeout(120 * time.Second) // a backstop only: a missing fence fails in two minutes instead of hanging
	fenced := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "execution time limit exceeded")
	}
	input := func(spin1, spin2, sleep int) string {
		return fmt.Sprintf(`{"spin1":%d,"spin2":%d,"sleep_ms":%d}`, spin1, spin2, sleep)
	}
	run := func(fence time.Duration, spin1, spin2, sleep int, resident bool) (heldRun, error) {
		return tryExecuteAComponent(t, b, WasmtimeLanguages, input(spin1, spin2, sleep), resident, "spin-sleep-spin@1", []WasmtimeOption{WithWasmtimeExecutionTimeout(fence)}, bound)
	}
	// spins measures both loops of a run with no sleep, as the gaps between the guest's calls: the first loop is call 1 to call 2, the second is call 2 to call 3.
	spins := func(n1, n2 int) (s1, s2 time.Duration, err error) {
		a, err := run(2*time.Minute, n1, n2, 0, false)
		if err != nil {
			return 0, 0, err
		}
		if len(a.callTimes) < 3 {
			return 0, 0, fmt.Errorf("the guest made %d calls, want 3", len(a.callTimes))
		}
		return a.callTimes[1].Sub(a.callTimes[0]), a.callTimes[2].Sub(a.callTimes[1]), nil
	}

	// Size both loops and the fence, then PROVE the fence is the right size: one loop must fit it, two in one segment must not.
	//
	// THE TWO LOOPS ARE SIZED SEPARATELY, and that is the lesson of this row's first version. It sized one loop and ran it twice, and the row stayed GREEN with the restart
	// removed: the second loop of a fresh instance runs faster than the first (the first pays cold-start costs; measured here at about half), so "two loops in one
	// segment" often fit a fence of 1.7 times the first one, and a second loop with only what the first left over did too. The second loop's iteration count is scaled so
	// that it takes as long as the first on this machine.
	var (
		n1, n2 int
		d      time.Duration
		fence  time.Duration
	)
	sized := false
	for attempt := 1; attempt <= 8 && !sized; attempt++ {
		n1, n2 = 1_000_000, 1_000_000
		s1, s2, err := spins(n1, n2)
		if err != nil || s1 < 5*time.Millisecond || s2 < 5*time.Millisecond {
			t.Fatalf("PRECONDITION FAILED: the sizing run did not complete or was too fast to size from (err=%v, loops %s and %s); nothing was measured", err, s1, s2)
		}
		n1 = int(float64(n1) * float64(300*time.Millisecond) / float64(s1)) // aim for about 300 ms of guest work in the first loop
		n2 = int(float64(n2) * float64(300*time.Millisecond) / float64(s2)) // and the same in the second, which runs at its own speed
		s1, s2, err = spins(n1, n2)
		if err != nil {
			t.Fatalf("PRECONDITION FAILED: the sized run failed: %v", err)
		}
		longer := max(s1, s2)
		shorter := min(s1, s2)
		if shorter < 100*time.Millisecond || longer > 1500*time.Millisecond || float64(longer) > 1.25*float64(shorter) {
			t.Logf("attempt %d: could not size the two loops to match: %d and %d iterations took %s and %s (want about 300 ms each, within 25%% of each other); retrying", attempt, n1, n2, s1, s2)
			continue
		}
		d = longer
		fence = time.Duration(float64(d) * 1.7)
		if s1+s2 <= fence {
			t.Logf("attempt %d: the two loops (%s + %s) fit one fence of %s: too generous to tell a restart from none; retrying", attempt, s1, s2, fence)
			continue
		}
		if _, err := run(fence, n1, 0, 0, false); err != nil {
			t.Logf("attempt %d: the control failed: the first loop (%s) did not fit a fence of %s (%v); the machine is too loaded to size it, retrying", attempt, s1, fence, err)
			continue
		}
		if _, err := run(fence, 0, n2, 0, false); err != nil {
			t.Logf("attempt %d: the control failed: the second loop (%s) did not fit a fence of %s (%v); retrying", attempt, s2, fence, err)
			continue
		}
		if _, err := run(fence, n1, n2, 0, false); !fenced(err) {
			t.Logf("attempt %d: the two loops in ONE segment were not stopped by a fence of %s (%v): the fence is too generous to tell a restart from none; retrying", attempt, fence, err)
			continue
		}
		sized = true
	}
	if !sized {
		t.Fatalf("PRECONDITION FAILED after 8 attempts: each loop never fit a fence sized from them, or the two never exceeded it. The machine is too loaded for this row to measure anything; it is reported as a failure and not a pass, because a pass here would mean nothing")
	}
	t.Logf("the loops are about %s each (%d and %d iterations); the fence is %s", d.Round(time.Millisecond), n1, n2, fence.Round(time.Millisecond))

	// A run killed BEFORE it reached the hold measured nothing about the restart (a load spike made the first spin exceed the fence), so it is run again. A run killed
	// AFTER a hold was recorded is the real failure and is NOT retried. The known positive runs immediately before each attempt: right now two spins in one segment are
	// stopped by this fence, because without it a pass below could be a fence that drifted generous since it was sized.
	runToTheWait := func(resident bool) (heldRun, error) {
		for attempt := 1; attempt <= 8; attempt++ {
			if _, err := run(fence, n1, n2, 0, false); !fenced(err) {
				t.Logf("attempt %d (resident=%v): two spins in one segment were not stopped (%v): the fence is not tight right now, so nothing would be measured; retrying", attempt, resident, err)
				continue
			}
			got, err := run(fence, n1, n2, sleepMs, resident)
			if fenced(err) && got.claimChecks == 0 {
				t.Logf("attempt %d (resident=%v): stopped by the fence before the hold: a load spike on the first spin, so nothing was measured; retrying", attempt, resident)
				continue
			}
			return got, err
		}
		t.Fatalf("PRECONDITION FAILED: the first spin was stopped by the fence on 8 attempts in a row (resident=%v): the machine is too loaded to measure the restart", resident)
		return heldRun{}, nil
	}

	// RESIDENCY OFF is the parity case: the run suspends at the sleep (its first spin fit the fence) and the next claim starts a fresh execution, so the second spin
	// has a whole fence.
	off, err := runToTheWait(false)
	if err != nil || !off.suspended || len(off.ops) != 2 {
		t.Errorf("residency off: err=%v suspended=%v ops=%v, want a suspension at the sleep after two calls (the first spin fits the fence)", err, off.suspended, off.ops)
	}

	on, err := runToTheWait(true)
	t.Logf("residency on: err=%v suspended=%v claim checks=%d ops=%v", err, on.suspended, on.claimChecks, on.ops)
	if on.claimChecks != 1 {
		t.Fatalf("PRECONDITION FAILED: the sleep was not held (claim checks %d), so this row did not exercise the fence at a wake (outcome err=%v suspended=%v)", on.claimChecks, err, on.suspended)
	}
	if err != nil {
		t.Errorf("residency on: the run was stopped after its hold: %v\n\nTwo loops of about %s each against a fence of %s: the fence was not restarted when the held wait ended, so the guest was charged for the work of BOTH segments where a suspended run's next segment would have started fresh.", err, d.Round(time.Millisecond), fence.Round(time.Millisecond))
		return
	}
	if on.suspended {
		t.Errorf("residency on: the run suspended instead of finishing in place (it was held but did not complete)")
	}
}
