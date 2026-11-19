package engine

import (
	"context"
	"testing"
	"time"
)

// TestAnAwaitPromiseWithNoTimeoutDoesNotWait pins what timeout_ms <= 0 means for await_promise: DO NOT WAIT, which is what it means for
// await_signals (cleat#1331) and for a durable sleep. The call reports a timeout and the run is not suspended.
//
// It used to suspend with a deadline of NOW. A fresh await recorded an await_promise event and set Until = nowMs + 0; the next claim
// replayed to that event, reached the resuming branch (awaitTimeoutMs <= 0, "keep waiting rather than inventing a deadline"), and set
// Until = time.Now() + 0 again. Each wake was a legitimate claim that changed nothing, so the run was re-claimable at once, forever:
// the loop cleat#1331 measured for await_signals at about fourteen claims a second, with event_history constant. Every SDK passes the
// guest's value through, and a guest that computes a sub-millisecond timeout (int(seconds * 1000) of 0.0004) arrives with 0.
func TestAnAwaitPromiseWithNoTimeoutDoesNotWait(t *testing.T) {
	const promiseID = "prom-await-zero"
	timedOut := packAwaitPromiseResult(0, true, 0)

	cases := []struct {
		name string
		// recorded is the timeout on an await_promise event already in history (a resume), or nil for a fresh await.
		recorded *int64
		arg      int64
		status   string
		// wantWait says the call must suspend: a CONTROL row, there so that "did not suspend" can be told from a session that could never
		// suspend at all.
		wantWait bool
		// wantSettled says the call returns the promise's value, not a timeout.
		wantSettled bool
	}{
		{name: "fresh/0", arg: 0},
		{name: "fresh/-1", arg: -1},
		{name: "resume/recorded 0, arg 0", recorded: ptr64(0), arg: 0},
		{name: "resume/recorded 0, arg -1", recorded: ptr64(0), arg: -1},

		// A promise that is already settled is reported, as await_signals reports a signal that is already waiting: a 0 ms await degrades
		// to a poll, not to nothing.
		{name: "fresh/0/resolved", arg: 0, status: "resolved", wantSettled: true},
		{name: "resume/recorded 0, arg 0/resolved", recorded: ptr64(0), arg: 0, status: "resolved", wantSettled: true},

		// CONTROLS.
		{name: "CONTROL fresh/5s waits", arg: 5000, wantWait: true},
		{name: "CONTROL resume/recorded 5s, arg 5s waits out the REST of the deadline", recorded: ptr64(5000), arg: 5000, wantWait: true},
		// A history written before the timeout was persisted has a recorded 0 that means "unknown", not "no wait", and the guest's own
		// argument is all there is to go on. That row keeps waiting, as it always did.
		{name: "CONTROL resume/recorded 0 (legacy), arg 5s keeps waiting", recorded: ptr64(0), arg: 5000, wantWait: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestExecSession()
			s.nowMs = time.Now().UnixMilli()
			s.engine.promiseStore = &mockPromiseStore{status: tc.status, result: `{"ok":true}`}
			if tc.recorded != nil {
				s.isReplay = true
				s.history = []EventRecord{{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: promiseID, TimeoutMs: *tc.recorded, TimestampMs: s.nowMs}}
			}
			before := len(s.history)

			got := s.AwaitPromise(context.Background(), nil, promiseID, tc.arg, 0, 0)

			if tc.wantWait {
				if s.suspendErr == nil {
					t.Fatalf("CONTROL: a pending await with a timeout of %d ms did not suspend (returned %#x): the rows above measure nothing", tc.arg, got)
				}
				return
			}
			if s.suspendErr != nil {
				t.Fatalf("await_promise(%d ms) suspended the run until %v (%v from now), want it not to wait. A deadline of NOW is re-claimable "+
					"at once, wakes, replays to this event and suspends again with a deadline of now: the cleat#1331 loop", tc.arg,
					s.suspendErr.Until.Format(time.RFC3339Nano), time.Until(s.suspendErr.Until).Round(time.Millisecond))
			}
			if tc.wantSettled {
				if got == timedOut {
					t.Fatalf("a promise that was already settled was reported as a timeout: a 0 ms await must still see it")
				}
				return
			}
			if got != timedOut {
				t.Fatalf("await_promise(%d ms) returned %#x, want the timed-out word %#x", tc.arg, got, timedOut)
			}
			if tc.recorded == nil && len(s.history) != before {
				t.Fatalf("a no-wait await recorded %d event(s): a loop of them would grow event_history without bound (cleat#1331)", len(s.history)-before)
			}
		})
	}
}

func ptr64(v int64) *int64 { return &v }
