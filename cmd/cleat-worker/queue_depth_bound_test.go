package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4311. Waking the dispatch loop when a slot is freed makes a pass per completion on a saturated worker, and every pass runs
// MemoryController.Tick, which counts the ready leases. The count grows with the queue that keeps a worker saturated, so it is bounded by
// time, not by the number of passes: at most one per --poll. What these hold is the bound, the control that a controller built without it is
// unchanged, and that the bound lifts when the interval has passed.

type countingDepthStore struct {
	engine.WorkflowStore
	calls atomic.Int64
	depth atomic.Int64
}

func (s *countingDepthStore) QueueDepth(context.Context) (int64, error) {
	s.calls.Add(1)
	return s.depth.Load(), nil
}

func tickN(c *MemoryController, n int) {
	for i := 0; i < n; i++ {
		c.Tick(context.Background())
	}
}

func TestAHundredDispatchPassesCountTheReadyLeasesOnceInsideThePollInterval(t *testing.T) {
	store := &countingDepthStore{}
	store.depth.Store(7)
	c := NewMemoryController(NewMemoryMonitor(time.Second), store, "w", 2, 1<<40, 1<<40)
	c.queueDepthEvery = time.Hour // far longer than the test: every Tick after the first is inside it
	tickN(c, 100)
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("100 Ticks inside one interval made %d QueueDepth queries, want 1", got)
	}
	if got := c.State().QueueDepth; got != 7 {
		t.Fatalf("the reading reused by the later Ticks was %d, want the 7 the first one read", got)
	}
}

func TestAControllerWithTheBoundSwitchedOffStillCountsOnEveryTick(t *testing.T) {
	// The control that makes the test above able to fail: with the bound off the same loop makes 100 queries.
	store := &countingDepthStore{}
	c := NewMemoryController(NewMemoryMonitor(time.Second), store, "w", 2, 1<<40, 1<<40)
	c.queueDepthEvery = 0
	tickN(c, 100)
	if got := store.calls.Load(); got != 100 {
		t.Fatalf("100 Ticks with no interval made %d QueueDepth queries, want 100", got)
	}
}

func TestTheQueueDepthIsCountedAgainOnceTheIntervalHasPassed(t *testing.T) {
	store := &countingDepthStore{}
	store.depth.Store(1)
	c := NewMemoryController(NewMemoryMonitor(time.Second), store, "w", 2, 1<<40, 1<<40)
	c.queueDepthEvery = 40 * time.Millisecond
	tickN(c, 10)
	time.Sleep(80 * time.Millisecond)
	store.depth.Store(9)
	tickN(c, 10)
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("two bursts of Ticks an interval apart made %d queries, want 2", got)
	}
	if got := c.State().QueueDepth; got != 9 {
		t.Fatalf("the reading after the interval was %d, want the 9 the second query read", got)
	}
}

func TestTheSlotFreedChannelHoldsExactlyOneToken(t *testing.T) {
	// Unbuffered, a slot freed between the loop's count and its wait is dropped by the non-blocking send; deeper, wakes queue into passes the first
	// already made. main.go builds the Worker's channel with this function, so a change here is a change there.
	if got := cap(newSlotFreedChannel()); got != 1 {
		t.Fatalf("the slot-freed channel has capacity %d, want 1", got)
	}
}

func TestAControllerIsBoundedWithoutAnyoneSettingTheInterval(t *testing.T) {
	// main.go replaces the interval with --poll; this is what holds when that line is missing or a controller is built another way.
	store := &countingDepthStore{}
	c := NewMemoryController(NewMemoryMonitor(time.Second), store, "w", 2, 1<<40, 1<<40)
	tickN(c, 100)
	if got := store.calls.Load(); got > 2 {
		t.Fatalf("100 Ticks on a controller nobody configured made %d QueueDepth queries; it should be bounded by default (want 1, 2 at most if the %s boundary passed)", got, defaultQueueDepthEvery)
	}
}
