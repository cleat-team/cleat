package engine

import (
	"slices"
	"testing"
	"time"
)

// The held-wait timing rows need a fence that ONE spin fits and TWO spins in one segment exceed. This file holds the arithmetic of choosing it, with no clock, so it runs in
// Tier 1 and the timing row (a_held_wait_restarts_the_fence_on_real_wasmtime_timing_test.go, build tag `timing`) stays a thin driver around it.
//
// cleat#4402 is why it is shaped this way. The row used to size the fence as 1.7 x one measured spin, which holds only while two spins cost at least 1.7 x one. Measured 2026-11-26
// on one machine, one backend, twice/once over the same work: 1.57, 1.89, 1.75, 1.80 in one run (the spin got FASTER across the run: once 244, 213, 193, 179 ms), and 1.98, 1.98, 2.01 in
// the next, with the fixed part of a run about zero in both. So the ratio is not a constant of the guest, it is whatever the machine's speed did between the measurement and the check,
// and a fence at 1.7 sat inside that spread. "The fastest of four spins" (#4406) cannot help when all four are measured in the slow phase. What does: calibrate only once the speed has
// SETTLED, measure one spin and two spins back to back, and put the fence between what was measured, so the fence is a statement about numbers the row has seen and not about a ratio.

const (
	// heldWaitFenceMarginFraction is how much of one spin must separate the fence's whole effective range from the slowest one-spin run and from the fastest two-spin run.
	heldWaitFenceMarginFraction = 0.20
	heldWaitFenceMarginFloor    = 10 * time.Millisecond
	// heldWaitSettleTolerance is how close the last settleWindow one-spin timings must be (slowest / fastest) before the machine's speed is taken as steady.
	heldWaitSettleTolerance = 1.15
	heldWaitSettleWindow    = 3
)

// fenceBetweenOneAndTwo returns a fence, as a whole number of epoch ticks, whose ENTIRE effective range lies between the MEDIAN one-spin run and the MEDIAN two-spin run, and ok=false when
// no such fence has a margin on both sides.
//
// Why a whole number of ticks and a range. The fence is enforced as an epoch deadline of floor(fence/tick) ticks (hostBudget.setDeadline), armed at an arbitrary point in the ticker's cycle,
// so it fires after somewhere in ((k-1)*tick, k*tick] with k = floor(fence/tick), not at the fence. A fence of 242 ms with a 50 ms tick is k = 4: it stops the guest between 150 and 200 ms,
// which is how the row's control died at about 170 ms on a spin of 179 to 190. A fence that sits in the MIDDLE of two measurements is therefore not between them. This picks the k whose
// whole range has the most room on its tighter side, and refuses unless that room is at least the margin.
//
// Why medians. On a shared machine the samples TREND (459, 333, 293 ms for the same spin, measured 2026-11-26 as another build finished): the slowest one-spin and the fastest two-spin come
// from different moments, and comparing them closes a gap that was there at every moment. The median is not moved by a trend or an outlier, and nothing here relies on it for safety: the row
// checks the fence it sets (one spin must fit it, two must not) immediately, and recalibrates when either fails.
func fenceBetweenOneAndTwo(once, twice []time.Duration, tick time.Duration) (fence time.Duration, ok bool) {
	if len(once) == 0 || len(twice) == 0 || tick <= 0 {
		return 0, false
	}
	oneRef, twoRef := median(once), median(twice)
	if oneRef <= 0 {
		return 0, false
	}
	margin := max(time.Duration(heldWaitFenceMarginFraction*float64(oneRef)), heldWaitFenceMarginFloor)
	best, bestSlack := time.Duration(0), time.Duration(-1)
	for k := time.Duration(2); k*tick < twoRef+tick; k++ {
		lo, hi := (k-1)*tick, k*tick // the fence fires somewhere in (lo, hi]
		if slack := min(lo-oneRef, twoRef-hi); slack > bestSlack {
			best, bestSlack = hi, slack
		}
	}
	if bestSlack < margin {
		return 0, false
	}
	// HALF A TICK ABOVE the boundary, not on it. The guest's budget is re-armed at every budgeted host call as floor((remaining - charged)/tick) ticks, and the guest makes about a dozen at
	// entry (cleat-review2 measured 749.8 ms left of a 750 ms fence before the first spin): a fence at an exact multiple of the tick loses a whole tick to ANY charge, so the first segment
	// would fire in ((k-2)*tick, (k-1)*tick] and the margin on the one-spin side would be one tick short. Half a tick absorbs any entry charge under 25 ms and leaves the range at
	// ((k-1)*tick, k*tick], which is what the selection above assumed.
	return best + tick/2, true
}

// median is the upper median: for an even count, the larger of the two middle values.
func median(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)/2]
}

// speedSettled reports whether the last heldWaitSettleWindow timings agree to within heldWaitSettleTolerance. It is false for fewer samples than the window, so a caller cannot settle
// on a single measurement, which is what the row used to do.
func speedSettled(samples []time.Duration) bool {
	if len(samples) < heldWaitSettleWindow {
		return false
	}
	last := samples[len(samples)-heldWaitSettleWindow:]
	lo, hi := last[0], last[0]
	for _, s := range last {
		lo, hi = min(lo, s), max(hi, s)
	}
	return lo > 0 && float64(hi) <= heldWaitSettleTolerance*float64(lo)
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// firesBefore reports, for EVERY phase of the epoch ticker, whether a fence of this value stops a run of the given length once the guest has been charged `charge` before its first spin
// (the budgeted host calls at entry): the budget is then floor((fence-charge)/tick) ticks armed at phase p in [0, tick), so it fires after k*tick - p.
func firesBefore(fence, charge, tick, runLength time.Duration) (always, sometimes bool) {
	k := (fence - charge) / tick
	if k < 1 {
		k = 1
	}
	always, sometimes = true, false
	for p := time.Duration(0); p < tick; p += time.Millisecond {
		fires := k*tick - p
		if fires < runLength {
			sometimes = true
		} else {
			always = false
		}
	}
	return always, sometimes
}

func TestTheHeldWaitFenceSitsBetweenWhatWasMeasured(t *testing.T) {
	const tick = 50 * time.Millisecond
	// KNOWN POSITIVE 1: the fence that FAILED on 2026-11-26, from a pair measured on the row's own machine: one spin at most 179 ms, two at least 305 ms, and the midpoint rule's fence of 242 ms.
	// That fence is 4 ticks, which fires between 150 and 200 ms: it kills a 179 ms spin for part of the ticker's cycle. If this stops being true the case below is not the one that was observed.
	if _, sometimes := firesBefore(242*time.Millisecond, 0, tick, 179*time.Millisecond); !sometimes {
		t.Fatalf("a 242 ms fence no longer kills a 179 ms spin for any phase: this does not reproduce the failure the tick-aware rule exists for")
	}
	// KNOWN POSITIVE 2: the OLD rule, 1.7 x one spin, on the pair measured while the machine was still speeding up (one 244 ms, two 382 ms): 415 ms, which two spins do not reliably exceed.
	if always, _ := firesBefore(ms(414), 0, tick, ms(382)); always {
		t.Fatalf("the old 1.7 x fence stops a 382 ms run for every phase: this does not reproduce the failure either")
	}
	for _, c := range []struct{ once, twice []time.Duration }{
		{[]time.Duration{ms(500), ms(495), ms(505)}, []time.Duration{ms(980), ms(1000), ms(990)}},
		{[]time.Duration{ms(490), ms(510)}, []time.Duration{ms(1020), ms(1000)}},
		{[]time.Duration{ms(600), ms(590)}, []time.Duration{ms(1150), ms(1180)}},
		{[]time.Duration{ms(289), ms(288), ms(284)}, []time.Duration{ms(573), ms(571), ms(570)}}, // the steady 2026-11-26 run, at the OLD spin length
	} {
		fence, ok := fenceBetweenOneAndTwo(c.once, c.twice, tick)
		if !ok {
			t.Errorf("once %v twice %v: no fence, want one", c.once, c.twice)
			continue
		}
		if fence%tick != tick/2 {
			t.Errorf("once %v twice %v: fence %s is not half a tick above a whole number of ticks", c.once, c.twice, fence)
		}
		// With NO charge and with the charge cleat-review2 measured at entry (about 0.2 ms) and a generous 20 ms: the one-spin runs must never be killed and the two-spin runs always.
		for _, charge := range []time.Duration{0, time.Millisecond, 20 * time.Millisecond} {
			for _, o := range c.once {
				if _, sometimes := firesBefore(fence, charge, tick, o); sometimes {
					t.Errorf("once %v twice %v: a one-spin run of %s is killed by the fence %s for some ticker phase after an entry charge of %s", c.once, c.twice, o, fence, charge)
				}
			}
			for _, w := range c.twice {
				if always, _ := firesBefore(fence, charge, tick, w); !always {
					t.Errorf("once %v twice %v: a two-spin run of %s is NOT always killed by the fence %s after an entry charge of %s", c.once, c.twice, w, fence, charge)
				}
			}
		}
	}
}

// KNOWN POSITIVE for the entry charge: a fence at an EXACT multiple of the tick (the boundary value the first version returned) loses a whole tick to a 1 ms charge. If this stops being
// true, the half-tick above is no longer what protects the fence, and the assertion above is not testing what it claims.
func TestAFenceOnAWholeTickLosesATickToAnyEntryCharge(t *testing.T) {
	const tick = 50 * time.Millisecond
	onTheBoundary := 750 * time.Millisecond
	if got, want := (onTheBoundary-time.Millisecond)/tick, onTheBoundary/tick-1; got != want {
		t.Fatalf("a 750 ms fence charged 1 ms arms %d ticks, want %d: the boundary no longer loses a tick", got, want)
	}
	// A run of 690 ms: with no charge a 750 ms fence is 15 ticks and fires in (700, 750], never before 690; after a 1 ms charge it is 14 ticks and fires in (650, 700], before 690 for most phases.
	if _, sometimes := firesBefore(onTheBoundary, 0, tick, 690*time.Millisecond); sometimes {
		t.Errorf("a 690 ms run is killed by an UNCHARGED 750 ms fence for some phase: the control for the case below is wrong")
	}
	if _, sometimes := firesBefore(onTheBoundary, time.Millisecond, tick, 690*time.Millisecond); !sometimes {
		t.Errorf("a 690 ms run is not killed for any phase by a 750 ms fence after a 1 ms charge: it should fire in (650, 700]")
	}
	// ... and the fence the helper returns for the same pair does not.
	fence, ok := fenceBetweenOneAndTwo([]time.Duration{ms(500)}, []time.Duration{ms(1000)}, tick)
	if !ok {
		t.Fatal("a 500/1000 ms pair was refused")
	}
	if got, want := (fence-time.Millisecond)/tick, fence/tick; got != want {
		t.Errorf("the fence %s arms %d ticks after a 1 ms charge and %d with none: it is on a boundary", fence, got, want)
	}
}

func TestTheHeldWaitFenceIsSizedFromTheMedianSoATrendOrAnOutlierDoesNotCloseTheGap(t *testing.T) {
	const tick = 50 * time.Millisecond
	// The 2026-11-26 trend, as measured: the same spin took 459, 333, 293 ms and two spins 797, 587, 529 ms, in that order. The slowest one-spin (459) against the fastest two-spin (529) is
	// 1.15x and would be refused; the medians (333 and 587) are 1.76x apart and have room for a fence. Every later pair on that machine was about 290 and 530, which this fence fits.
	once, twice := []time.Duration{ms(459), ms(333), ms(293)}, []time.Duration{ms(797), ms(587), ms(529)}
	fence, ok := fenceBetweenOneAndTwo(once, twice, tick)
	if !ok {
		t.Fatalf("the trend %v / %v was refused; its medians 333 and 587 have room", once, twice)
	}
	for _, later := range []time.Duration{ms(290), ms(333)} {
		if _, sometimes := firesBefore(fence, 0, tick, later); sometimes {
			t.Errorf("a later one-spin run of %s is killed by the fence %s for some phase", later, fence)
		}
	}
	for _, later := range []time.Duration{ms(529), ms(587)} {
		if always, _ := firesBefore(fence, 0, tick, later); !always {
			t.Errorf("a later two-spin run of %s is not always killed by the fence %s", later, fence)
		}
	}
	// One wild outlier among the one-spin samples does not move the median.
	if _, ok := fenceBetweenOneAndTwo([]time.Duration{ms(500), ms(495), ms(2000)}, []time.Duration{ms(980), ms(1000), ms(990)}, tick); !ok {
		t.Errorf("one outlier of 2000 ms among 500, 495 refused a pair whose medians are 500 and 990")
	}
}

func TestTheHeldWaitFenceIsRefusedWhenOneAndTwoSpinsAreTooClose(t *testing.T) {
	const tick = 50 * time.Millisecond
	// Two spins costing 1.1 x one, or 1.36 x one (295 and 400 ms, the 2026-11-23 runner: 105 ms apart, less than a tick plus two margins): the honest answer is UNMEASURED, not a fence.
	for _, c := range []struct {
		name        string
		once, twice []time.Duration
	}{
		{"1.1x", []time.Duration{ms(300), ms(310)}, []time.Duration{ms(340), ms(350)}},
		{"1.4x at the new spin length: 200 ms apart cannot hold a tick and two 100 ms margins", []time.Duration{ms(500), ms(495)}, []time.Duration{ms(700), ms(710)}},
		{"the 2026-11-26 pair while the machine was speeding up (a 242 ms fence kills a 179 ms spin)", []time.Duration{ms(179), ms(172)}, []time.Duration{ms(305), ms(311)}},
		{"the 2026-11-26 pair, 244 and 382 ms", []time.Duration{ms(244), ms(238)}, []time.Duration{ms(382), ms(390)}},
		{"1.36x, the 2026-11-23 runner", []time.Duration{ms(295), ms(290)}, []time.Duration{ms(400), ms(404)}},
		{"a gap that is closed at the median, not only in one sample", []time.Duration{ms(500), ms(520), ms(540)}, []time.Duration{ms(700), ms(720), ms(1000)}},
	} {
		if f, ok := fenceBetweenOneAndTwo(c.once, c.twice, tick); ok {
			t.Errorf("%s: once %v twice %v gave a fence %s, want none", c.name, c.once, c.twice, f)
		}
	}
	// Nothing measured is not a fence, and a zero tick is not a tick.
	if f, ok := fenceBetweenOneAndTwo(nil, []time.Duration{ms(500)}, tick); ok || f != 0 {
		t.Errorf("no one-spin samples gave fence %s ok=%v", f, ok)
	}
	if f, ok := fenceBetweenOneAndTwo([]time.Duration{ms(250)}, nil, tick); ok || f != 0 {
		t.Errorf("no two-spin samples gave fence %s ok=%v", f, ok)
	}
	if f, ok := fenceBetweenOneAndTwo([]time.Duration{ms(250)}, []time.Duration{ms(500)}, 0); ok || f != 0 {
		t.Errorf("a zero tick gave fence %s ok=%v", f, ok)
	}
}

func TestTheHeldWaitSpeedIsNotSettledOnOneMeasurementOrASlope(t *testing.T) {
	// The 2026-11-26 slope: 244, 213, 193, 179 ms. Every window of three is still falling by more than 15 percent, so the row must not calibrate on it.
	if speedSettled([]time.Duration{ms(244), ms(213), ms(193)}) || speedSettled([]time.Duration{ms(213), ms(193), ms(179)}) {
		t.Errorf("a falling sequence counted as settled")
	}
	// The same machine once steady: 289, 288, 284.
	if !speedSettled([]time.Duration{ms(289), ms(288), ms(284)}) {
		t.Errorf("a steady sequence did not count as settled")
	}
	// Only the LAST window counts: a slow start followed by three steady samples is settled.
	if !speedSettled([]time.Duration{ms(500), ms(300), ms(290), ms(288), ms(287)}) {
		t.Errorf("a slow start followed by a steady window did not count as settled")
	}
	// Fewer samples than the window never settle, and a zero timing never settles.
	if speedSettled([]time.Duration{ms(300), ms(300)}) || speedSettled(nil) || speedSettled([]time.Duration{0, 0, 0}) {
		t.Errorf("too few samples, or zero timings, counted as settled")
	}
}
