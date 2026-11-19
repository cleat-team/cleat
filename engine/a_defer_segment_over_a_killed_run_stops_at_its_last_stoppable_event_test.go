package engine

import (
	"context"
	"testing"
)

// cleat#4079: a defer segment over a KILLED run replays only as far as the last host call the engine can refuse, and stops
// there. These are the mechanics, with no guest: the stop-point rule, and what the fast-forward does to the session.

func TestLastStoppableIndex(t *testing.T) {
	ev := func(t EventType) EventRecord { return EventRecord{EventType: t} }
	drain := func(t EventType) EventRecord { return EventRecord{EventType: t, InDeferPhase: true} }
	// An event a defer SEGMENT recorded itself (cleat#4079), marked by the host and never by the guest. A re-run over such a history must not
	// stop at the segment's own calls: they are not the original run's last effect.
	seg := func(t EventType) EventRecord { return EventRecord{EventType: t, InDeferSegment: true} }
	// What resolve_promise and reject_promise record (cleat#4079): the promise event types WITH the Op that marks them as the effect.
	settle := func(t EventType, op string) EventRecord {
		return EventRecord{EventType: t, Service: PromiseEventService, Op: op, PromiseID: "p"}
	}
	cases := []struct {
		name string
		hist []EventRecord
		want int
	}{
		{"empty history has no stop point", nil, -1},
		{"only effect-free or unguarded events have none", []EventRecord{ev(EventTypeDurableLog), ev(EventTypeStateMutation), ev(EventTypeCreatePromise)}, -1},
		{"a single call is the stop point", []EventRecord{ev(EventTypeCall)}, 0},
		{"the LAST call wins over an earlier one", []EventRecord{ev(EventTypeCall), ev(EventTypeCall), ev(EventTypeCall)}, 2},
		{"trailing effect-free and unguarded events are skipped past", []EventRecord{ev(EventTypeDefer), ev(EventTypeCall), ev(EventTypeDurableLog), ev(EventTypeStateMutation), ev(EventTypeScopeAcquired)}, 1},
		{"a defer registration is a stop point", []EventRecord{ev(EventTypeCall), ev(EventTypeDefer), ev(EventTypeDurableLog)}, 1},
		{"a retried call is reported at its FIRST attempt, not between its attempts", []EventRecord{ev(EventTypeDefer), ev(EventTypeCallAttemptFailed), ev(EventTypeCallAttemptFailed), ev(EventTypeCall)}, 1},
		{"a call interrupted mid-retry is not a stop point; the one before it is", []EventRecord{ev(EventTypeCall), ev(EventTypeCallAttemptFailed)}, 0},
		{"a segment's own events are never a stop point", []EventRecord{ev(EventTypeDefer), ev(EventTypeCall), seg(EventTypeCall), seg(EventTypeDefer)}, 1},
		{"a defer drain's own calls are never a stop point", []EventRecord{ev(EventTypeDefer), drain(EventTypeCall)}, 0},
		{"resolve_promise is a stop point", []EventRecord{ev(EventTypeDefer), settle(EventTypePromiseResolved, PromiseOpResolve)}, 1},
		{"reject_promise is a stop point", []EventRecord{ev(EventTypeDefer), settle(EventTypePromiseRejected, PromiseOpReject)}, 1},
		// THE CONTROL. await_promise of an already-settled promise records the same event type with the same PromiseID and result,
		// and only the resolve and reject calls are effects. A stop at the await returns normally for a settled promise and the
		// guest runs on inside the segment, so the stop point must be the call before it.
		{"an await of a resolved promise is NOT a stop point", []EventRecord{ev(EventTypeDefer), ev(EventTypeCall), {EventType: EventTypePromiseResolved, PromiseID: "p"}}, 1},
		{"an await of a rejected promise is NOT a stop point", []EventRecord{ev(EventTypeDefer), ev(EventTypeCall), {EventType: EventTypePromiseRejected, PromiseID: "p"}}, 1},
		// A history written before cleat#4079 has no Op on its resolve event, so it cannot be told from an await and is not a stop.
		{"a resolve recorded before the discriminator existed is not a stop point", []EventRecord{ev(EventTypeDefer), ev(EventTypeCall), {EventType: EventTypePromiseResolved, Service: PromiseEventService, PromiseID: "p"}}, 1},
		{"a promise event with an Op of the wrong kind is not a stop point", []EventRecord{ev(EventTypeDefer), ev(EventTypeCall), settle(EventTypePromiseResolved, PromiseOpReject)}, 1},
		{"the LAST settlement wins over an earlier call", []EventRecord{ev(EventTypeCall), settle(EventTypePromiseResolved, PromiseOpResolve), ev(EventTypeDurableLog)}, 1},
		{"the first event of a two-event call, not its second", []EventRecord{ev(EventTypeDefer), ev(EventTypeAwaitSignals), ev(EventTypeSignalReceived)}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastStoppableIndex(c.hist); got != c.want {
				t.Fatalf("lastStoppableIndex = %d, want %d", got, c.want)
			}
		})
	}
}

// segmentSession builds the session a defer segment over hist would have, armed or not.
func segmentSession(t *testing.T, hist []EventRecord, opts ...EngineOption) *execSession {
	t.Helper()
	for i := range hist {
		hist[i].Step = i
		hist[i].TimestampMs = int64(1000 + i)
	}
	e := NewEngine(nil, &mockCaller{}, opts...)
	s := &execSession{engine: e, history: hist, isReplay: len(hist) > 0, chainAnchor: e.chainAnchor(), deferrals: map[string]string{}}
	s.armStopAtLastStoppable(context.Background(), true)
	return s
}

func chainOver(hist []EventRecord) string {
	last := ""
	for _, r := range hist {
		last = computeEventChecksum(r, last)
	}
	return last
}

func TestAKilledRunsSegmentFastForwardsToItsStopPoint(t *testing.T) {
	ctx := context.Background()
	mk := func() []EventRecord {
		return []EventRecord{
			{EventType: EventTypeDefer}, {EventType: EventTypeCall}, {EventType: EventTypeCall}, // stop point is index 2
			{EventType: EventTypeDurableLog}, {EventType: EventTypeStateMutation},
		}
	}
	on := []EngineOption{WithDeferPhase(), WithStopAtLastStoppable()}

	t.Run("replay stops before the stop point and leaves the history intact", func(t *testing.T) {
		s := segmentSession(t, mk(), on...)
		if s.stopIdx != 2 {
			t.Fatalf("PRECONDITION FAILED: stopIdx = %d, want 2", s.stopIdx)
		}
		// Consume events 0 and 1 the way the guest's replayed calls do.
		for i := 0; i < 2; i++ {
			if !s.isReplay {
				t.Fatalf("left replay after consuming only %d event(s)", i)
			}
			rec := s.history[s.stepCount]
			s.advanceReplayStep(ctx, &rec)
		}
		if s.isReplay {
			t.Fatal("still replaying after the event before the stop point was consumed; the next call would be replayed and not refused")
		}
		if s.stepCount != len(s.history) {
			t.Fatalf("stepCount = %d, want %d: the segment's own events must be recorded AFTER the whole history", s.stepCount, len(s.history))
		}
		if len(s.history) != 5 {
			t.Fatalf("history was altered: %d events", len(s.history))
		}
		if got, want := s.lastChecksum, chainOver(s.history); got != want {
			t.Fatalf("checksum chain after the fast-forward is %q, want the chain over every history event %q: the segment's first event would chain from the wrong predecessor", got, want)
		}
		if got, want := s.nowMs, int64(1000+4); got != want {
			t.Fatalf("virtual clock = %d, want the last event's timestamp %d", got, want)
		}
		if !s.stopBeforeNewWork() {
			t.Fatal("stopBeforeNewWork is false in the segment, so the next call would not be refused")
		}
	})

	t.Run("a stop point at index 0 fast-forwards when the session is armed", func(t *testing.T) {
		s := segmentSession(t, []EventRecord{{EventType: EventTypeCall}, {EventType: EventTypeDurableLog}}, on...)
		if s.isReplay || s.stepCount != 2 {
			t.Fatalf("isReplay=%v stepCount=%d, want not replaying and stepCount 2: nothing precedes the stop event to trigger it", s.isReplay, s.stepCount)
		}
		if got, want := s.lastChecksum, chainOver(s.history); got != want {
			t.Fatalf("checksum chain %q, want %q", got, want)
		}
	})

	t.Run("CONTROL: without the option the same history replays in full", func(t *testing.T) {
		s := segmentSession(t, mk(), WithDeferPhase())
		if s.stopIdx != -1 {
			t.Fatalf("stopIdx = %d without the option, want -1", s.stopIdx)
		}
		for s.stepCount < len(s.history) {
			if !s.isReplay {
				t.Fatalf("left replay at step %d of %d without the option", s.stepCount, len(s.history))
			}
			rec := s.history[s.stepCount]
			s.advanceReplayStep(ctx, &rec)
		}
	})

	t.Run("CONTROL: an ordinary engine (no defer phase) never stops early", func(t *testing.T) {
		s := segmentSession(t, mk(), WithStopAtLastStoppable())
		if s.stopIdx != -1 {
			t.Fatalf("stopIdx = %d outside a defer segment, want -1", s.stopIdx)
		}
	})

	t.Run("CONTROL: a core module is never stopped early, whatever the options say", func(t *testing.T) {
		hist := mk()
		for i := range hist {
			hist[i].Step = i
		}
		e := NewEngine(nil, &mockCaller{}, WithDeferPhase(), WithStopAtLastStoppable())
		s := &execSession{engine: e, history: hist, isReplay: true, chainAnchor: e.chainAnchor()}
		s.armStopAtLastStoppable(ctx, false)
		if s.stopIdx != -1 || s.stopArmed {
			t.Fatalf("stopIdx=%d armed=%v for a core module, want -1 and false: its segment stops at its frontier as it always has", s.stopIdx, s.stopArmed)
		}
	})

	t.Run("a session that was never armed carries the zero stopIdx and must not stop at index 0", func(t *testing.T) {
		hist := mk()
		for i := range hist {
			hist[i].Step = i
		}
		s := &execSession{engine: NewEngine(nil, &mockCaller{}, WithDeferPhase(), WithStopAtLastStoppable()), history: hist, isReplay: true}
		rec := s.history[0]
		s.advanceReplayStep(ctx, &rec) // stepCount becomes 1; stopIdx is the zero value
		if !s.isReplay {
			t.Fatal("an unarmed session left replay on its first consumed event")
		}
	})
}
