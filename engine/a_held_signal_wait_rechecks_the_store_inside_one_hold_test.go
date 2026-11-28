package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A HELD SIGNAL WAIT (cleat#4162, 2b-i). The wait is woken by a store write, so the resident set asks the call's Recheck whether the write has landed: once straight after it admits the
// run, and then every RecheckInterval, all inside ONE hold. These rows pin that with no store and no real clock. What they do NOT show is that the Recheck finds a real delivery on a real
// store, or that a signal landing after the last recheck is not lost when the hold is sent back: those are the real-store rows (a_held_signal_wait_is_woken_by_a_real_store_test.go).

type recheckAnswer struct {
	found bool
	err   error
}

// scriptedRecheck answers in order and then says "nothing yet", and counts its calls.
type scriptedRecheck struct {
	mu      sync.Mutex
	answers []recheckAnswer
	calls   int
}

func (r *scriptedRecheck) fn(context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.calls
	r.calls++
	if i < len(r.answers) {
		return r.answers[i].found, r.answers[i].err
	}
	return false, nil
}

func (r *scriptedRecheck) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// signalReq is a signal wait due remainingMs from the fake clock's now.
func signalReq(remainingMs int64, rc *scriptedRecheck) parkRequest {
	req := parkRequest{Kind: wakeSignal, Key: "go", DeadlineMs: residentT + remainingMs, Reason: "await_signals(go)", Until: time.UnixMilli(residentT + remainingMs)}
	if rc != nil {
		req.Recheck = rc.fn
	}
	return req
}

// waitTimers blocks until the fake clock has had n timers started. The hold arms the deadline and then a tick, and a new tick after every recheck.
func waitTimers(t *testing.T, clock *fakeResidentClock, n int) {
	t.Helper()
	limit := time.After(10 * time.Second)
	for clock.timerCount() < n {
		select {
		case <-clock.armed:
		case <-time.After(2 * time.Millisecond):
		case <-limit:
			t.Fatalf("the hold never armed timer %d (it has armed %d)", n, clock.timerCount())
		}
	}
}

// notYet fails if the wait has already ended: a no-op recheck must not return.
func notYet(t *testing.T, out <-chan parkOutcome, what string) {
	t.Helper()
	select {
	case got := <-out:
		t.Fatalf("%s: the wait ended with %d, but nothing had been found", what, got)
	case <-time.After(60 * time.Millisecond):
	}
}

func signalSet(t *testing.T, mut func(*ResidencyConfig)) (*ResidentSet, *fakeResidentClock) {
	t.Helper()
	clock := newFakeResidentClock(residentT)
	return newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) {
		c.RecheckInterval = 500 * time.Millisecond // far below the 5 s the waits below are due in, so fireShorterThan(time.Second) fires the ticks and not the deadline
		if mut != nil {
			mut(c)
		}
	})), clock
}

func TestASignalWaitIsHeldOnlyWithARecheckAndAnInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		req      func() parkRequest
		interval time.Duration
		held     bool
	}{
		{"with a recheck and an interval", func() parkRequest { return signalReq(5000, &scriptedRecheck{}) }, 500 * time.Millisecond, true},
		{"with no recheck: nothing could wake it", func() parkRequest { return signalReq(5000, nil) }, 500 * time.Millisecond, false},
		{"with no interval: the safe reading of an unset field", func() parkRequest { return signalReq(5000, &scriptedRecheck{}) }, 0, false},
		{"a promise wait still has no wake source, recheck or not", func() parkRequest {
			r := signalReq(5000, &scriptedRecheck{})
			r.Kind = wakePromise
			return r
		}, 500 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReplayMetrics(t)
			set, clock := signalSet(t, func(c *ResidencyConfig) { c.Metrics = m; c.RecheckInterval = tc.interval })
			out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), tc.req())
			if tc.held {
				waitTimers(t, clock, 2)
				if n := set.Len(); n != 1 {
					t.Fatalf("%d runs held, want 1", n)
				}
				clock.fireAll()
				mustOutcome(t, out, parkDeadline, "the signal wait")
				return
			}
			mustOutcome(t, out, parkRefused, "a signal wait that cannot be woken")
			if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="kind"`); n != 1 {
				t.Errorf("the refusal was counted %d time(s) as kind, want 1", n)
			}
			if n := set.Len(); n != 0 {
				t.Errorf("%d runs held after a refusal, want 0", n)
			}
		})
	}
}

// The race window the call leaves open: it read the store, THEN asked to be held. A signal committed in between is found by the recheck that runs straight after admission, so a
// delivery that is already there never waits an interval. Asserted by the number of timers armed: a found delivery returns before the hold arms any.
func TestARecheckRunsStraightAfterAdmissionSoADeliveryAlreadyThereNeverWaitsAnInterval(t *testing.T) {
	m := newReplayMetrics(t)
	set, clock := signalSet(t, func(c *ResidencyConfig) { c.Metrics = m })
	rc := &scriptedRecheck{answers: []recheckAnswer{{found: true}}}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, rc))
	mustOutcome(t, out, parkWoke, "a delivery that was already there")
	if n := clock.timerCount(); n != 0 {
		t.Errorf("%d timer(s) were armed: the delivery was found only after the hold began waiting, i.e. it waited for a tick", n)
	}
	if rc.count() != 1 {
		t.Errorf("the recheck ran %d times, want exactly 1", rc.count())
	}
	if n := residentCounter(t, m, "cleat_resident_rechecks_total", `result="found"`); n != 1 {
		t.Errorf("counted %d found recheck(s), want 1", n)
	}
	if n := chargedBytes(set); n != 0 {
		t.Errorf("the set is still charged %d bytes after the run left", n)
	}
	if n := residentCounter(t, m, "cleat_resident_parks_total", `outcome="woke"`); n != 1 {
		t.Errorf("the hold was counted %d time(s) as ending woke, want 1", n)
	}
}

func TestANoOpRecheckDoesNotReturnAndTheHoldEndsOnTheTickThatFindsTheDelivery(t *testing.T) {
	m := newReplayMetrics(t)
	set, clock := signalSet(t, func(c *ResidencyConfig) { c.Metrics = m })
	// The admission recheck, then two ticks that find nothing, then the tick that finds it.
	rc := &scriptedRecheck{answers: []recheckAnswer{{}, {}, {}, {found: true}}}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, rc))
	waitTimers(t, clock, 2) // the deadline and the first tick
	notYet(t, out, "before the first tick")
	for i, wantTimers := range []int{3, 4} {
		clock.fireShorterThan(time.Second)
		waitTimers(t, clock, wantTimers) // the hold re-armed its tick, so the recheck ran and found nothing
		notYet(t, out, "after a tick that found nothing")
		if rc.count() != i+2 {
			t.Fatalf("after tick %d the recheck had run %d times, want %d", i+1, rc.count(), i+2)
		}
	}
	clock.fireShorterThan(time.Second)
	mustOutcome(t, out, parkWoke, "the tick that found the delivery")
	if rc.count() != 4 {
		t.Errorf("the recheck ran %d times, want 4", rc.count())
	}
	if n, f := residentCounter(t, m, "cleat_resident_rechecks_total", `result="none"`), residentCounter(t, m, "cleat_resident_rechecks_total", `result="found"`); n != 3 || f != 1 {
		t.Errorf("counted %d none and %d found, want 3 and 1", n, f)
	}
	// ONE hold, however many rechecks: one park counted, one run charged and given back.
	if n := residentCounter(t, m, "cleat_resident_parks_total"); n != 1 {
		t.Errorf("%d holds counted for a wait that rechecked 4 times, want 1: a no-op recheck must not end the hold", n)
	}
	if n := chargedBytes(set); n != 0 {
		t.Errorf("the set is still charged %d bytes", n)
	}
}

// A NO-OP RECHECK MUST NOT RESTART THE LIMITS SEGMENT. A return from Wait that is not a refusal restarts the run's wall-clock ceiling, workflow deadline and fence, so a hold that
// returned a wake on every tick would restart them on every no-op wake and let a looping hold outlive its own ceiling. The limit below is 150 ms and the hold lasts about 600 ms of real
// time across four no-op ticks: if any tick restarted it, it would be running and would expire mid-hold. Then it must come back in FULL at the end, once.
func TestANoOpRecheckDoesNotRestartTheLimitsSegmentButTheEndOfTheHoldDoes(t *testing.T) {
	set, clock := signalSet(t, nil)
	ctx, cancel := limitedSetCtx(t, 150*time.Millisecond)
	defer cancel()
	rc := &scriptedRecheck{answers: []recheckAnswer{{}, {}, {}, {}, {}, {found: true}}}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, signalReq(5000, rc))
	waitTimers(t, clock, 2)
	for i := 0; i < 4; i++ {
		time.Sleep(150 * time.Millisecond) // real time passes inside the hold; the limit must not run
		clock.fireShorterThan(time.Second)
		waitTimers(t, clock, 3+i)
		if ctx.Err() != nil {
			t.Fatalf("the limit expired during the hold, after %d no-op recheck(s): a recheck restarted the segment instead of leaving it paused (%v)", i+1, ctx.Err())
		}
	}
	notYet(t, out, "after four no-op rechecks")
	clock.fireShorterThan(time.Second)
	mustOutcome(t, out, parkWoke, "the recheck that found the delivery")
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < 100*time.Millisecond {
		t.Errorf("after the hold the limit has %v left, want about its full 150ms: the end of the hold restarts it", time.Until(dl))
	}
}

// The call's own loop (awaitSuspendOrPark) re-calls Wait on every wake that finds nothing. This replays that shape against a registry that counts restarts: with the recheck INSIDE the
// hold the segment restarts exactly once, at the end. The rejected design (return a wake on every tick) would restart it once per tick.
func TestTheLimitsSegmentRestartsExactlyOnceAcrossManyNoOpRechecksInTheCallsOwnLoop(t *testing.T) {
	set, clock := signalSet(t, nil)
	ctx, cancel := limitedSetCtx(t, 5*time.Second)
	defer cancel()
	restarts := 0
	onLimitsRestart(ctx, func() { restarts++ })
	var delivered sync.Mutex
	arrived := false
	rc := &scriptedRecheck{}
	req := signalReq(5000, rc)
	inner := req.Recheck
	req.Recheck = func(c context.Context) (bool, error) {
		delivered.Lock()
		defer delivered.Unlock()
		if arrived {
			return true, nil
		}
		return inner(c)
	}
	run := set.ForRun(ResidentRunInfo{Language: "go"})
	done := make(chan int, 1)
	go func() {
		waits := 0
		for { // the shape of awaitSuspendOrPark: a wake means "poll the store"; if nothing is there, ask to be held again
			waits++
			out := run.Wait(ctx, req)
			if out != parkWoke {
				done <- -waits
				return
			}
			delivered.Lock()
			got := arrived
			delivered.Unlock()
			if got {
				done <- waits
				return
			}
		}
	}()
	waitTimers(t, clock, 2)
	for i := 0; i < 4; i++ {
		clock.fireShorterThan(time.Second)
		waitTimers(t, clock, 3+i)
	}
	delivered.Lock()
	arrived = true
	delivered.Unlock()
	clock.fireShorterThan(time.Second)
	select {
	case waits := <-done:
		if waits != 1 {
			t.Errorf("the call's loop had to ask to be held %d times, want 1: a no-op recheck returned a wake", waits)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never ended")
	}
	if restarts != 1 {
		t.Errorf("the limits segment restarted %d times across four no-op rechecks, want exactly 1 (at the end of the hold)", restarts)
	}
}

func TestARecheckThatCannotTellEndsTheHoldAsARefusalAndGivesTheChargeBack(t *testing.T) {
	m := newReplayMetrics(t)
	set, clock := signalSet(t, func(c *ResidencyConfig) { c.Metrics = m })
	rc := &scriptedRecheck{answers: []recheckAnswer{{}, {err: errors.New("store is down")}}}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, rc))
	waitTimers(t, clock, 2)
	clock.fireShorterThan(time.Second)
	mustOutcome(t, out, parkRefused, "a recheck that could not read the store")
	if n := residentCounter(t, m, "cleat_resident_rechecks_total", `result="error"`); n != 1 {
		t.Errorf("counted %d error recheck(s), want 1", n)
	}
	if n := residentCounter(t, m, "cleat_resident_parks_total", `outcome="recheck_failed"`); n != 1 {
		t.Errorf("the hold was counted %d time(s) as ending recheck_failed, want 1", n)
	}
	if n := chargedBytes(set); n != 0 {
		t.Errorf("the set is still charged %d bytes after the hold ended", n)
	}
	if n := set.Len(); n != 0 {
		t.Errorf("%d runs still held", n)
	}
}

func TestADeadlineEndsAHeldSignalWaitAndRechecksUpToThenAreCounted(t *testing.T) {
	m := newReplayMetrics(t)
	set, clock := signalSet(t, func(c *ResidencyConfig) { c.Metrics = m })
	rc := &scriptedRecheck{}
	out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, rc))
	waitTimers(t, clock, 2)
	clock.fireShorterThan(time.Second)
	waitTimers(t, clock, 3)
	clock.fireAll() // the deadline, and a tick
	mustOutcome(t, out, parkDeadline, "the deadline of a signal wait")
	if rc.count() < 2 {
		t.Errorf("the recheck ran %d times before the deadline, want at least 2 (admission and a tick)", rc.count())
	}
	if n := residentCounter(t, m, "cleat_resident_parks_total", `outcome="deadline"`); n != 1 {
		t.Errorf("the hold was counted %d time(s) as ending at its deadline, want 1", n)
	}
}

// D2: the existing window rule applies to a signal wait unchanged. A wait due beyond the window is refused, so a long approval wait is NOT held (polling it to save one replay would be
// a net loss), and one inside the window is.
func TestTheWindowRuleAppliesToASignalWaitUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name        string
		remainingMs int64
		held        bool
	}{{"due inside the window", 1500, true}, {"due beyond the window", 5000, false}} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReplayMetrics(t)
			set, clock := signalSet(t, func(c *ResidencyConfig) {
				c.Metrics = m
				c.WindowFloor = 2 * time.Second
				c.WindowCap = 2 * time.Second
			})
			out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(tc.remainingMs, &scriptedRecheck{}))
			if tc.held {
				waitTimers(t, clock, 2)
				clock.fireAll()
				mustOutcome(t, out, parkDeadline, "a signal wait inside the window")
				return
			}
			mustOutcome(t, out, parkRefused, "a signal wait beyond the window")
			if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="window"`); n != 1 {
				t.Errorf("counted %d window refusal(s), want 1", n)
			}
		})
	}
}

func TestAHeldSignalWaitEndsOnCancelAndOnEvictionAndGivesTheChargeBack(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		set, clock := signalSet(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		out := park(set.ForRun(ResidentRunInfo{Language: "go"}), ctx, signalReq(5000, &scriptedRecheck{}))
		waitTimers(t, clock, 2)
		cancel()
		mustOutcome(t, out, parkRefused, "a cancelled signal hold")
		if n := chargedBytes(set); n != 0 {
			t.Errorf("charged %d bytes after a cancel", n)
		}
	})
	t.Run("eviction", func(t *testing.T) {
		m := newReplayMetrics(t)
		set, clock := signalSet(t, func(c *ResidencyConfig) { c.Metrics = m })
		out := park(set.ForRun(ResidentRunInfo{Language: "go"}), context.Background(), signalReq(5000, &scriptedRecheck{}))
		waitTimers(t, clock, 2)
		set.EvictAll(ResidentEvictedShutdown)
		mustOutcome(t, out, parkRefused, "an evicted signal hold")
		if n := residentCounter(t, m, "cleat_resident_evictions_total", `reason="shutdown"`); n != 1 {
			t.Errorf("counted %d shutdown eviction(s), want 1", n)
		}
		if n := chargedBytes(set); n != 0 {
			t.Errorf("charged %d bytes after an eviction", n)
		}
	})
}

func TestAResidencyConfigWithANegativeRecheckIntervalIsRefusedAtBoot(t *testing.T) {
	cfg := DefaultResidencyConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("PRECONDITION FAILED: the default config is refused: %v", err)
	}
	if cfg.RecheckInterval != 500*time.Millisecond {
		t.Errorf("the default recheck interval is %s, want 500ms (the worker's --poll default, so a held run is never later than the same run with residency off)", cfg.RecheckInterval)
	}
	cfg.RecheckInterval = -time.Second
	if err := cfg.Validate(); err == nil {
		t.Error("a negative recheck interval was accepted")
	}
}
