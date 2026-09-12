package engine

import (
	"context"
	"testing"
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

	// Drive the two sources the way a guest would: interleaved reads.
	wall := func() int64 {
		ms := h.Now(ctx)
		return ms
	}

	var monotonicNs int64
	mono := func() int64 {
		monotonicNs += wasiMonotonicStepNs
		return monotonicNs
	}

	firstWall, firstMono := wall(), mono()
	secondWall, secondMono := wall(), mono()

	wallDelta := secondWall - firstWall
	monoDelta := secondMono - firstMono

	if wallDelta != 3_600_000 {
		t.Fatalf("fixture is wrong: the durable clock moved %dms, expected an hour", wallDelta)
	}
	// LITERAL BOUNDS, not a comparison against wasiMonotonicStepNs. Asserting
	// monoDelta == wasiMonotonicStepNs compares the implementation against
	// itself and passes for any value of the constant, including one sourced
	// from the durable clock -- which is the exact mistake this test exists to
	// catch. Verified by sabotage: setting the constant to an hour left that
	// form of the assertion green.
	//
	// The band comes from the measurements on cleat#1300. Below ~1us per read,
	// poll_oneoff needs so many crossings to clear a timer that it reads as a
	// hang. At or above ~1s per read the GC pacer collapses to 3 cycles and the
	// heap reaches 8x the default limit.
	const minStepNs = 1_000       // 1us
	const maxStepNs = 100_000_000 // 100ms, two orders below the measured-bad 1s
	if monoDelta < minStepNs || monoDelta > maxStepNs {
		t.Errorf("the monotonic clock moved %dns between reads, outside the measured-safe band "+
			"[%d, %d]. Too small and poll_oneoff spins without terminating; too large and Go's "+
			"GC pacer runs 3 cycles instead of 17 and the heap reaches 256MB against "+
			"DefaultMemoryLimitPages of 32MB.", monoDelta, minStepNs, maxStepNs)
	}
	if monoDelta >= wallDelta*1_000_000 {
		t.Errorf("the monotonic clock moved as far as the durable clock (%dns vs %dns); "+
			"the two domains are not actually separate", monoDelta, wallDelta*1_000_000)
	}
}

// TestTheMonotonicClockIsNeverZero pins the other measured failure: the Go
// runtime throws "fatal error: nanotime returning zero" before user code runs.
func TestTheMonotonicClockIsNeverZero(t *testing.T) {
	if wasiMonotonicStepNs <= 0 {
		t.Fatalf("wasiMonotonicStepNs is %d; a monotonic clock that does not advance makes "+
			"poll_oneoff spin without terminating and a zero one kills the Go runtime at boot",
			wasiMonotonicStepNs)
	}
	var ns int64
	for i := 0; i < 3; i++ {
		prev := ns
		ns += wasiMonotonicStepNs
		if ns <= 0 {
			t.Fatalf("read %d produced %d; the clock must never be zero or negative", i, ns)
		}
		if ns <= prev {
			t.Fatalf("read %d went backwards: %d then %d", i, prev, ns)
		}
	}
}

// TestWasiEntropyIsReplayable: two runs of the same seeded session produce the
// same bytes, and a different session does not.
func TestWasiEntropyIsReplayable(t *testing.T) {
	read := func(h HostHandler, n int) []byte {
		d := &deterministicEntropy{h: h}
		buf := make([]byte, n)
		if _, err := d.Read(buf); err != nil {
			t.Fatal(err)
		}
		return buf
	}

	a := read(&jumpyHandler{}, 32)
	b := read(&jumpyHandler{}, 32)
	if string(a) != string(b) {
		t.Errorf("two identically seeded sessions produced different entropy:\n a=%x\n b=%x\n"+
			"random_get must reproduce on replay or a guest reaching entropy through WASI "+
			"diverges silently", a, b)
	}

	// The negative control: entropy that is merely CONSTANT would pass the test
	// above. It must actually vary within a run.
	if allSame(a) {
		t.Errorf("the entropy stream is a single repeated byte (%x); the equality check above "+
			"would pass for any constant, so this is what makes it mean something", a)
	}

	c := read(&jumpyHandler{random: 99}, 32)
	if string(a) == string(c) {
		t.Errorf("a session at a different point in its sequence produced identical entropy; " +
			"the stream does not depend on the seed at all")
	}
}

func allSame(b []byte) bool {
	for i := range b {
		if b[i] != b[0] {
			return false
		}
	}
	return true
}

// TestAWasiHandlerlessConfigDoesNotPanic: cleat dev and unit tests instantiate
// without a session in the context. handlerFromContextOrNil exists for exactly
// this, and the closures must tolerate the nil.
func TestAWasiHandlerlessConfigDoesNotPanic(t *testing.T) {
	d := &deterministicEntropy{h: nil}
	buf := make([]byte, 16)
	if _, err := d.Read(buf); err != nil {
		t.Fatalf("entropy with no session returned an error: %v", err)
	}
}
