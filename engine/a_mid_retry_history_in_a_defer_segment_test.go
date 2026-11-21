package engine

// cleat#4152, the row its tests left out. A run lost INSIDE a retry policy leaves call_attempt_failed events and no terminal call event, and
// DurableCallWithRetry resumes the policy from them (cleat#1145). That resume leaves replay and then meets the dispatcher's refusal (it was the call's own
// stopBeforeNewWork before cleat#4152 G3), so what a segment
// over such a history does is a separate fact from what it does at the frontier of a history that ends cleanly, which
// a_retry_policy_applies_at_the_replay_frontier_test.go covers. Nothing there puts a defer segment over a history that ends on an attempt.
//
//	control         an ordinary resume: attempts already spent are resumed, not restarted. It shows this history does resume, so the segment
//	                row below cannot pass by the history being unreadable
//	defer segment   no new work: the stop sentinel, nothing recorded, the service never reached
//	defer body      a cleanup's retried call resumes and finishes the policy
import (
	"context"
	"testing"
)

func TestAMidRetryHistoryIsRefusedInADeferSegmentAndResumedInADeferBody(t *testing.T) {
	ctx := ctxWithMem(context.Background(), make([]byte, 1<<16))
	before := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "before", Request: `{}`, Response: `{"ok":true}`}
	spent := EventRecord{Step: 1, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op", Request: `{}`, Err: "boom", Attempt: 1}
	sh := retryShape{maxAttempts: 3, nonRetryable: `[]`}
	history := func() []EventRecord { return []EventRecord{before, spent} }

	t.Run("control: an ordinary resume finishes the policy", func(t *testing.T) {
		c := &scriptedCaller{failFirst: 1 << 20}
		s := retrySession{replaying: true, history: history()}.build(c)
		guestBody(ctx, s, sh)
		if c.attempts != 2 {
			t.Fatalf("the CONTROL made %d further attempt(s) from one recorded, want 2 (a policy of 3): this history does not resume, so the rows below prove nothing", c.attempts)
		}
	})

	t.Run("a defer segment's retried call is refused: no attempt, nothing recorded", func(t *testing.T) {
		c := &scriptedCaller{failFirst: 1 << 20}
		s := retrySession{replaying: true, history: history(), deferPhase: true}.build(c)
		_, retried := guestBody(ctx, s, sh)
		if retried != callSuspendSentinel {
			t.Errorf("a defer segment over a mid-retry history returned %#x, want the stop sentinel %#x", retried, callSuspendSentinel)
		}
		if c.attempts != 0 || len(s.history) != 2 {
			t.Errorf("the refused call reached the service %d time(s) and left %d event(s); want 0 and the 2 replayed ones", c.attempts, len(s.history))
		}
	})

	t.Run("a defer body's retried call resumes and finishes the policy", func(t *testing.T) {
		c := &scriptedCaller{failFirst: 1 << 20}
		s := retrySession{replaying: true, history: history(), deferPhase: true, draining: true}.build(c)
		_, retried := guestBody(ctx, s, sh)
		if retried == callSuspendSentinel || c.attempts != 2 {
			t.Errorf("a defer body's retried call returned %#x after %d further attempt(s); want a result after 2 (a policy of 3, one spent)", retried, c.attempts)
		}
	})
}
