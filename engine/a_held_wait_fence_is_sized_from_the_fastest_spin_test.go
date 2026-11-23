package engine

import (
	"testing"
	"time"
)

// heldWaitFenceFactor is how many spins' worth of fence the held-wait timing rows give a guest: more than one, so one spin fits, and less than two, so two do not.
const heldWaitFenceFactor = 1.7

// fenceFromSpins sizes the execution fence for the held-wait timing rows from SEVERAL timed spins and returns the fastest of them with the fence.
//
// cleat#4402. The rows used to size the fence from ONE spin, and the row needs two spins in one segment to EXCEED it, which holds only while a later spin takes at least
// 1/1.7 = 0.59 of that one. On the 2026-11-23 nightly it did not, eight attempts in a row: two spins took 0.75 to 0.90 of the fence, because the later spins ran FASTER than
// the one the fence was sized from (a cold first measurement), and the row ended "PRECONDITION FAILED ... too loaded", which was the opposite of what the numbers said. The fastest of
// several spins is the cleanest estimate of what one spin costs here, and a later spin is almost never faster than it, so two spins exceed 1.7 of it. The fence is still never
// loosened to make a check pass: this only stops it being sized from a measurement that was slow for a reason that is gone by the time the check runs.
func fenceFromSpins(spins []time.Duration) (fastest, fence time.Duration) {
	for i, s := range spins {
		if i == 0 || s < fastest {
			fastest = s
		}
	}
	return fastest, time.Duration(float64(fastest) * heldWaitFenceFactor)
}

func TestTheHeldWaitFenceIsSizedFromTheFastestSpinNotTheFirst(t *testing.T) {
	// The 2026-11-23 shape: a cold first spin, then spins about a third faster. Sized from the first, two later spins (2 x 200 = 400 ms) do NOT exceed the fence, and the
	// row cannot tell a restart from none. Sized from the fastest, they do.
	spins := []time.Duration{300 * time.Millisecond, 210 * time.Millisecond, 205 * time.Millisecond, 200 * time.Millisecond}
	steady := 200 * time.Millisecond

	fastest, fence := fenceFromSpins(spins)
	if fastest != 200*time.Millisecond {
		t.Fatalf("fastest = %s, want 200ms", fastest)
	}
	if fence != 340*time.Millisecond {
		t.Fatalf("fence = %s, want 1.7 x 200ms = 340ms", fence)
	}
	if one, two := steady, 2*steady; !(one < fence && two > fence) {
		t.Errorf("with spins of %s: one spin %s must fit the fence %s and two (%s) must exceed it", steady, one, fence, two)
	}

	// KNOWN POSITIVE: the OLD rule, a fence of 1.7 x the FIRST spin, leaves two steady spins UNDER it. If this stops failing the old rule, the case above is not the case
	// that was observed.
	oldFence := time.Duration(float64(spins[0]) * heldWaitFenceFactor)
	if two := 2 * steady; two > oldFence {
		t.Errorf("two steady spins (%s) exceed the OLD fence %s: this test no longer reproduces the failure the new rule exists for", two, oldFence)
	}

	// Order does not matter, and one spin is its own fastest.
	if f, _ := fenceFromSpins([]time.Duration{200 * time.Millisecond, 300 * time.Millisecond}); f != 200*time.Millisecond {
		t.Errorf("fastest of {200, 300} = %s, want 200ms", f)
	}
	if f, fe := fenceFromSpins([]time.Duration{250 * time.Millisecond}); f != 250*time.Millisecond || fe != 425*time.Millisecond {
		t.Errorf("a single spin gave fastest %s fence %s, want 250ms and 425ms", f, fe)
	}
	// No samples is not a fence: the caller must not size one.
	if f, fe := fenceFromSpins(nil); f != 0 || fe != 0 {
		t.Errorf("no spins gave fastest %s fence %s, want zero values", f, fe)
	}
}
