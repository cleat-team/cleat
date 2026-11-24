package engine

// cleat#1331, at the layer the SDK guard cannot reach.
//
// A 0ms await used to suspend with `Until: time.UnixMilli(nowMs).Add(0)` -- a
// deadline of NOW. The run is immediately re-claimable, wakes, replays to this
// step and suspends again: roughly fourteen claims a second, forever, while
// event_history stays constant and reclaim_count stays 0, because every cycle
// is a legitimate claim rather than a stall. Nothing in the system detects it.
//
// WHY THIS TEST EXISTS ALONGSIDE THE SDK ONE, which is the whole point rather
// than duplication. cleat/runtime_signals.go guards Go guests. This function is
// on the public interface and is reached over an ABI shared by every SDK, so a
// Rust, Java, Python or AssemblyScript guest that computes a sub-millisecond
// timeout arrives here with 0 and has nothing between it and the livelock.
// Fixing only the Go SDK would have left four languages broken and this test
// green either way, which is why it asserts on the session rather than on a
// returned error.

import (
	"context"
	"testing"
	"time"
)

func zeroTimeoutSession(t *testing.T, store *awaitProbeStore) *execSession {
	t.Helper()
	eng := NewEngine(nil, &mockCaller{}, WithSignalStore(store), WithWorkflowID("wf-1331"))
	return &execSession{
		engine: eng, workflowID: "wf-1331", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
	}
}

func callAwaitWithTimeout(t *testing.T, s *execSession, timeoutMs int64) (timedOut bool) {
	t.Helper()
	buf := make([]byte, 512)
	c := contextWithRawMemBuf(context.Background(), buf)
	packed := s.DurableAwaitSignals(c, nil, `["never"]`, timeoutMs, 0, 200, 256, 200)
	return (packed>>16)&0xFFFF != 0
}

func TestAZeroTimeoutAwaitReportsTimedOutWithoutSuspending(t *testing.T) {
	for _, timeoutMs := range []int64{0, -1} {
		t.Run(time.Duration(timeoutMs).String(), func(t *testing.T) {
			s := zeroTimeoutSession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}})

			timedOut := callAwaitWithTimeout(t, s, timeoutMs)

			if s.suspendErr != nil {
				t.Errorf("a %dms await suspended, with Until=%s against now=%s.\n\n"+
					"A deadline that is not in the future makes the run immediately "+
					"re-claimable: it wakes, replays to this step and suspends again, "+
					"indefinitely, without advancing a single event (cleat#1331).",
					timeoutMs, s.suspendErr.Until, time.UnixMilli(s.nowMs))
			}
			if !timedOut {
				t.Errorf("a %dms await did not report timedOut; a wait with no deadline to "+
					"expire has already expired", timeoutMs)
			}
			// ONE event is recorded, carrying the timeout (cleat#4153, reversing cleat#1331's choice, which this test used to pin as
			// "no event recorded"). That choice worried that a poll loop would grow event_history without bound. It does grow by one
			// row per poll, as any durable call in a loop does, and the event cap and compaction bound it. The alternative was
			// measured to be wrong: with no event, a replay that ran after a signal arrived returned the signal where the original
			// run had been told it timed out, and a replay that found a LATER event at this step fell out of the replay branch with
			// history unread and re-ran every durable call recorded after the poll. See
			// a_no_wait_await_replays_as_the_timeout_it_was_test.go for those cases, replayed.
			if len(s.history) != 1 || s.history[0].EventType != EventTypeAwaitSignals || s.history[0].TimeoutMs != timeoutMs {
				t.Errorf("a %dms await recorded %+v, want exactly one await_signals event carrying the timeout", timeoutMs, s.history)
			}
		})
	}
}

// A 0ms await degrades to PollSignals, not to nothing. The guard sits AFTER the
// signal-store check, so a signal already waiting is still delivered -- which is
// both the useful behaviour and the reason the guard is not at the top of the
// function.
func TestAZeroTimeoutAwaitStillReportsASignalAlreadyWaiting(t *testing.T) {
	store := &awaitProbeStore{deliveries: map[string]SignalDelivery{
		"never": {Payload: `{"v":1}`},
	}}
	s := zeroTimeoutSession(t, store)

	if timedOut := callAwaitWithTimeout(t, s, 0); timedOut {
		t.Error("a 0ms await reported timedOut while the store held a delivery.\n\n" +
			"Refusing before the store check would make a 0ms await strictly useless; " +
			"after it, the await degrades to a non-blocking poll (cleat#1331).")
	}
}

// The negative control. Without it, a guard that refused EVERY await would pass
// both assertions above -- and refusing every await is exactly the shape of the
// defect this replaces.
func TestAPositiveTimeoutAwaitStillSuspends(t *testing.T) {
	s := zeroTimeoutSession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}})

	callAwaitWithTimeout(t, s, 1)

	if s.suspendErr == nil {
		t.Fatal("a 1ms await did not suspend; an unresolved await with a future deadline " +
			"must yield the worker rather than return")
	}
	if !s.suspendErr.Until.After(time.UnixMilli(s.nowMs)) {
		t.Errorf("a 1ms await suspended until %s, which is not after now=%s",
			s.suspendErr.Until, time.UnixMilli(s.nowMs))
	}
	if len(s.history) != 1 {
		t.Errorf("a 1ms await appended %d history events, want 1", len(s.history))
	}
}
