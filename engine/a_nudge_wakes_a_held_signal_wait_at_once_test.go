package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ResidentSet.Nudge (cleat#4162, 2b-i, the listener half of 2b-ii). On PostgreSQL a delivery sends a NOTIFY that redispatches a SUSPENDED waiter at once, and a held run is `running`, so
// the NOTIFY finds nothing to dispatch and the held wait would wait for its next interval tick (cleat-review3, #4305). Nudge is the hint that closes that: the worker's NOTIFY listener calls
// it, and every held signal wait rechecks the store NOW. It is never the correctness path (the tick still finds anything a lost nudge would have), so what these rows pin is that it WAKES,
// that it does not RETURN a no-op, that a nudge landing while a recheck is running is not lost, and that a storm of them is one read per gap.

func nudgeSet(t *testing.T, gap time.Duration) (*ResidentSet, *fakeResidentClock) {
	t.Helper()
	return signalSet(t, func(c *ResidencyConfig) { c.NudgeMinInterval = gap })
}

func waitCalls(t *testing.T, rc *scriptedRecheck, n int) {
	t.Helper()
	limit := time.After(10 * time.Second)
	for rc.count() < n {
		select {
		case <-limit:
			t.Fatalf("the recheck ran %d times, wanted %d", rc.count(), n)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestANudgeMakesAHeldSignalWaitRecheckAtOnceWithoutAnyTimerFiring(t *testing.T) {
	set, clock := nudgeSet(t, 0)
	rc := &scriptedRecheck{answers: []recheckAnswer{{}, {found: true}}}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, rc))
	waitTimers(t, clock, 2) // admitted, held, the admission recheck found nothing
	notYet(t, out, "before the nudge")
	armed := clock.timerCount()
	set.Nudge()
	mustOutcome(t, out, parkWoke, "a nudge, with the delivery now there")
	if rc.count() != 2 {
		t.Errorf("the recheck ran %d times, want 2 (the admission recheck and the nudged one)", rc.count())
	}
	// No timer was FIRED: the only timers are the ones the hold armed itself, and the wake came from the nudge, not from the interval.
	if n := clock.timerCount(); n != armed {
		t.Errorf("%d timers were armed after the nudge, want none beyond the %d already there", n-armed, armed)
	}
}

func TestANudgeThatFindsNothingDoesNotEndTheHoldOrRestartAnything(t *testing.T) {
	m := newReplayMetrics(t)
	set, clock := signalSet(t, func(c *ResidencyConfig) { c.Metrics = m; c.NudgeMinInterval = 0 })
	// The admission recheck and three nudges find nothing; the tick after them finds it.
	rc := &scriptedRecheck{answers: []recheckAnswer{{}, {}, {}, {}, {found: true}}}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, rc))
	waitTimers(t, clock, 2)
	for i := 0; i < 3; i++ {
		set.Nudge()
		waitCalls(t, rc, i+2)
		notYet(t, out, "after a nudge that found nothing")
	}
	// Still ONE hold: nudges are rechecks, not returns, so the limits segment is not restarted by any of them.
	if n := residentCounter(t, m, "cleat_resident_parks_total"); n != 0 {
		t.Errorf("%d holds ended for a wait that is still held: a no-op nudge must not return", n)
	}
	if n := set.Len(); n != 1 {
		t.Errorf("%d runs held, want 1", n)
	}
	// And the interval tick still works afterwards: the nudge is a hint on top of it, not a replacement.
	clock.fireShorterThan(time.Second)
	mustOutcome(t, out, parkWoke, "the tick after three nudges")
}

// THE ORDERING THAT MAKES A NUDGE NOT LOSSY. The hold fetches the nudge channel BEFORE it rechecks. A nudge that lands while a recheck is running (the delivery committed after the
// recheck's read, and the NOTIFY arrived before the recheck returned) closes the channel the hold already holds, so the next loop sees it at once; a hold that fetched the channel AFTER
// the recheck would miss exactly that nudge and wait a full interval for the delivery it was told about.
func TestANudgeThatLandsWhileARecheckIsRunningIsNotLost(t *testing.T) {
	set, clock := nudgeSet(t, 0)
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	recheck := func(context.Context) (bool, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 2: // the tick-triggered recheck: block INSIDE it, with its read already "done"
			close(entered)
			<-release
			return false, nil
		case 3: // the one the nudge must cause, with no timer fired
			return true, nil
		}
		return false, nil
	}
	req := signalReq(5000, nil)
	req.Recheck = recheck
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), req)
	waitTimers(t, clock, 2)
	clock.fireShorterThan(time.Second) // the first tick: recheck #2 starts and blocks
	<-entered
	set.Nudge() // lands DURING recheck #2
	close(release)
	mustOutcome(t, out, parkWoke, "a nudge that landed during a recheck")
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("the recheck ran %d times, want 3: the nudge that landed during #2 should have caused #3 without any timer", calls)
	}
}

// A NOTIFY channel carries every dispatch wake-up, so without a floor a write-heavy database makes each held wait read the store once per write. Inside NudgeMinInterval of the last recheck a
// nudge is answered ONCE, when the gap ends, and never dropped.
func TestAStormOfNudgesIsOneReadPerGapAndNoneAreDropped(t *testing.T) {
	set, clock := nudgeSet(t, 50*time.Millisecond)
	rc := &scriptedRecheck{answers: []recheckAnswer{{}, {}, {found: true}}}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, rc))
	waitTimers(t, clock, 2)
	// The admission recheck ran at fake t0 and the fake clock has not moved: every nudge below is inside the gap.
	for i := 0; i < 20; i++ {
		set.Nudge()
	}
	time.Sleep(80 * time.Millisecond)
	if rc.count() != 1 {
		t.Fatalf("the recheck ran %d times during a storm inside the gap, want still 1: the floor did not hold", rc.count())
	}
	notYet(t, out, "during the gap")
	// The gap timer is the one armed with a duration at or below the floor; fire only it.
	clock.fireShorterThan(100 * time.Millisecond)
	waitCalls(t, rc, 2)
	if rc.count() != 2 {
		t.Errorf("after the gap the recheck ran %d times, want exactly 2: twenty nudges are one read", rc.count())
	}
	notYet(t, out, "after the one read the gap allows")
	// Time moves past the floor, and a later nudge is answered at once.
	clock.nowMs.Add(60)
	set.Nudge()
	mustOutcome(t, out, parkWoke, "a nudge after the floor")
	if rc.count() != 3 {
		t.Errorf("the recheck ran %d times, want 3", rc.count())
	}
}

func TestNudgeIsSafeOnANilASetWithNothingHeldAndAClosedSet(t *testing.T) {
	var nilSet *ResidentSet
	nilSet.Nudge()
	set, _ := nudgeSet(t, 0)
	set.Nudge() // nothing held
	set.EvictAll(ResidentEvictedShutdown)
	set.Nudge() // after the set has sent everything back
}

// A nudge is for signal waits. A held TIMER has nothing to recheck and must not be disturbed by one.
func TestANudgeDoesNotDisturbAHeldTimer(t *testing.T) {
	set, clock := nudgeSet(t, 0)
	req := parkRequest{Kind: wakeTimer, DeadlineMs: residentT + 5000, Reason: "sleep", Until: time.UnixMilli(residentT + 5000)}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), req)
	waitTimers(t, clock, 1)
	for i := 0; i < 3; i++ {
		set.Nudge()
	}
	notYet(t, out, "a timer after nudges")
	clock.fireAll()
	mustOutcome(t, out, parkDeadline, "the timer's own deadline")
}

// THE FLOOR HOLDS FOR THE WHOLE HOLD, NOT ONLY ITS FIRST 50 ms (cleat-review3 on #4305, a surviving mutant). The row above puts every nudge right after the admission recheck, so a hold that
// forgot to move its last-recheck time forward after a later read would still pass it: the floor would hold only until the first gap ended and then never again, and a write storm that began
// later in the hold would pin the store. Here nudges arrive every 10 ms for 500 ms of fake time, the gap timers are fired as the clock passes them, and the number of reads is bounded by
// one per gap (about 10) and not one per nudge (50).
func TestTheNudgeFloorHoldsAcrossTheWholeHoldAndNotJustItsFirstGap(t *testing.T) {
	set, clock := nudgeSet(t, 50*time.Millisecond)
	rc := &scriptedRecheck{} // never finds
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, rc))
	waitTimers(t, clock, 2)
	const steps, stepMs = 50, 10 // 500 ms of fake time
	for i := 0; i < steps; i++ {
		clock.nowMs.Add(stepMs)
		set.Nudge()
		time.Sleep(2 * time.Millisecond) // the hold takes the nudge and arms (or skips) its gap
		// A fake timer fires only when the test says so, so it must say so only when fake time has REACHED the timer's due time. The gaps here are due on 50 ms boundaries, so the
		// gap timers (the only ones under 100 ms; the 500 ms ticks and the deadline stay put) are fired on every fifth step. Firing them on every step answers each throttled nudge
		// at once, which is the same as having no floor: the first version of this row did that, and read 51 reads on the UNMUTATED code.
		if (i+1)%(50/stepMs) == 0 {
			clock.fireShorterThan(100 * time.Millisecond)
		}
		time.Sleep(3 * time.Millisecond)
	}
	notYet(t, out, "after 500 ms of nudges that found nothing")
	n := rc.count()
	// One admission read, then at most one per 50 ms gap: 1 + 10 = 11. A little headroom for a gap that ends on a step boundary; nowhere near the 51 an absent floor reads.
	if n > 14 {
		t.Errorf("the recheck ran %d times for 50 nudges over 500 ms with a 50 ms floor, want about 11: the floor stopped holding after the first gap", n)
	}
	if n < 6 {
		t.Errorf("the recheck ran only %d times over 500 ms of nudges: the floor is swallowing nudges it should answer, or the row is not exercising it", n)
	}
	t.Logf("%d rechecks for 50 nudges over 500 ms (floor 50 ms)", n)
}
