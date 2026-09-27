package engine

import (
	"context"
	"testing"
	"time"
)

// A stand-in session with a durable clock that JUMPS, which is what a workflow
// doing DurableSleepMs(1h) produces, and the case that must not reach the
// monotonic clock.
type jumpyHandler struct {
	HostHandler
	nowMs  int64
	random int64
}

func (j *jumpyHandler) Now(context.Context) int64 {
	j.nowMs += 3_600_000 // an hour per read
	return j.nowMs
}

func (j *jumpyHandler) Random(context.Context) int64 {
	j.random++
	return j.random * 0x0123456789ABCDEF
}

// TestTheMonotonicClockDoesNotFollowTheDurableClock is the whole design in one
// assertion. Feeding the durable clock to CLOCK_MONOTONIC is the obvious
// implementation and it is measured to kill guests: Go's GC pacer reacts to the
// jump, runs a fifth as many cycles, and the heap reaches 8x
// DefaultMemoryLimitPages. cleat#1300.
func TestTheMonotonicClockDoesNotFollowTheDurableClock(t *testing.T) {
	h := &jumpyHandler{}
	ctx := withHandler(context.Background(), h)

	// THIS CALLS nextMonotonicNs, THE SHIPPED RULE. It used to keep its own
	// copy -- `monotonicNs += wasiMonotonicStepNs` -- written out in the test
	// body, so it asserted against a reimplementation and would have passed
	// unchanged whatever the real clock did. cleat#1300 changed that clock from
	// a per-read step to real elapsed time and neither of these tests noticed.
	start := time.Now()
	var monotonicNs int64
	mono := func() int64 {
		monotonicNs = nextMonotonicNs(monotonicNs, start, time.Now())
		return monotonicNs
	}
	wall := func() int64 { return h.Now(ctx) }

	firstWall, firstMono := wall(), mono()
	secondWall, secondMono := wall(), mono()

	wallDelta := secondWall - firstWall
	monoDelta := secondMono - firstMono

	if wallDelta != 3_600_000 {
		t.Fatalf("fixture is wrong: the durable clock moved %dms, expected an hour", wallDelta)
	}

	// The invariant is that the two domains are SEPARATE, and it is the one
	// that survived the redesign. The durable clock may jump an hour because a
	// workflow slept an hour; the monotonic clock must not, because the GC
	// pacer reacts to the jump rather than to the value -- 3 collections
	// instead of 17 and a 256MB heap against a 32MB limit.
	//
	// THE OLD PER-READ BAND IS GONE ON PURPOSE. It required each read to move
	// between 1us and 100ms, with the floor justified by "below ~1us,
	// poll_oneoff needs so many crossings to clear a timer that it reads as a
	// hang". That reasoning assumed poll_oneoff DOES NOT SLEEP, which was true
	// only on wazero's default FakeNanosleep. It does sleep on wasmtime, and
	// wazero now has a real Nanosleep, so a timer is cleared by waiting rather
	// than by crossings and two reads a nanosecond apart are correct.
	if monoDelta <= 0 {
		t.Errorf("the monotonic clock moved %dns between reads; it must be strictly "+
			"increasing, because the Go runtime throws \"fatal error: nanotime returning "+
			"zero\" and a clock that repeats a value can make a timer never expire", monoDelta)
	}
	if monoDelta >= wallDelta*1_000_000 {
		t.Errorf("the monotonic clock moved as far as the durable clock (%dns vs %dns); "+
			"the two domains are not actually separate. CLOCK_REALTIME is the durable, "+
			"replayable one; CLOCK_MONOTONIC exists so timers terminate.",
			monoDelta, wallDelta*1_000_000)
	}
}

// The monotonic clock tracks REAL elapsed time, which is the fix for
// cleat#1300's 125x sleep amplification.
//
// A clock advancing a fixed amount per read made a guest's 500ms time.Sleep
// take 62.5 seconds on wasmtime: Go computes poll_oneoff's relative timeout as
// deadline - nanotime(), wasmtime really blocks for it, and the guest then saw
// only one step of progress and slept again.
//
// Asserted as a RATIO against a real sleep rather than against a constant. A
// test that checked "the clock advanced by roughly the elapsed time" using its
// own elapsed measurement would agree with any implementation that reads a
// clock at all.
func TestTheMonotonicClockTracksRealElapsedTime(t *testing.T) {
	start := time.Now()
	var ns int64
	ns = nextMonotonicNs(ns, start, time.Now())

	const pause = 50 * time.Millisecond
	time.Sleep(pause)
	after := nextMonotonicNs(ns, start, time.Now())

	advanced := time.Duration(after - ns)
	if advanced < pause/2 || advanced > 4*pause {
		t.Errorf("across a real %v pause the monotonic clock advanced %v, which is not "+
			"tracking real time.\n\n"+
			"If this clock advances a FIXED AMOUNT PER READ again, a guest's time.Sleep is "+
			"multiplied on any backend whose poll_oneoff really blocks -- measured at 125x on "+
			"wasmtime, a 500ms sleep taking 62.5 seconds. cleat#1300.", pause, advanced)
	}
}

// TestTheMonotonicClockIsNeverZero pins the other measured failure: the Go
// runtime throws "fatal error: nanotime returning zero" before user code runs.
//
// It also pins STRICTLY increasing, which is what the floor is for now that the
// clock reads a real clock: two reads inside the same nanosecond are ordinary
// on a fast machine, and a repeated value can make a timer never expire.
func TestTheMonotonicClockIsNeverZero(t *testing.T) {
	if wasiMonotonicFloorNs <= 0 {
		t.Fatalf("wasiMonotonicFloorNs is %d; a monotonic clock that can repeat a value or "+
			"return zero kills the Go runtime at boot", wasiMonotonicFloorNs)
	}

	// Same instant for every read, which is the case the floor exists for: no
	// real time passes between them, so only the floor keeps them apart.
	frozen := time.Now()
	var ns int64
	for i := 0; i < 3; i++ {
		prev := ns
		ns = nextMonotonicNs(ns, frozen, frozen)
		if ns <= 0 {
			t.Fatalf("read %d produced %d; the clock must never be zero or negative", i, ns)
		}
		if ns <= prev {
			t.Fatalf("read %d produced %d after %d; the clock must be strictly increasing "+
				"even when no real time has passed", i, ns, prev)
		}
	}
}
