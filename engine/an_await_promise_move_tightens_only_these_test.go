package engine

// W4 of cleat#4152: the rows where moving AwaitPromise onto the dispatcher CHANGES what it does, and nothing else. The rows of
// an_await_promise_behaves_as_recorded_before_the_dispatcher_test.go pass on the old code AND the moved code (checked by running that file on both),
// so this is the whole of the difference. Every row here is a TIGHTENING, each one decided by the coordinator on 2026-11-20 (a migration never loosens a
// check the old code had, and identity rows only tighten): it passes on the new code and FAILS on the old, which is what makes it a row about the move
// and not about the call.
//
// A divergence consumes nothing (cleat#4157), leaves replay ON, reads the store NOT AT ALL and records nothing, so every row below has step 0, replaying,
// no events and no store reads; what differs between them is the message, which says what was recorded and what was asked.

import "testing"

func TestTheAwaitPromiseMoveTightensTheseAndNothingElse(t *testing.T) {
	await := func(id string, ts, timeoutMs int64) EventRecord {
		return EventRecord{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: id, TimeoutMs: timeoutMs, TimestampMs: ts}
	}
	const now = int64(1_790_000_000_000)
	diverged := func(has string) promiseWant {
		return promiseWant{errCode: 1, resultHas: has, step: 0, replaying: true}
	}

	rows := []promiseRow{
		// IDENTITY of the FIRST record. The look-ahead after an await always compared the promise id and the discriminator; the first record never did.
		{name: "replay: a recorded resolution of ANOTHER promise is a divergence that consumes nothing (old: consumed it and handed it back as this await's answer)",
			hist:      []EventRecord{{Step: 0, EventType: EventTypePromiseResolved, PromiseID: "other", PromiseResult: "v"}},
			timeoutMs: 60000, want: diverged(`promise "p1", recorded "other"`)},
		{name: "replay: a recorded rejection of ANOTHER promise is a divergence that consumes nothing (old: consumed it, with the rejection flag set)",
			hist:      []EventRecord{{Step: 0, EventType: EventTypePromiseRejected, PromiseID: "other", PromiseError: "boom"}},
			timeoutMs: 60000, want: diverged(`promise "p1", recorded "other"`)},
		{name: "replay: a recorded await of ANOTHER promise is a divergence that consumes nothing (old: consumed it as this one's, and read the next record against p1)",
			hist:      []EventRecord{await("other", now-10_000, 3_600_000)},
			timeoutMs: 60000, want: diverged(`promise "p1", recorded "other"`)},
		{name: "replay: a no-wait await of ANOTHER promise is a divergence too, not the timeout it would have replayed as (old: consumed it and answered timed out)",
			hist:      []EventRecord{await("other", now-10_000, 0)},
			timeoutMs: 0, want: diverged(`promise "p1", recorded "other"`)},
		// The discriminator on the FIRST record. A settlement the workflow made itself (resolve_promise records the same type, for the same promise, with
		// Service promise and Op resolve) is not the answer to an await, and the record after an await already refused it (cleat#4178).
		{name: "replay: a settlement the WORKFLOW made itself is a divergence when it is the first record (old: handed back as this await's answer)",
			hist:      []EventRecord{{Step: 0, EventType: EventTypePromiseResolved, PromiseID: "p1", PromiseResult: "v", Service: PromiseEventService, Op: PromiseOpResolve}},
			timeoutMs: 60000, want: diverged("a settlement the workflow made itself")},

		// A FIRST RECORD OF ANOTHER TYPE (cleat#947). The old code did not report a divergence: it fell through to the FRESH path with history still unread and
		// replay still on, read the store and recorded at a step that already held an event (the cleat#4157 class). With a store it is a divergence now.
		// (With NO store it is still the error and consumes nothing: a row in the characterization table, because it holds on both.)
		{name: "replay: a first record of another type is a divergence when there is a store (old: ran the fresh path, recorded a resolved promise at a step that held an event)",
			status: "resolved", result: "v", hist: []EventRecord{{Step: 0, EventType: fakeEvent}}, timeoutMs: 60000,
			want: diverged("expected promise_resolved, got fake_step_event")},
		{name: "replay: ... and with a pending promise (old: recorded the await at that step and suspended, still replaying)",
			status: "pending", hist: []EventRecord{{Step: 0, EventType: fakeEvent}}, timeoutMs: 60000,
			want: diverged("expected promise_resolved, got fake_step_event")},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { runPromiseRow(t, r) })
	}
	if len(rows) != 7 {
		t.Fatalf("the tightening table has %d rows, want 7: every row is a deliberate change and is listed in the PR", len(rows))
	}
}
