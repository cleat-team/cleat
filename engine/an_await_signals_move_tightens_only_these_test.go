package engine

// W5 of cleat#4152: the rows where moving DurableAwaitSignals onto the dispatcher CHANGES what it does, and nothing else. The 33 rows of
// an_await_signals_behaves_as_recorded_before_the_dispatcher_test.go pass on the old code AND the moved code (checked by running that file on both),
// so this is the whole of the difference. Every row here is a TIGHTENING (the rule from the coordinator: a migration never loosens a check the
// old code had, and identity rows only tighten); each one passes on the new code and FAILS on the old, which is what makes it a row about the
// move and not about the call.

import "testing"

func TestTheAwaitSignalsMoveTightensTheseAndNothingElse(t *testing.T) {
	await := func(ts, timeoutMs int64, names string) EventRecord {
		return EventRecord{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: names, TimeoutMs: timeoutMs, TimestampMs: ts}
	}
	delivered := func(step int, name, payload string) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeSignalReceived, SignalName: name, SignalPayload: payload}
	}
	closed := make(chan struct{})
	close(closed)

	rows := []awaitRow{
		// IDENTITY. The old code compared nothing: any signal_received was handed to the next await, whatever it asked for.
		{name: "replay: a delivery recorded under a name the call did not ask for is a divergence that consumes nothing (old: delivered it)",
			hist: []EventRecord{delivered(0, "z", "pz")}, timeoutMs: 60000,
			want: awaitWant{diverged: true, step: 0, replaying: true}},
		{name: "replay: an await recorded for other names is a divergence that consumes nothing (old: consumed it)",
			hist: []EventRecord{await(awaitT-1000, 60000, `["x"]`)}, timeoutMs: 60000,
			want: awaitWant{diverged: true, step: 0, replaying: true}},
		// cleat#933's misattribution, closed: an await followed by ANOTHER call's delivery (a different name) times out alone and leaves the delivery.
		{name: "replay: an await followed by a delivery of another name times out ALONE and leaves the delivery (old: paired them)",
			hist: []EventRecord{await(awaitT-1000, 60000, awaitNames), delivered(1, "z", "pz")}, timeoutMs: 60000,
			want: awaitWant{timedOut: true, step: 1, replaying: true}},

		// ONE REFUSAL FOR BOTH ARRIVAL PATHS (the cleat#4143 shape). The old code checked stopBeforeNewWork only on the FRESH path; the replay
		// frontier polled the store and recorded a delivery on a worker that was shutting down, and on a session that had already suspended.
		{name: "replay frontier: a worker that is shutting down refuses before it polls or records (old: delivered at the frontier)",
			opts: []EngineOption{WithShutdownSignal(closed)}, deliver: map[string][]SignalDelivery{"a": {{ID: 7, Payload: "pa"}}},
			hist: []EventRecord{await(awaitT-10_000, 60000, awaitNames)}, timeoutMs: 60000,
			want: awaitWant{sentinel: true, step: 1}},
		{name: "replay frontier: a session that has already suspended refuses a second await before it polls (old: polled and recorded)",
			mod:     func(s *execSession) { s.suspendErr = &SuspendError{Reason: "earlier"} },
			deliver: map[string][]SignalDelivery{"a": {{ID: 7, Payload: "pa"}}},
			hist:    []EventRecord{await(awaitT-10_000, 60000, awaitNames)}, timeoutMs: 60000,
			want: awaitWant{sentinel: true, reason: "earlier", step: 1}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { runAwaitRow(t, r) })
	}
	if len(rows) != 5 {
		t.Fatalf("the tightening table has %d rows, want 5: every row is a deliberate change and is listed in the PR", len(rows))
	}
}
