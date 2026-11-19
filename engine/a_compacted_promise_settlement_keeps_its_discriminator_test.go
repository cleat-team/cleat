package engine

import (
	"testing"
)

// cleat#4079 part 3 puts Service="promise" and Op="resolve"|"reject" on promise_resolved and promise_rejected, because an await of an
// already-settled promise records the same event type and only the settlements are effects a defer segment's stop point may sit at.
// The event checksum covers Service and Op. Compaction copies fields PER EVENT TYPE, and for these two it kept only the promise id and
// the result or error, so an expanded history had lost the discriminator. Two consequences, both silent: the settlement stopped being
// a stop point (the safe direction, but the guarantee that a killed run's cleanup sees every effect then failed for exactly the old,
// compacted runs), and the checksum chain recomputed over the expanded history no longer matched the one that was recorded.
//
// This test goes through the real compaction functions, with the chain computed the way the store computes it, and reads the
// discriminator back through the predicate that chooses the stop point rather than comparing fields alone.
func TestACompactedPromiseSettlementKeepsItsDiscriminator(t *testing.T) {
	settle := func(step int, typ EventType, op, id string) EventRecord {
		r := EventRecord{Step: step, EventType: typ, Service: PromiseEventService, Op: op, PromiseID: id, TimestampMs: int64(1000 + step)}
		if typ == EventTypePromiseResolved {
			r.PromiseResult = `"done"`
		} else {
			r.PromiseError = "declined"
		}
		return r
	}
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCreatePromise, PromiseName: "a", PromiseID: "p-a", TimestampMs: 1000},
		{Step: 1, EventType: EventTypeCreatePromise, PromiseName: "b", PromiseID: "p-b", TimestampMs: 1001},
		settle(2, EventTypePromiseResolved, PromiseOpResolve, "p-a"),
		settle(3, EventTypePromiseRejected, PromiseOpReject, "p-b"),
		// The control: an await of an already-settled promise. Same event type, same id and result, no Service, no Op.
		{Step: 4, EventType: EventTypeAwaitPromise, PromiseID: "p-a", TimestampMs: 1004},
		{Step: 5, EventType: EventTypePromiseResolved, PromiseID: "p-a", PromiseResult: `"done"`, TimestampMs: 1005},
		{Step: 6, EventType: EventTypeCall, Service: "work", Op: "charge", Request: "{}", Response: "{}", TimestampMs: 1006},
	}

	cs, err := extractCompactionState(events)
	if err != nil {
		t.Fatalf("extractCompactionState: %v", err)
	}
	full := buildFullHistoryFromCompaction(nil, cs)
	if len(full) != len(events) {
		t.Fatalf("expanded history has %d events, recorded %d", len(full), len(events))
	}

	for i := range events {
		if events[i].EventType != EventTypePromiseResolved && events[i].EventType != EventTypePromiseRejected {
			continue
		}
		if full[i].Service != events[i].Service || full[i].Op != events[i].Op {
			t.Errorf("step %d (%s): expanded Service/Op = %q/%q, recorded %q/%q: compaction dropped the discriminator",
				i, events[i].EventType, full[i].Service, full[i].Op, events[i].Service, events[i].Op)
		}
		if got, want := isStoppableEvent(full[i]), isStoppableEvent(events[i]); got != want {
			t.Errorf("step %d (%s): stop point after compaction = %v, before = %v", i, events[i].EventType, got, want)
		}
	}

	// The known-positive for the predicate itself: the two settlements ARE stop points and the await's event is NOT, so the loop
	// above compares real differences and not two falses.
	if !isStoppableEvent(events[2]) || !isStoppableEvent(events[3]) {
		t.Fatal("the recorded resolve and reject must be stop points, or this test compares nothing")
	}
	if isStoppableEvent(events[5]) {
		t.Fatal("an await's promise_resolved event must not be a stop point")
	}

	chain := func(h []EventRecord) string {
		last := ""
		for _, r := range h {
			last = computeEventChecksum(r, last)
		}
		return last
	}
	if got, want := chain(full), chain(events); got != want {
		t.Errorf("the checksum chain over the expanded history is %s, over the recorded history %s: the expanded events do not hold "+
			"the fields the chain was computed over", got, want)
	}
}
