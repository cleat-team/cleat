package engine

import (
	"errors"
	"testing"
)

// cleat#4153, measured through a real worker (cmd/cleat-worker/a_poll_loop_of_no_wait_awaits_test.go): a run whose stored event count
// had reached --max-quota-events kept polling. The cap lived in freshCall only, so a poll never met it, and every poll then wrote an
// event into a quota the flush refuses ("event quota exceeded"): the refused write was retried for the whole flush window (about a
// second per poll), the event was not persisted, and the guest carried on until the 30 s wall-clock fence killed it.
//
// So a recorded no-wait await counts toward the cap and stops at it, the way a durable call does. Each row drives the session the
// worker builds: maxEventsPerWorkflow set, eventCount seeded as WithInitialEventCount seeds it.

func TestANoWaitPollCountsTowardTheEventCapAndStopsAtIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		poll func(s *execSession) int64
	}{
		{"await_promise", func(s *execSession) int64 { return s.AwaitPromise(hostCtx(), nil, "prom-cap", 0, 0, 256) }},
		{"await_signals", func(s *execSession) int64 { return s.DurableAwaitSignals(hostCtx(), nil, "go", 0, 0, 64, 64, 256) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cap = 3
			s, _ := originalRun(t, WithMaxQuotaEvents(cap), WithPromiseStore(&mockPromiseStore{status: ""}), WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
			s.originalInput = `{}`

			// Below the cap each poll times out, is recorded, and counts.
			for i := 0; i < cap; i++ {
				if got := tc.poll(s); !timedOutWord(got) {
					t.Fatalf("poll %d (below the cap) returned %#x, want the timed-out word", i, got)
				}
			}
			if len(s.history) != cap || s.eventCount != cap {
				t.Fatalf("after %d polls: %d events recorded, eventCount %d; want %d and %d (each poll is an event that counts)", cap, len(s.history), s.eventCount, cap, cap)
			}
			if s.autoContinueAsNewTriggered || s.suspendErr != nil {
				t.Fatalf("the cap fired early: triggered=%v suspendErr=%v", s.autoContinueAsNewTriggered, s.suspendErr)
			}

			// At the cap the poll is refused with the stop, a continue-as-new is under way, and NOTHING more is recorded.
			if got := tc.poll(s); got != callSuspendSentinel {
				t.Fatalf("the poll at the cap returned %#x, want the stop (%#x)", got, callSuspendSentinel)
			}
			var sus *SuspendError
			if !s.autoContinueAsNewTriggered || !errors.As(s.suspendErr, &sus) {
				t.Fatalf("the poll at the cap did not start the continue-as-new: triggered=%v suspendErr=%v", s.autoContinueAsNewTriggered, s.suspendErr)
			}
			// The continue-as-new records its own event, once; what must not appear is a further POLL event.
			polls, continues := countPollEvents(s.history)
			if polls != cap || continues != 1 {
				t.Fatalf("after the poll at the cap: %d poll events and %d continue-as-new events; want %d and 1 (a poll the quota refuses must not be recorded)", polls, continues, cap)
			}

			// A guest that ignores the stop and polls again is refused again, and still records nothing.
			before := len(s.history)
			if got := tc.poll(s); got != callSuspendSentinel || len(s.history) != before {
				t.Fatalf("a second poll past the cap returned %#x and grew the history from %d to %d; want the stop and no growth", got, before, len(s.history))
			}
		})
	}
}

// CONTROL: with no cap configured a poll loop is unbounded, as it was. The cap is opt-in.
func TestANoWaitPollWithNoEventCapIsUnbounded(t *testing.T) {
	s, _ := originalRun(t, WithPromiseStore(&mockPromiseStore{status: ""}))
	for i := 0; i < 50; i++ {
		if got := s.AwaitPromise(hostCtx(), nil, "prom-nocap", 0, 0, 256); !timedOutWord(got) {
			t.Fatalf("poll %d returned %#x, want the timed-out word", i, got)
		}
	}
	if len(s.history) != 50 || s.autoContinueAsNewTriggered {
		t.Fatalf("%d events, triggered=%v; want 50 and no continue-as-new", len(s.history), s.autoContinueAsNewTriggered)
	}
}

func countPollEvents(h []EventRecord) (polls, continues int) {
	for _, e := range h {
		switch e.EventType {
		case EventTypeAwaitPromise, EventTypeAwaitSignals:
			polls++
		case EventTypeContinueAsNew:
			continues++
		}
	}
	return
}
