package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#4277. A workflow awaits promise P with a timeout and the await TIMES OUT (a timeout records nothing but the await). The guest's very next call
// is a second await of P, and P has settled in the instant between the two. The second await finds it settled on its first read, which records a
// resolution with no await before it. So the history is
//
//	[await P, promise_resolved P]
//
// -- byte for byte what ONE await records when it is answered on a wake. A replay cannot tell the two apart, and handed the FIRST await the resolution
// where the original run had been told it timed out: a guest that counts its timeouts, or branches on one, then takes a different path on replay.
//
// This drives two calls on one session (the store flipping to settled between them) and then replays the history that session wrote on a fresh one,
// which is what a worker does when it resumes the run. The control is the legitimate shape: ONE await answered on a wake records the same two events and
// must keep replaying as the resolution.
func TestATimedOutAwaitFollowedByASettledAwaitReplaysAsTwoCalls(t *testing.T) {
	const timeoutMs = int64(100)
	longAgo := time.Now().UnixMilli() - 3_600_000 // the original await began an hour ago: its 100 ms deadline is long past

	newSession := func(w *promiseWorld, hist []EventRecord) *execSession {
		return &execSession{
			engine:     NewEngine(nil, &mockCaller{}, WithClock(func() int64 { return time.Now().UnixMilli() }), WithWorkflowID("wf-promise"), WithPromiseStore(w)),
			workflowID: "wf-promise", nowMs: promiseT, deferrals: map[string]string{}, queryState: map[string]string{},
			isReplay: len(hist) > 0, history: append([]EventRecord(nil), hist...),
		}
	}
	type answer struct {
		timedOut bool
		errCode  uint16
		result   string
	}
	await := func(t *testing.T, s *execSession, timeout int64) answer {
		t.Helper()
		buf := make([]byte, 400)
		got := s.AwaitPromise(contextWithRawMemBuf(context.Background(), buf), nil, "p1", timeout, 0, 200)
		if got == callSuspendSentinel {
			t.Fatalf("the await suspended; this test needs it to answer")
		}
		return answer{timedOut: (uint64(got)>>16)&0xFFFF != 0, errCode: uint16(uint64(got) & 0xFFFF), result: string(buf[:uint32(uint64(got)>>32)])}
	}
	types := func(h []EventRecord) []EventType {
		var out []EventType
		for _, r := range h {
			out = append(out, r.EventType)
		}
		return out
	}

	for _, outcome := range []struct {
		name, status, result, errMsg string
		rejected                     bool
		wantErrCode                  uint16
		wantText                     string
	}{
		{name: "resolved", status: "resolved", result: "v", wantText: "v"},
		{name: "rejected", status: "rejected", errMsg: "boom", rejected: true, wantErrCode: 1, wantText: "boom"},
	} {
		t.Run(outcome.name+": a timed-out await then a second await that finds it settled", func(t *testing.T) {
			// ORIGINAL RUN: the await was recorded and the run suspended; this is the wake that finds its deadline past while P is still pending.
			world := &promiseWorld{status: "pending"}
			orig := newSession(world, []EventRecord{{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: timeoutMs, TimestampMs: longAgo}})
			first := await(t, orig, timeoutMs)
			if !first.timedOut {
				t.Fatalf("PRECONDITION FAILED: the first await answered %+v, want a timeout; the scenario did not start", first)
			}
			world.status, world.result, world.errMsg = outcome.status, outcome.result, outcome.errMsg // P settles between the timeout and the guest's next call
			second := await(t, orig, 60000)
			if second.timedOut || second.result != outcome.wantText || second.errCode != outcome.wantErrCode {
				t.Fatalf("PRECONDITION FAILED: the second await answered %+v, want %q with error code %d", second, outcome.wantText, outcome.wantErrCode)
			}
			written := append([]EventRecord(nil), orig.history...)
			settled := EventTypePromiseResolved
			if outcome.rejected {
				settled = EventTypePromiseRejected
			}
			if !sameEventTypeList(types(written), []EventType{EventTypeAwaitPromise, settled}) {
				t.Fatalf("PRECONDITION FAILED: the run wrote %v, want [await_promise %s], the shape this issue is about", types(written), settled)
			}

			// A fresh replay of what the run wrote, which is what a worker does when it resumes the run.
			replay := newSession(&promiseWorld{status: outcome.status, result: outcome.result, errMsg: outcome.errMsg}, written)
			if got := await(t, replay, timeoutMs); !got.timedOut {
				t.Errorf("the replay answered the FIRST await %+v; the original run was told it timed out", got)
			}
			if got := await(t, replay, 60000); got.timedOut || got.result != outcome.wantText || got.errCode != outcome.wantErrCode {
				t.Errorf("the replay answered the SECOND await %+v, want %q with error code %d", got, outcome.wantText, outcome.wantErrCode)
			}
			if replay.lastAwaitPromiseRejected() != outcome.rejected {
				t.Errorf("the rejection flag after the second await = %v, want %v", replay.lastAwaitPromiseRejected(), outcome.rejected)
			}
			if len(replay.stepViolations) != 0 {
				t.Errorf("the dispatcher noted spec violations: %v", replay.stepViolations)
			}
			if !written[1].FirstRead {
				t.Errorf("the settlement the second await wrote is not marked FirstRead: %+v", written[1])
			}
		})
	}

	t.Run("control: ONE await answered on a wake records the same two events and still replays as the resolution", func(t *testing.T) {
		// The wake finds P already settled at the replay frontier: the answer is recorded after the await, and the guest saw ONE await resolved.
		w := &promiseWorld{status: "resolved", result: "v"}
		woken := newSession(w, []EventRecord{{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 3_600_000, TimestampMs: time.Now().UnixMilli() - 1000}})
		if got := await(t, woken, 3_600_000); got.timedOut || got.result != "v" {
			t.Fatalf("PRECONDITION FAILED: the wake answered %+v, want the resolution", got)
		}
		if !sameEventTypeList(types(woken.history), []EventType{EventTypeAwaitPromise, EventTypePromiseResolved}) {
			t.Fatalf("PRECONDITION FAILED: the wake wrote %v", types(woken.history))
		}
		replay := newSession(&promiseWorld{status: "resolved", result: "v"}, woken.history)
		if got := await(t, replay, 3_600_000); got.timedOut || got.result != "v" {
			t.Errorf("the replay answered the one await %+v, want the resolution: this is the legitimate shape and must not change", got)
		}
	})
}
