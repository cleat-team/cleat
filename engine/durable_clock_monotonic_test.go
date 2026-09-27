package engine

import (
	"testing"
	"time"
)

// cleat#944: two clock domains fed Now(). The seed is the workflow row's
// created_at (the DATABASE's clock); the first recorded event's timestamp is
// the WORKER's time.Now(). Nothing reconciled them, so the first durable event
// stepped by the offset between two machines' clocks -- backwards in 6 of 8
// PostgreSQL runs (worst -26ms) and 4 of 5 MySQL runs (worst -118ms).
//
// THESE TESTS ASSERT THE MECHANISM, NOT THE SYMPTOM, and that is deliberate.
// The issue records a port test that failed on CI by reporting "cleat#944
// appears to be FIXED" when what it had measured was that the runner's database
// and worker clocks agreed closely enough to hide it. A test that needs two
// disagreeing clocks to fail is a test of the environment. These construct the
// disagreement directly, so they fail on any machine.

// TestTheDurableClockNeverStepsBackwards is the property, stated directly.
func TestTheDurableClockNeverStepsBackwards(t *testing.T) {
	s := &execSession{engine: NewEngine(nil, &mockCaller{})}

	// The seed is one hour ahead of this worker's clock -- the same shape as a
	// database whose clock runs ahead, with the offset made large enough that
	// no scheduling delay can be mistaken for it.
	seed := time.Now().Add(time.Hour).UnixMilli()
	s.nowMs = seed

	s.recordEvent(EventRecord{EventType: EventTypeCall, Step: 0})

	if s.nowMs < seed {
		t.Errorf("the durable clock went backwards: seed %d -> %d (%dms)",
			seed, s.nowMs, s.nowMs-seed)
	}
}

// TestTheClampedTimestampIsWhatGoesIntoHistory pins why the clamp lives in
// recordEvent rather than in Now().
//
// Replay sets s.nowMs from the recorded timestamp. If history held the
// un-clamped worker value while the original run reported the clamped one, the
// two executions would disagree about Now() -- trading a backwards step for a
// replay divergence, which is worse.
//
// NOTE WHAT THIS CANNOT CATCH, measured rather than assumed: it passes with the
// clamp removed, because recordEvent assigns s.nowMs from the same rec it
// appends, so the two agree either way. It fails only if someone moves the
// adjustment to the read side, which is exactly the tempting refactor -- so it
// is kept, labelled, and not counted as evidence that the clamp works.
// TestTheDurableClockNeverStepsBackwards is that evidence.
func TestTheClampedTimestampIsWhatGoesIntoHistory(t *testing.T) {
	s := &execSession{engine: NewEngine(nil, &mockCaller{})}
	seed := time.Now().Add(time.Hour).UnixMilli()
	s.nowMs = seed

	s.recordEvent(EventRecord{EventType: EventTypeCall, Step: 0})

	if len(s.history) != 1 {
		t.Fatalf("expected one recorded event, got %d", len(s.history))
	}
	if got := s.history[0].TimestampMs; got != s.nowMs {
		t.Errorf("history holds %d but the session clock reads %d; a replay reading "+
			"history would disagree with the original run", got, s.nowMs)
	}
}

// TestTheClampIsAFloorAndNotARewrite. Once the worker clock passes the seed --
// tens of milliseconds, in the deployments this was measured on -- the branch
// must stop firing and timestamps must be the worker's own. A clamp that kept
// pinning the clock to the seed would freeze durable time.
func TestTheClampIsAFloorAndNotARewrite(t *testing.T) {
	s := &execSession{engine: NewEngine(nil, &mockCaller{})}

	// Seed an hour in the PAST: the worker clock is ahead, which is the other
	// direction of the same offset and must be left alone.
	s.nowMs = time.Now().Add(-time.Hour).UnixMilli()

	before := time.Now().UnixMilli()
	s.recordEvent(EventRecord{EventType: EventTypeCall, Step: 0})
	after := time.Now().UnixMilli()

	if s.nowMs < before || s.nowMs > after {
		t.Errorf("timestamp %d is not the worker clock (expected between %d and %d); "+
			"the clamp rewrote a value it should have passed through",
			s.nowMs, before, after)
	}
}

// TestAnExplicitTimestampAtOrAheadOfTheClockIsUntouched pins that DurableSleep
// is unaffected. Sleep sets TimestampMs to anchor+duration, which is already
// at or ahead of the session clock, so the clamp must be a no-op for it --
// otherwise a sleep's virtual advance would be silently discarded.
func TestAnExplicitTimestampAtOrAheadOfTheClockIsUntouched(t *testing.T) {
	s := &execSession{engine: NewEngine(nil, &mockCaller{})}
	s.nowMs = 1_000_000

	const ahead = 1_000_200 // as if DurableSleepMs(200)
	s.recordEvent(EventRecord{EventType: EventTypeSleep, Step: 0, TimestampMs: ahead})

	if s.nowMs != ahead {
		t.Errorf("an explicit timestamp ahead of the clock became %d, want %d", s.nowMs, ahead)
	}
	if s.history[0].TimestampMs != ahead {
		t.Errorf("history holds %d, want %d", s.history[0].TimestampMs, ahead)
	}
}

// TestSuccessiveEventsAreNonDecreasing is the general form: whatever the two
// domains do, the sequence a workflow observes must never decrease.
func TestSuccessiveEventsAreNonDecreasing(t *testing.T) {
	s := &execSession{engine: NewEngine(nil, &mockCaller{})}
	s.nowMs = time.Now().Add(time.Hour).UnixMilli()

	// The seed is the FIRST observation, not the first recorded event. Omitting
	// it was a real defect in this test: the only backwards step is seed ->
	// first event, so a sequence that starts after the first event cannot see
	// it, and this test passed with the clamp removed. Everything after the
	// first event is worker-clock throughout and non-decreasing anyway.
	seen := []int64{s.nowMs}
	for i := 0; i < 8; i++ {
		s.recordEvent(EventRecord{EventType: EventTypeCall, Step: i})
		seen = append(seen, s.nowMs)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Errorf("event %d stepped backwards: %d -> %d (%dms)",
				i, seen[i-1], seen[i], seen[i]-seen[i-1])
		}
	}
}

// TestTheSeedStillComesFromCreatedAt guards the fix's boundary.
//
// The tempting repair for cleat#944 is to seed from the worker clock instead,
// which removes the second domain outright. It is wrong, and seedNowMs's own
// doc comment records why: created_at is the only one of the three candidates
// identical on the original run and the replay. Seeding from the wall clock or
// from the first event's timestamp made Now()-before-any-event differ between
// the two executions -- measured at 109ms, and fatal inside a SideEffect, which
// validates its recomputed value against history.
//
// So this fix must NOT touch the seed. If it ever does, that older defect comes
// back, and it is the more serious of the two.
func TestTheSeedStillComesFromCreatedAt(t *testing.T) {
	const createdAt = 1_700_000_000_000
	e := NewEngine(nil, &mockCaller{}, WithWorkflowStartTime(createdAt))

	// Even with history whose first event says otherwise, created_at wins.
	got := e.seedNowMs([]EventRecord{{TimestampMs: createdAt + 5_000}})
	if got != createdAt {
		t.Errorf("seedNowMs returned %d, want created_at %d. Seeding from anything "+
			"else reintroduces the replay divergence seedNowMs documents.", got, createdAt)
	}
}
