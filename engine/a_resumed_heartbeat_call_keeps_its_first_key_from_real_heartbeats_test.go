package engine

// cleat#4247. The golden table's two rows for a resumed heartbeat call (replay_mid_heartbeat_resumes_the_call, replay_mid_heartbeat_after_a_plain_call)
// drive the resume from a HAND-WRITTEN history, hb(0), hb(1). They show the fix reads the first heartbeat's Step. They cannot show that a REAL first
// incarnation's first heartbeat carries the step its idempotency key was derived from, which is the claim the fix rests on: a test that writes the
// producer's shape by hand cannot see the producer.
//
// This test runs the producer. The first incarnation is a real fresh call that really ticks; its recorded heartbeats, with the terminal event dropped
// (the worker was lost before writing it), are the history the resume replays; and the key the resume's call carries is compared with the key the first
// incarnation's call carried. Both shapes are run: a heartbeat call that starts at step 0, and one that follows a plain call and starts at step 1.
//
// The first incarnation's call is held long enough for several ticks, so a call that ticks is what is measured. If fewer than two heartbeats were
// recorded the precondition did not hold and the test says so rather than passing: with no heartbeats there is nothing for a resume to recover.

import (
	"context"
	"testing"
	"time"
)

// g3hSleepingCaller holds each svc.op call for hold, then answers it as inner does. Only svc.op is held.
type g3hSleepingCaller struct {
	inner *g3hCaller
	hold  time.Duration
}

func (c *g3hSleepingCaller) Call(ctx context.Context, service, operation, req string) (string, error) {
	return c.CallWithIdempotencyKey(ctx, service, operation, req, "")
}

func (c *g3hSleepingCaller) CallWithIdempotencyKey(ctx context.Context, service, operation, req, key string) (string, error) {
	if service == "svc" && operation == "op" {
		select {
		case <-time.After(c.hold):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return c.inner.CallWithIdempotencyKey(ctx, service, operation, req, key)
}

func TestAResumedHeartbeatCallCarriesTheKeyOfItsFirstIncarnationFromRealHeartbeats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix bool // a plain svc.before call first, so the heartbeat call starts at step 1
	}{
		{"starts_at_step_0", false},
		{"starts_after_a_plain_call", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const wf = "wf-4247"
			buf := make([]byte, g3hBufLen)
			ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)

			// The first incarnation: real, fresh, ticking.
			first := &g3hCaller{mode: "ok"}
			first.learnKeys(wf)
			s1 := &execSession{
				engine:     NewEngine(nil, &g3hSleepingCaller{inner: first, hold: 150 * time.Millisecond}, WithWorkflowID(wf)),
				workflowID: wf, nowMs: time.Now().UnixMilli(), deferrals: map[string]string{}, queryState: map[string]string{},
			}
			if tc.prefix {
				s1.DurableCall(ctx, nil, "svc", "before", `{}`, 0, g3hBufLen)
			}
			startStep := s1.stepCount
			s1.DurableCallWithHeartbeat(ctx, nil, "svc", "op", `{}`, 2, 0, g3hBufLen)

			if len(first.steps) != 1 || first.steps[0] != startStep {
				t.Fatalf("PRECONDITION: the first incarnation's call carried a key from step(s) %v, want exactly [%d] (the step the call started at)", first.steps, startStep)
			}
			// The history a worker lost before writing the terminal event leaves behind: everything up to, not including, the svc.op call record.
			var lost []EventRecord
			beats := 0
			for _, r := range s1.history {
				if r.EventType == EventTypeCall && r.Service == "svc" && r.Op == "op" {
					break
				}
				if r.EventType == EventTypeHeartbeat {
					beats++
				}
				lost = append(lost, r)
			}
			if beats < 2 {
				t.Fatalf("PRECONDITION: the first incarnation recorded %d heartbeat(s) in a 150 ms call at a 2 ms interval, want at least 2; "+
					"with fewer there is nothing for the resume to recover, so this run measured nothing", beats)
			}

			// The resume: replays that history, then runs the call again.
			second := &g3hCaller{mode: "ok"}
			second.learnKeys(wf)
			s2 := &execSession{
				engine:     NewEngine(nil, second, WithWorkflowID(wf)),
				workflowID: wf, nowMs: time.Now().UnixMilli(), deferrals: map[string]string{}, queryState: map[string]string{},
				isReplay: true, history: append([]EventRecord(nil), lost...),
			}
			buf2 := make([]byte, g3hBufLen)
			ctx2 := context.WithValue(context.Background(), wasmMemBufKey{}, buf2)
			if tc.prefix {
				s2.DurableCall(ctx2, nil, "svc", "before", `{}`, 0, g3hBufLen)
			}
			s2.DurableCallWithHeartbeat(ctx2, nil, "svc", "op", `{}`, g3hHour, 0, g3hBufLen)

			if len(second.steps) != 1 || second.steps[0] != startStep {
				t.Fatalf("the resumed call carried a key from step(s) %v after replaying %d real heartbeats, want [%d]: the first incarnation's key. "+
					"(-1 means a key from none of steps 0..9, which is what a key derived AFTER many consumed heartbeats looks like.) "+
					"A different key lets a service that deduplicates on it perform the call again", second.steps, beats, startStep)
			}
		})
	}
}
