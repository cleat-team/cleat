package main

import (
	"testing"
	"time"
)

// A fake clock, because every assertion here is about WHEN a change is applied
// and a test that slept would be asserting on wall-clock time -- the defect
// cleat#1504 was filed for.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestShare(budget int, hold time.Duration) (*connectionShare, *fakeClock) {
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return newConnectionShare(budget, hold, c.now), c
}

func TestAShrinkIsAppliedImmediatelyAndAGrowthWaits(t *testing.T) {
	const hold = 30 * time.Second
	s, clk := newTestShare(100, hold)

	if got := s.Observe(1); got != 100 {
		t.Fatalf("first observation applied %d, want the whole budget 100; a worker that "+
			"registered before counting is claiming its own share, not a departed one", got)
	}

	// A second worker joins. This is the direction that must be instant.
	if got := s.Observe(2); got != 50 {
		t.Fatalf("after a second worker joined, share is %d, want 50 applied immediately; "+
			"being slow to shrink is the failure that overcommits the database", got)
	}
	if got := s.Observe(4); got != 25 {
		t.Fatalf("after four workers, share is %d, want 25", got)
	}

	// Two leave. This is the direction that must wait.
	if got := s.Observe(2); got != 25 {
		t.Fatalf("share grew to %d the instant the count dropped; it must hold at 25. A "+
			"crashed worker stops heartbeating at once but its connections survive until "+
			"the server reaps them", got)
	}
	pending, held := s.Pending()
	if pending != 50 || held != 0 {
		t.Errorf("Pending() = (%d, %v), want (50, 0)", pending, held)
	}

	clk.advance(hold - time.Second)
	if got := s.Observe(2); got != 25 {
		t.Errorf("share grew to %d one second before the hold-down elapsed, want 25", got)
	}
	clk.advance(2 * time.Second)
	if got := s.Observe(2); got != 50 {
		t.Errorf("share is %d after the hold-down elapsed, want 50", got)
	}
	if pending, _ := s.Pending(); pending != 0 {
		t.Errorf("Pending() = %d after the growth was taken, want 0", pending)
	}
}

func TestAShrinkDuringAHeldGrowthIsStillImmediate(t *testing.T) {
	const hold = 30 * time.Second
	s, clk := newTestShare(100, hold)
	s.Observe(4) // 25

	s.Observe(2) // wants 50, holds at 25
	clk.advance(hold / 2)

	// A worker joins again while the growth is pending. The pending growth must
	// not survive: it was an entitlement under a count that no longer holds.
	if got := s.Observe(5); got != 20 {
		t.Fatalf("a join during a held growth applied %d, want 20 immediately", got)
	}
	if pending, _ := s.Pending(); pending != 0 {
		t.Errorf("a pending growth of %d survived a shrink; it must be discarded", pending)
	}
	clk.advance(hold * 2)
	if got := s.Observe(5); got != 20 {
		t.Errorf("share is %d after time passed with the count steady at 5, want 20 -- the "+
			"discarded growth must not resurface", got)
	}
}

func TestAFlappingCountRestartsTheHoldDownRatherThanAccumulatingCredit(t *testing.T) {
	const hold = 30 * time.Second
	s, clk := newTestShare(120, hold)
	s.Observe(6) // 20

	s.Observe(3) // wants 40, holds
	clk.advance(hold - time.Second)

	// The count moves to a DIFFERENT larger target before the first elapsed.
	if got := s.Observe(2); got != 20 {
		t.Fatalf("a new larger target applied %d, want 20 -- still held", got)
	}
	clk.advance(2 * time.Second) // past the ORIGINAL deadline, not the new one
	if got := s.Observe(2); got != 20 {
		t.Errorf("share is %d: the clock was inherited from the previous target. A flapping "+
			"count must restart the hold-down, not accumulate credit toward growth", got)
	}
	clk.advance(hold)
	if got := s.Observe(2); got != 60 {
		t.Errorf("share is %d after a full hold-down at the new target, want 60", got)
	}
}

func TestTheFloorIsOnePerWorkerAndAZeroCountIsReadAsOne(t *testing.T) {
	s, _ := newTestShare(10, time.Minute)
	if got := s.Observe(50); got != 1 {
		t.Errorf("with a budget of 10 across 50 workers the share is %d, want the floor 1. "+
			"Handing back 0 would make the cluster unable to run the protocol that reports "+
			"the misconfiguration", got)
	}

	// A count of zero means this worker did not see its own row. "I am alone,
	// take everything" is the one answer that is certainly wrong.
	s2, _ := newTestShare(100, time.Minute)
	if got := s2.Observe(0); got != 100 {
		t.Errorf("first observation of a zero count gave %d, want 100 (read as 1 worker)", got)
	}
	s3, _ := newTestShare(100, time.Minute)
	s3.Observe(4) // 25
	if got := s3.Observe(0); got != 25 {
		t.Errorf("a zero count after a real one applied %d immediately, want 25 held -- a "+
			"count that lost this worker's own row must not grant the whole budget", got)
	}
}
