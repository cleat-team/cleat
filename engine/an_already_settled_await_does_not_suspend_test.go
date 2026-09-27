package engine

import (
	"context"
	"testing"
)

// TestAnAlreadySettledAwaitDoesNotSuspend asserts that awaiting a promise the
// store already reports as settled returns without suspending the workflow.
//
// # Why this is not covered by the tests that look like it
//
// The settled branches are already guarded. Disabling the `resolved` branch
// in promises.go turns three existing tests red -- TestAwaitPromiseFreshResolved,
// TestAwaitPromiseReplayAwaitThenFreshResolved and TestAwaitPromise_FreshResolved
// -- and the rejected branch has its own pair. Measured, not assumed.
//
// What none of them asserts is that the session was not ALSO left suspended,
// because each compares the packed return value and stops there.
//
// Those are different properties, and the return value is the weaker one.
// executor.go decides on the session, not on what the host call returned:
//
//	if (res != nil && res.Suspended) || session.suspendErr != nil {
//
// So a settled await that returns its value correctly AND sets suspendErr
// suspends the workflow anyway, with the right value in hand. Measured
// 2026-09-17 by doing exactly that -- returning the settled result unchanged
// and setting suspendErr beside it, on both branches. Every promise, await,
// suspend and resume test in the package stayed green:
//
//	go test ./engine/ -run 'Promise|Await|Suspend|Resume' -count=1   # ok
//
// cleat#1754 is what sent someone looking here. Its measurement was retracted
// by its author (the interval measured a durable sleep's wake latency, not the
// await), so this test is not a fix for a defect; it is the guard whose absence
// that investigation turned up. The claim under examination there was "an
// already-settled await suspends", and nothing in this package could have
// answered it.
//
// # The control
//
// The pending rows are why the others mean anything. Without a case that DOES
// suspend, "suspendErr is nil" is equally satisfied by a session that could
// never have set it. There is one per route, and they fail if the assertion
// below stops being able to fire.
func TestAnAlreadySettledAwaitDoesNotSuspend(t *testing.T) {
	const promiseID = "prom-1754"

	cases := []struct {
		name string
		// resumingAwait puts an await_promise event in history, so the call
		// arrives through the replay path that carries the original deadline
		// forward. That is the ordering a durable sleep before the await
		// produces, and it reaches the settled check by a different route.
		resumingAwait bool
		// noRecordedTimeout writes the await event with TimeoutMs unset, the
		// shape left by an execution that predates the timeout being
		// persisted. See the control rows below.
		noRecordedTimeout bool
		status            string
		wantSuspend       bool
	}{
		{name: "fresh/resolved", status: "resolved"},
		{name: "fresh/rejected", status: "rejected"},
		{name: "resuming/resolved", resumingAwait: true, status: "resolved"},
		{name: "resuming/rejected", resumingAwait: true, status: "rejected"},

		// CONTROLS. A promise still pending has something to wait for, so
		// these rows must suspend. They are not cases under test -- they are
		// the proof that the rows above are being measured at all, one per
		// route into the settled check.
		//
		// The resuming control records NO timeout, which takes the
		// "recorded before the timeout was persisted" branch and suspends
		// unconditionally. That is deliberate: a resume carrying a live
		// deadline suspends too, but only while the deadline is in the
		// future, so writing it needs the wall clock and an arbitrary
		// margin. The first draft of this row instead set TimeoutMs without
		// TimestampMs -- an anchor of 0 puts the deadline in 1970, so the
		// call reports a TIMEOUT and returns without suspending, and the
		// control failed. Worth knowing before writing any resume-path test:
		// a recorded await with no anchor is already expired.
		{name: "fresh/pending (control)", status: "", wantSuspend: true},
		{name: "resuming/pending (control)", resumingAwait: true, noRecordedTimeout: true, status: "", wantSuspend: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestExecSession()
			s.engine.promiseStore = &mockPromiseStore{
				status: tc.status,
				result: `{"status":"ok"}`,
				errMsg: "bad request",
			}
			if tc.resumingAwait {
				rec := EventRecord{
					Step:      0,
					EventType: EventTypeAwaitPromise,
					PromiseID: promiseID,
					TimeoutMs: 5000,
				}
				if tc.noRecordedTimeout {
					rec.TimeoutMs = 0
				}
				s.isReplay = true
				s.history = []EventRecord{rec}
			}

			// A nil module makes writeResult a no-op. Nothing here asserts on
			// the value, only on whether the session was left suspended.
			s.AwaitPromise(context.Background(), nil, promiseID, 5000, 0, 0)

			switch {
			case tc.wantSuspend && s.suspendErr == nil:
				t.Fatalf("a promise still pending did not suspend.\n\n" +
					"This row is the control: it exists so the settled rows mean " +
					"something. With it passing vacuously, `suspendErr == nil` is " +
					"satisfied by a session that never sets suspendErr at all, and " +
					"the other rows stop being evidence.")
			case !tc.wantSuspend && s.suspendErr != nil:
				t.Fatalf("awaiting an already-%s promise left the session suspended: %q\n\n"+
					"The value may still be returned correctly -- executor.go does not "+
					"consult the return value:\n\n"+
					"\tif (res != nil && res.Suspended) || session.suspendErr != nil {\n\n"+
					"so a settled await that sets suspendErr costs a suspend and a "+
					"replay for a call that had nothing to wait for. cleat#1754.",
					tc.status, s.suspendErr.Reason)
			}
		})
	}
}
