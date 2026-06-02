package engine

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Local event type constants for thread scheduling events not in engine.go.
// ---------------------------------------------------------------------------

const (
	EventTypeFork           EventType = "fork"
	EventTypeThreadComplete EventType = "thread_complete"
)

// ---------------------------------------------------------------------------
// Prototype structs for the replay scheduler.
// ---------------------------------------------------------------------------

// SchedulerEvent is a single event in the global history, carrying thread
// routing information (ThreadID, GlobalSeq) alongside the EventType.
type SchedulerEvent struct {
	ThreadID  string
	GlobalSeq int
	LocalStep int
	EventType EventType
}

// Scheduler holds the flat global event history (sorted by GlobalSeq) and
// manages the replay cursor (nextIndex). On each ConsumeOrStall call it
// checks whether the next unconsumed event belongs to the requesting thread.
type Scheduler struct {
	events    []SchedulerEvent
	nextIndex int
}

// ThreadSession tracks a single thread's replay progress during testing.
type ThreadSession struct {
	ThreadID  string
	LocalStep int
	Stalled   bool
}

// NewScheduler creates a Scheduler from the provided events. The events are
// sorted by GlobalSeq so callers may pass them in any order.
func NewScheduler(events []SchedulerEvent) *Scheduler {
	sorted := make([]SchedulerEvent, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].GlobalSeq < sorted[j].GlobalSeq
	})
	return &Scheduler{events: sorted}
}

// ConsumeOrStall checks whether the next unconsumed event belongs to
// threadID. If it does, the event is consumed (cursor advances) and returned
// with stalled=false. If the next event belongs to another thread, stalled=true
// is returned and the cursor does not advance.
//
// When expectedType is non-empty and the consumed event has a different type,
// ConsumeOrStall returns a replay-divergence error.
func (s *Scheduler) ConsumeOrStall(threadID string, expectedType EventType) (*SchedulerEvent, bool, error) {
	if s.nextIndex >= len(s.events) {
		return nil, false, nil
	}
	ev := &s.events[s.nextIndex]
	if ev.ThreadID != threadID {
		return nil, true, nil
	}
	if expectedType != "" && ev.EventType != expectedType {
		return nil, false, fmt.Errorf(
			"replay divergence at global seq %d: thread %q expected event type %q but found %q",
			ev.GlobalSeq, threadID, expectedType, ev.EventType,
		)
	}
	s.nextIndex++
	return ev, false, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// consume is a convenience wrapper that fatally fails on unexpected errors.
func consume(t *testing.T, sched *Scheduler, thread string, expectedType EventType) (*SchedulerEvent, bool) {
	t.Helper()
	ev, stalled, err := sched.ConsumeOrStall(thread, expectedType)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", thread, err)
	}
	return ev, stalled
}

// assertConsumed asserts that the call consumed an event with the given
// global sequence number (not stalled).
func assertConsumed(t *testing.T, ev *SchedulerEvent, stalled bool, wantSeq int) {
	t.Helper()
	if stalled {
		t.Fatal("expected consumption but got stall")
	}
	if ev.GlobalSeq != wantSeq {
		t.Fatalf("expected global seq %d, got %d", wantSeq, ev.GlobalSeq)
	}
}

// assertStalled asserts that the call stalled (event belonged to another thread).
func assertStalled(t *testing.T, stalled bool) {
	t.Helper()
	if !stalled {
		t.Fatal("expected stall but got consumption")
	}
}

// ---------------------------------------------------------------------------
// Test cases
// ---------------------------------------------------------------------------

// TestScheduler_SimpleInterleaving verifies that two threads with interleaved
// events (A, B, A, B) each consume their own events in order and stall when
// it is not their turn.
func TestScheduler_SimpleInterleaving(t *testing.T) {
	events := []SchedulerEvent{
		{ThreadID: "A", GlobalSeq: 1, LocalStep: 1, EventType: EventTypeCall},
		{ThreadID: "B", GlobalSeq: 2, LocalStep: 1, EventType: EventTypeCall},
		{ThreadID: "A", GlobalSeq: 3, LocalStep: 2, EventType: EventTypeCall},
		{ThreadID: "B", GlobalSeq: 4, LocalStep: 2, EventType: EventTypeCall},
	}
	sched := NewScheduler(events)

	// A consumes seq 1.
	ev, stalled := consume(t, sched, "A", EventTypeCall)
	assertConsumed(t, ev, stalled, 1)

	// A stalls — seq 2 belongs to B.
	_, stalled = consume(t, sched, "A", EventTypeCall)
	assertStalled(t, stalled)

	// B consumes seq 2.
	ev, stalled = consume(t, sched, "B", EventTypeCall)
	assertConsumed(t, ev, stalled, 2)

	// B stalls — seq 3 belongs to A.
	_, stalled = consume(t, sched, "B", EventTypeCall)
	assertStalled(t, stalled)

	// A consumes seq 3.
	ev, stalled = consume(t, sched, "A", EventTypeCall)
	assertConsumed(t, ev, stalled, 3)

	// A stalls — seq 4 belongs to B.
	_, stalled = consume(t, sched, "A", EventTypeCall)
	assertStalled(t, stalled)

	// B consumes seq 4.
	ev, stalled = consume(t, sched, "B", EventTypeCall)
	assertConsumed(t, ev, stalled, 4)
}

// TestScheduler_BurstyThreads verifies that when thread A has 5 consecutive
// events followed by thread B's 3 events, A consumes all 5 before B consumes
// any, and each thread stalls while the other is active.
func TestScheduler_BurstyThreads(t *testing.T) {
	events := make([]SchedulerEvent, 0, 8)
	for i := 1; i <= 5; i++ {
		events = append(events, SchedulerEvent{
			ThreadID: "A", GlobalSeq: i, LocalStep: i, EventType: EventTypeCall,
		})
	}
	for i := 6; i <= 8; i++ {
		events = append(events, SchedulerEvent{
			ThreadID: "B", GlobalSeq: i, LocalStep: i - 5, EventType: EventTypeCall,
		})
	}
	sched := NewScheduler(events)

	// A consumes all 5 events in a row. B stalls each time.
	for i := 1; i <= 5; i++ {
		_, stalled := consume(t, sched, "B", EventTypeCall)
		assertStalled(t, stalled)

		ev, stalled := consume(t, sched, "A", EventTypeCall)
		assertConsumed(t, ev, stalled, i)
	}

	// Now B consumes its 3 events in a row. A stalls each time.
	for i := 6; i <= 8; i++ {
		_, stalled := consume(t, sched, "A", EventTypeCall)
		assertStalled(t, stalled)

		ev, stalled := consume(t, sched, "B", EventTypeCall)
		assertConsumed(t, ev, stalled, i)
	}
}

// TestScheduler_SingleThread verifies that when all events belong to the same
// thread ("main"), they are consumed sequentially with no stalling.
func TestScheduler_SingleThread(t *testing.T) {
	events := []SchedulerEvent{
		{ThreadID: "main", GlobalSeq: 1, LocalStep: 1, EventType: EventTypeCall},
		{ThreadID: "main", GlobalSeq: 2, LocalStep: 2, EventType: EventTypeSignalReceived},
		{ThreadID: "main", GlobalSeq: 3, LocalStep: 3, EventType: EventTypeDefer},
	}
	sched := NewScheduler(events)

	ev, stalled := consume(t, sched, "main", EventTypeCall)
	assertConsumed(t, ev, stalled, 1)

	ev, stalled = consume(t, sched, "main", EventTypeSignalReceived)
	assertConsumed(t, ev, stalled, 2)

	ev, stalled = consume(t, sched, "main", EventTypeDefer)
	assertConsumed(t, ev, stalled, 3)

	// No more events — consumption returns nil, no stall.
	ev, stalled, err := sched.ConsumeOrStall("main", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stalled {
		t.Fatal("expected no stall on empty event list")
	}
	if ev != nil {
		t.Fatalf("expected nil event after exhaustion, got %+v", ev)
	}
}

// TestScheduler_StallResolution verifies that a stalled thread wakes up once
// the blocking thread consumes its event and the stalled thread's event reaches
// the head of the global sequence.
func TestScheduler_StallResolution(t *testing.T) {
	events := []SchedulerEvent{
		{ThreadID: "B", GlobalSeq: 1, LocalStep: 1, EventType: EventTypeCall},
		{ThreadID: "A", GlobalSeq: 2, LocalStep: 1, EventType: EventTypeCall},
	}
	sched := NewScheduler(events)

	// A stalls — seq 1 belongs to B.
	_, stalled := consume(t, sched, "A", EventTypeCall)
	assertStalled(t, stalled)

	// B consumes seq 1, advancing the cursor.
	ev, stalled := consume(t, sched, "B", EventTypeCall)
	assertConsumed(t, ev, stalled, 1)

	// Now A's event is at the head — A wakes and consumes seq 2.
	ev, stalled = consume(t, sched, "A", EventTypeCall)
	assertConsumed(t, ev, stalled, 2)
}

// TestScheduler_ForkAndJoin verifies that Fork events (which create new
// threads) and ThreadComplete events (which mark them done) are correctly
// routed to the right thread IDs, and that the parent thread resumes after
// the child's events are consumed.
func TestScheduler_ForkAndJoin(t *testing.T) {
	events := []SchedulerEvent{
		{ThreadID: "main", GlobalSeq: 1, LocalStep: 1, EventType: EventTypeFork},
		{ThreadID: "fork-1", GlobalSeq: 2, LocalStep: 1, EventType: EventTypeCall},
		{ThreadID: "fork-1", GlobalSeq: 3, LocalStep: 2, EventType: EventTypeCall},
		{ThreadID: "fork-1", GlobalSeq: 4, LocalStep: 3, EventType: EventTypeThreadComplete},
		{ThreadID: "main", GlobalSeq: 5, LocalStep: 2, EventType: EventTypeCall},
	}
	sched := NewScheduler(events)

	// main consumes the Fork event.
	ev, stalled := consume(t, sched, "main", EventTypeFork)
	assertConsumed(t, ev, stalled, 1)

	// main stalls — seq 2 belongs to fork-1.
	_, stalled = consume(t, sched, "main", EventTypeCall)
	assertStalled(t, stalled)

	// fork-1 consumes its three events in order.
	ev, stalled = consume(t, sched, "fork-1", EventTypeCall)
	assertConsumed(t, ev, stalled, 2)

	ev, stalled = consume(t, sched, "fork-1", EventTypeCall)
	assertConsumed(t, ev, stalled, 3)

	ev, stalled = consume(t, sched, "fork-1", EventTypeThreadComplete)
	assertConsumed(t, ev, stalled, 4)

	// fork-1 stalls — all its events are consumed.
	_, stalled = consume(t, sched, "fork-1", EventTypeCall)
	assertStalled(t, stalled)

	// main resumes and consumes seq 5.
	ev, stalled = consume(t, sched, "main", EventTypeCall)
	assertConsumed(t, ev, stalled, 5)
}

// TestScheduler_ReplayDivergence verifies that when the recorded event type
// does not match what the replaying thread expects, the scheduler returns a
// descriptive error instead of silently returning the wrong event.
func TestScheduler_ReplayDivergence(t *testing.T) {
	events := []SchedulerEvent{
		{ThreadID: "A", GlobalSeq: 1, LocalStep: 1, EventType: EventTypeCall},
	}
	sched := NewScheduler(events)

	// Thread A expects EventTypeSignalReceived but the recorded event is
	// EventTypeCall — should produce a divergence error.
	_, _, err := sched.ConsumeOrStall("A", EventTypeSignalReceived)
	if err == nil {
		t.Fatal("expected replay divergence error, got nil")
	}
	if !strings.Contains(err.Error(), "replay divergence") {
		t.Fatalf("expected error containing 'replay divergence', got: %v", err)
	}
	if !strings.Contains(err.Error(), "global seq 1") {
		t.Fatalf("expected error mentioning global seq 1, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"call"`) {
		t.Fatalf("expected error mentioning recorded type 'call', got: %v", err)
	}
}
