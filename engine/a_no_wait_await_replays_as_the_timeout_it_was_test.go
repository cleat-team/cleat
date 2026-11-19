package engine

import (
	"testing"
	"time"
)

// cleat#4144, cleat#4153. A await with a timeout of 0 or less does not wait: it returns "timed out" at once. The first version of the fix
// RECORDED NOTHING for that outcome (as await_signals did), and cleat-review measured two defects in a replay of such a history:
//
//  1. The outcome of a look at the store is not reproducible. A replay that ran after the promise settled (or a signal arrived) returned
//     the VALUE where the original run had been told it timed out, so a poll loop took a different branch on its first resume.
//  2. With no event at that step, the replay found some LATER event there, fell out of the replay branch with history unread, and
//     called exitReplay. The rest of the replay then ran FRESH, so a durable call already recorded after the poll EXECUTED AGAIN: a
//     duplicate side effect.
//
// So the no-wait outcome is recorded, and a replay returns the timeout from the record alone. Every row replays a history the
// ORIGINAL run produced, against a store that has changed since.

type replayProbe struct {
	s      *execSession
	caller *mockCaller
}

// originalRun is the run the history comes from: a session with the given stores that makes the poll, then makes one durable call.
func originalRun(t *testing.T, opts ...EngineOption) (*execSession, *mockCaller) {
	t.Helper()
	c := &mockCaller{}
	opts = append([]EngineOption{WithWorkflowID("wf-4153")}, opts...)
	return &execSession{
		engine: NewEngine(nil, c, opts...), workflowID: "wf-4153", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
	}, c
}

func replayOf(hist []EventRecord, opts ...EngineOption) replayProbe {
	c := &mockCaller{}
	opts = append([]EngineOption{WithWorkflowID("wf-4153")}, opts...)
	return replayProbe{
		s: &execSession{
			engine: NewEngine(nil, c, opts...), workflowID: "wf-4153", nowMs: time.Now().UnixMilli(),
			deferrals: map[string]string{}, queryState: map[string]string{},
			isReplay: true, history: append([]EventRecord(nil), hist...),
		},
		caller: c,
	}
}

func timedOutWord(packed int64) bool { return (packed>>16)&0xFFFF != 0 }

// callAfterThePoll makes the one durable call the original run made after its poll, through the real DurableCall.
func (p replayProbe) callAfterThePoll(t *testing.T) string {
	t.Helper()
	ctx := hostCtx()
	packed := p.s.DurableCall(ctx, nil, "work", "charge", `{}`, 0, 256)
	if packed == callSuspendSentinel {
		t.Fatal("the call after the poll was refused with a stop")
	}
	return "" // the response is not what is asserted: whether the call RAN is
}

func TestAPromiseAwaitWithNoTimeoutReplaysAsTheTimeoutItWas(t *testing.T) {
	const id = "prom-4153"

	// The original run: the promise is pending, the poll times out, and the guest goes on to make a durable call.
	orig, _ := originalRun(t, WithPromiseStore(&mockPromiseStore{status: ""}))
	if got := orig.AwaitPromise(hostCtx(), nil, id, 0, 0, 256); !timedOutWord(got) {
		t.Fatalf("PRECONDITION FAILED: the original poll returned %#x, want the timed-out word", got)
	}
	if len(orig.history) != 1 || orig.history[0].EventType != EventTypeAwaitPromise {
		t.Fatalf("PRECONDITION FAILED: the original run recorded %+v, want exactly one await_promise event for the poll", orig.history)
	}
	orig.recordEvent(EventRecord{Step: orig.stepCount, EventType: EventTypeCall, Service: "work", Op: "charge", Request: `{}`, Response: `{"ok":true}`})
	hist := orig.history

	for _, tc := range []struct {
		name  string
		store *mockPromiseStore
	}{
		{"the promise is still pending", &mockPromiseStore{status: ""}},
		// THE FINDING. The store now says resolved; the original run was told it timed out.
		{"the promise has RESOLVED since", &mockPromiseStore{status: "resolved", result: `{"v":1}`}},
		{"the promise has been REJECTED since", &mockPromiseStore{status: "rejected", errMsg: "no"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := replayOf(hist, WithPromiseStore(tc.store))
			if got := p.s.AwaitPromise(hostCtx(), nil, id, 0, 0, 256); !timedOutWord(got) {
				t.Fatalf("the replayed poll returned %#x, want the timed-out word the original run was given", got)
			}
			if p.s.suspendErr != nil {
				t.Fatalf("the replayed poll suspended the run: %q", p.s.suspendErr.Reason)
			}
			if len(p.s.history) != len(hist) {
				t.Fatalf("the replayed poll appended %d event(s) to a history it was replaying", len(p.s.history)-len(hist))
			}
			// THE DUPLICATE SIDE EFFECT. History is still unread, so the replay must still be a replay, and the recorded call must be
			// served from it and not run.
			if !p.s.isReplay || p.s.stepCount != 1 {
				t.Fatalf("after the poll isReplay=%v stepCount=%d, want a session still replaying at step 1: the rest of the history would run fresh", p.s.isReplay, p.s.stepCount)
			}
			p.callAfterThePoll(t)
			if len(p.caller.calls) != 0 {
				t.Fatalf("the call recorded after the poll EXECUTED AGAIN on replay: %+v", p.caller.calls)
			}
		})
	}

	// CONTROL: a promise that was already settled when the original poll ran is reported, not timed out, and replays as it was.
	t.Run("CONTROL: settled at the time, reported then and now", func(t *testing.T) {
		o, _ := originalRun(t, WithPromiseStore(&mockPromiseStore{status: "resolved", result: `{"v":1}`}))
		if got := o.AwaitPromise(hostCtx(), nil, id, 0, 0, 256); timedOutWord(got) {
			t.Fatalf("a poll of a settled promise reported a timeout")
		}
		p := replayOf(o.history, WithPromiseStore(&mockPromiseStore{status: ""}))
		if got := p.s.AwaitPromise(hostCtx(), nil, id, 0, 0, 256); timedOutWord(got) {
			t.Fatalf("the replay timed out a promise the original poll reported as settled")
		}
	})
}

func TestASignalAwaitWithNoTimeoutReplaysAsTheTimeoutItWas(t *testing.T) {
	await := func(s *execSession) int64 {
		return s.DurableAwaitSignals(hostCtx(), nil, `["go"]`, 0, 0, 200, 256, 200)
	}

	// The original run: no signal is waiting, the poll times out, and the guest goes on to make a durable call.
	orig, _ := originalRun(t, WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
	if got := await(orig); !timedOutWord(got) {
		t.Fatalf("PRECONDITION FAILED: the original poll returned %#x, want the timed-out word", got)
	}
	if len(orig.history) != 1 || orig.history[0].EventType != EventTypeAwaitSignals {
		t.Fatalf("PRECONDITION FAILED: the original run recorded %+v, want exactly one await_signals event for the poll", orig.history)
	}
	orig.recordEvent(EventRecord{Step: orig.stepCount, EventType: EventTypeCall, Service: "work", Op: "charge", Request: `{}`, Response: `{"ok":true}`})
	hist := orig.history

	for _, tc := range []struct {
		name  string
		store *awaitProbeStore
	}{
		{"still no signal", &awaitProbeStore{deliveries: map[string]SignalDelivery{}}},
		// THE FINDING. A signal has arrived since; the original run was told it timed out.
		{"a signal has ARRIVED since", &awaitProbeStore{deliveries: map[string]SignalDelivery{"go": {Payload: `{}`}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := replayOf(hist, WithSignalStore(tc.store))
			if got := await(p.s); !timedOutWord(got) {
				t.Fatalf("the replayed poll returned %#x, want the timed-out word the original run was given", got)
			}
			if len(p.s.history) != len(hist) {
				t.Fatalf("the replayed poll appended %d event(s) to a history it was replaying", len(p.s.history)-len(hist))
			}
			if !p.s.isReplay || p.s.stepCount != 1 {
				t.Fatalf("after the poll isReplay=%v stepCount=%d, want a session still replaying at step 1: the rest of the history would run fresh", p.s.isReplay, p.s.stepCount)
			}
			p.callAfterThePoll(t)
			if len(p.caller.calls) != 0 {
				t.Fatalf("the call recorded after the poll EXECUTED AGAIN on replay: %+v", p.caller.calls)
			}
		})
	}

	// The poll is the LAST event: the guest timed out, then suspended on something that records no event (a durable sleep). Without the
	// rule the replay reached the end of history at this await, exited replay, and looked at the store: a signal that had arrived
	// meanwhile was returned where the original run had been told it timed out.
	t.Run("the poll is the last event and a signal has ARRIVED since", func(t *testing.T) {
		p := replayOf(hist[:1], WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{"go": {Payload: `{}`}}}))
		if got := await(p.s); !timedOutWord(got) {
			t.Fatalf("the replayed poll returned %#x, want the timed-out word the original run was given", got)
		}
		if len(p.s.history) != 1 {
			t.Fatalf("the replayed poll appended %d event(s) (a signal_received it had no right to record)", len(p.s.history)-1)
		}
	})

	// The misattribution of cleat#933 through this door: a poll that timed out, then a SECOND poll that found a signal waiting. The
	// second records only a signal_received, which the first poll's replay must not take for its own.
	t.Run("a later poll's delivery is not paired with an earlier poll that timed out", func(t *testing.T) {
		o, _ := originalRun(t, WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
		if !timedOutWord(await(o)) {
			t.Fatal("PRECONDITION FAILED: the first poll did not time out")
		}
		o.engine.signalStore = &awaitProbeStore{deliveries: map[string]SignalDelivery{"go": {Payload: `{"n":2}`}}}
		if timedOutWord(await(o)) {
			t.Fatal("PRECONDITION FAILED: the second poll found no signal")
		}
		if len(o.history) != 2 || o.history[0].EventType != EventTypeAwaitSignals || o.history[1].EventType != EventTypeSignalReceived {
			t.Fatalf("PRECONDITION FAILED: history = %+v, want [await_signals, signal_received]", o.history)
		}
		p := replayOf(o.history, WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
		if got := await(p.s); !timedOutWord(got) {
			t.Fatalf("the first replayed poll returned %#x: it took the SECOND poll's delivery, which the original run gave to the second", got)
		}
		if got := await(p.s); timedOutWord(got) {
			t.Fatalf("the second replayed poll timed out: it lost the delivery the original run received")
		}
	})

	// CONTROL: a recorded timeout of 0 with a POSITIVE argument is not a no-wait replay (a history written before the timeout was
	// persisted). It must still take the old path, which suspends; if the new rule swallowed it, every row above would pass trivially.
	t.Run("CONTROL: a recorded 0 resumed with a positive argument still waits", func(t *testing.T) {
		legacy := EventRecord{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: `["go"]`, TimeoutMs: 0, TimestampMs: time.Now().UnixMilli()}
		p := replayOf([]EventRecord{legacy}, WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
		p.s.DurableAwaitSignals(hostCtx(), nil, `["go"]`, 60000, 0, 200, 256, 200)
		if p.s.suspendErr == nil {
			t.Fatal("a legacy recorded 0 resumed with a positive argument did not suspend: the no-wait rule swallowed it")
		}
	})
}
