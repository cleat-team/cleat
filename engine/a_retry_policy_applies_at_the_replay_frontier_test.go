package engine

// cleat#4152. A call made under a retry policy (cleat_call_retry -> DurableCallWithRetry) must get its WHOLE policy wherever it arrives.
//
// A session over a run with history starts in replay and stays there until something leaves it. A retried call that follows the last
// replayed event directly (a run lost, reclaimed or shut down after a recorded step, with no wait between) arrives still replaying, with
// nothing left to replay: the replay frontier. replayRetryAttempts finds no attempts to resume there, and the call used to go to replayCall,
// which makes ONE plain call. Measured on a real worker (cmd/cleat-worker/a_retry_policy_survives_a_resume_on_a_real_worker_test.go): the
// service saw one attempt of five and the run failed on its first failure, where the same call after a wait, or in the run's first segment,
// retried to success.
//
// THE ASSERTION THAT HOLDS THIS TOGETHER IS EQUIVALENCE, not an attempt count. A frontier call and the same call on a fresh session must leave
// the SAME events behind (type, step, attempt, service, op) and reach the service the same number of times. A count alone passes for the
// wrong reason when a policy happens to make one attempt anyway (a first attempt that succeeds); equivalence over the whole shape table does not.
//
// THE SESSIONS, because each one rules out a way for the table to pass without testing anything:
//
//	fresh        an ordinary session with nothing to replay. The CONTROL: it shows the probe sees the policy run.
//	frontier     a replaying session whose history ends right before the retried call. The subject.
//	segment      the same, as a terminate's defer segment: no new work, and nothing recorded, not even a refusal.
//	defer body   the same segment draining the guest's own defers. Its retried call must get the whole policy.
//
// plus the neighbours the change must not disturb: a recorded terminal call is replayed and not re-run, a recorded step for another call is
// still a divergence, and a refusal recorded at the frontier is read back by the next segment.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// scriptedCaller fails its first failFirst attempts at svc.op with a retryable error and answers everything else, counting attempts.
type scriptedCaller struct {
	failFirst int
	attempts  int // attempts at svc.op
	others    int // every other call
}

func (c *scriptedCaller) Call(_ context.Context, service, operation, _ string) (string, error) {
	if service != "svc" || operation != "op" {
		c.others++
		return `{"ok":true}`, nil
	}
	c.attempts++
	if c.attempts <= c.failFirst {
		// A ServiceError with a code, so a policy's non-retryable list can match it; with an empty list it is retried like any other.
		return "", &ServiceError{Code: "TRY_AGAIN", Message: "503 try again"}
	}
	return `{"ok":true}`, nil
}

type retryShape struct {
	name         string
	failFirst    int
	maxAttempts  int64
	nonRetryable string
	maxRetries   int // engine ceiling, 0 = default
	// wantAttempts and wantOK are the control's expected outcome, asserted so a shape cannot go vacuous.
	wantAttempts int
	wantOK       bool
}

func retryShapes() []retryShape {
	return []retryShape{
		{name: "fails twice then succeeds", failFirst: 2, maxAttempts: 5, nonRetryable: `[]`, wantAttempts: 3, wantOK: true},
		{name: "always fails", failFirst: 1 << 20, maxAttempts: 3, nonRetryable: `[]`, wantAttempts: 3, wantOK: false},
		{name: "first attempt succeeds", failFirst: 0, maxAttempts: 5, nonRetryable: `[]`, wantAttempts: 1, wantOK: true},
		{name: "non-retryable failure stops at one", failFirst: 1 << 20, maxAttempts: 5, nonRetryable: `["TRY_AGAIN"]`, wantAttempts: 1, wantOK: false},
		{name: "attempt ceiling bounds the policy", failFirst: 1 << 20, maxAttempts: 1000, nonRetryable: `[]`, maxRetries: 4, wantAttempts: 4, wantOK: false},
	}
}

type retrySession struct {
	replaying  bool
	history    []EventRecord
	deferPhase bool
	draining   bool
	budget     time.Duration
	maxRetries int
}

func (r retrySession) build(c *scriptedCaller) *execSession {
	opts := []EngineOption{WithWorkflowID("retry-frontier")}
	if r.deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	if r.budget > 0 {
		opts = append(opts, WithHostRetryBudget(r.budget))
	}
	eng := NewEngine(nil, c, opts...)
	if r.maxRetries > 0 {
		eng.maxRetries = r.maxRetries
	}
	return &execSession{
		engine:       eng,
		workflowID:   "retry-frontier",
		nowMs:        time.Now().UnixMilli(),
		deferrals:    map[string]string{},
		queryState:   map[string]string{},
		isReplay:     r.replaying,
		history:      r.history,
		inDeferDrain: r.draining,
	}
}

// guestBody is the workflow: a plain call, then the retried one. Run against a session whose history already holds the first call, it replays
// it and meets the retried call at the frontier.
func guestBody(ctx context.Context, s *execSession, sh retryShape) (first, retried int64) {
	first = s.DurableCall(ctx, nil, "svc", "before", `{}`, 0, 4096)
	retried = s.DurableCallWithRetry(ctx, nil, "svc", "op", `{}`, sh.maxAttempts, 1, 100, 1, sh.nonRetryable, 0, 4096)
	return first, retried
}

// callSucceeded reads a packed result the way the guest adapter does: the write error code in the low byte is zero on success.
func callSucceeded(result int64) bool {
	_, _, errCode := decodeDurableCallResult(result)
	return errCode == 0
}

// shapeOf is what an event is, without the things that legitimately differ between two runs (timestamps, checksums).
func shapeOf(events []EventRecord) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s step=%d attempt=%d %s.%s", e.EventType, e.Step, e.Attempt, e.Service, e.Op))
	}
	return out
}

func sameShape(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestARetriedCallAtTheReplayFrontierGetsItsWholePolicy(t *testing.T) {
	for _, sh := range retryShapes() {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			ctx := ctxWithMem(context.Background(), make([]byte, 1<<16))

			// The control: a fresh session runs the whole body. Its history is the real writer's output, and cutting it after the first call
			// gives the history of a run lost right there -- no hand-written event to agree with itself.
			controlCaller := &scriptedCaller{failFirst: sh.failFirst}
			control := retrySession{maxRetries: sh.maxRetries}.build(controlCaller)
			_, ctrlRetried := guestBody(ctx, control, sh)
			ctrlOK := callSucceeded(ctrlRetried)
			if controlCaller.attempts != sh.wantAttempts || ctrlOK != sh.wantOK {
				t.Fatalf("the CONTROL made %d attempt(s) and ok=%v; want %d and ok=%v. The fresh path does not behave as this shape says, "+
					"so the shape proves nothing", controlCaller.attempts, ctrlOK, sh.wantAttempts, sh.wantOK)
			}
			if len(control.history) < 2 {
				t.Fatalf("PRECONDITION FAILED: the control recorded %d event(s); a cut after the first call leaves nothing for the retried call", len(control.history))
			}
			prefix := control.history[:1]
			if prefix[0].EventType != EventTypeCall || prefix[0].Op != "before" {
				t.Fatalf("PRECONDITION FAILED: the control's first event is %s %s.%s, not the plain call", prefix[0].EventType, prefix[0].Service, prefix[0].Op)
			}
			want := shapeOf(control.history[1:])

			t.Run("frontier: the same events and the same attempts as the fresh session", func(t *testing.T) {
				c := &scriptedCaller{failFirst: sh.failFirst}
				s := retrySession{replaying: true, history: append([]EventRecord(nil), prefix...), maxRetries: sh.maxRetries}.build(c)
				_, retried := guestBody(ctx, s, sh)
				got := shapeOf(s.history[1:])
				if c.attempts != sh.wantAttempts || callSucceeded(retried) != sh.wantOK || !sameShape(got, want) {
					t.Errorf("a retried call at the replay frontier did not get its policy.\n  attempts: %d (fresh session: %d)\n  ok:       %v (fresh session: %v)\n"+
						"  events:   %v\n  fresh:    %v\n\nThe call arrived on a replaying session with nothing left to replay and was sent to replayCall, "+
						"which makes ONE plain call.", c.attempts, sh.wantAttempts, callSucceeded(retried), sh.wantOK, got, want)
				}
			})

			t.Run("segment: no new work, nothing recorded, nothing sent", func(t *testing.T) {
				c := &scriptedCaller{failFirst: sh.failFirst}
				s := retrySession{replaying: true, history: append([]EventRecord(nil), prefix...), deferPhase: true, maxRetries: sh.maxRetries}.build(c)
				_, retried := guestBody(ctx, s, sh)
				if retried != callSuspendSentinel {
					t.Errorf("a defer segment's retried call at the frontier returned %#x, want the stop sentinel %#x", retried, callSuspendSentinel)
				}
				if len(s.history) != 1 || c.attempts != 0 {
					t.Errorf("a refused retried call left events %v and reached the service %d time(s); it must record nothing and send nothing",
						shapeOf(s.history[1:]), c.attempts)
				}
			})

			t.Run("defer body: a cleanup's retried call gets its whole policy", func(t *testing.T) {
				c := &scriptedCaller{failFirst: sh.failFirst}
				s := retrySession{replaying: true, history: append([]EventRecord(nil), prefix...), deferPhase: true, draining: true, maxRetries: sh.maxRetries}.build(c)
				_, retried := guestBody(ctx, s, sh)
				if retried == callSuspendSentinel || c.attempts != sh.wantAttempts || !sameShape(shapeOf(s.history[1:]), want) {
					t.Errorf("a defer body's retried call (inDeferDrain) got attempts=%d (want %d) and events %v (want %v), returned %#x",
						c.attempts, sh.wantAttempts, shapeOf(s.history[1:]), want, retried)
				}
			})
		})
	}
}

// The budget check sits on the fresh branch, and a call at the frontier was never judged by it. A policy too long to run in one segment must be
// REFUSED there exactly as it is on a fresh session: the refusal recorded, the service untouched, and the next segment reading the refusal back.
func TestAPolicyTooLongForTheBudgetIsRefusedAtTheReplayFrontierToo(t *testing.T) {
	ctx := ctxWithMem(context.Background(), make([]byte, 1<<16))
	call := func(s *execSession) int64 {
		// Ten attempts at a minute: a worst case of nine minutes against a one-second budget.
		first := s.DurableCall(ctx, nil, "svc", "before", `{}`, 0, 4096)
		_ = first
		return s.DurableCallWithRetry(ctx, nil, "svc", "op", `{}`, 10, 60000, 100, 0, `[]`, 0, 4096)
	}

	fc := &scriptedCaller{}
	fresh := retrySession{budget: time.Second}.build(fc)
	if got := byte(call(fresh) >> 8); got != callErrorRetryPolicyTooLong {
		t.Fatalf("the CONTROL (a fresh session) answered %d, want callErrorRetryPolicyTooLong (%d): the policy does not exceed the budget, so this row proves nothing", got, callErrorRetryPolicyTooLong)
	}
	prefix := fresh.history[:1]

	c := &scriptedCaller{}
	s := retrySession{replaying: true, history: append([]EventRecord(nil), prefix...), budget: time.Second}.build(c)
	if got := byte(call(s) >> 8); got != callErrorRetryPolicyTooLong {
		t.Errorf("a policy too long for the budget was NOT refused at the replay frontier (callErrorCode %d, want %d): the budget check ran on the fresh branch only", got, callErrorRetryPolicyTooLong)
	}
	if c.attempts != 0 {
		t.Errorf("the service was reached %d time(s) by a refused policy", c.attempts)
	}
	if want, got := shapeOf(fresh.history[1:]), shapeOf(s.history[1:]); !sameShape(got, want) {
		t.Errorf("the refusal left events %v, the fresh session left %v: the next segment cannot read it back", got, want)
	}

	// The next segment reads that refusal back: it refuses again without a budget in sight, which is what cleat#4032's marker exists for.
	nc := &scriptedCaller{}
	next := retrySession{replaying: true, history: append([]EventRecord(nil), s.history...)}.build(nc)
	if got := byte(call(next) >> 8); got != callErrorRetryPolicyTooLong || nc.attempts != 0 {
		t.Errorf("replaying the recorded refusal answered %d after %d attempt(s), want %d and none", got, nc.attempts, callErrorRetryPolicyTooLong)
	}

	// A defer segment at the same frontier is refused first and records NO refusal: a stop is not a field (ABI.md).
	sc := &scriptedCaller{}
	seg := retrySession{replaying: true, history: append([]EventRecord(nil), prefix...), budget: time.Second, deferPhase: true}.build(sc)
	if got := call(seg); got != callSuspendSentinel || len(seg.history) != 1 {
		t.Errorf("a defer segment answered %#x and left %d event(s); want the stop sentinel %#x and only the replayed call", got, len(seg.history), callSuspendSentinel)
	}
}

// What the change must NOT disturb: a recorded step is still read.
func TestARecordedStepAtTheRetriedCallIsStillRead(t *testing.T) {
	ctx := ctxWithMem(context.Background(), make([]byte, 1<<16))
	before := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "before", Request: `{}`, Response: `{"ok":true}`}
	sh := retryShape{maxAttempts: 5, nonRetryable: `[]`}

	t.Run("a recorded terminal call is replayed, not run again", func(t *testing.T) {
		c := &scriptedCaller{}
		s := retrySession{replaying: true, history: []EventRecord{before,
			{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{"ok":"recorded"}`}}}.build(c)
		_, retried := guestBody(ctx, s, sh)
		if !callSucceeded(retried) || c.attempts != 0 || s.stepCount != 2 {
			t.Errorf("a recorded call was not replayed as recorded: ok=%v, %d service attempt(s), stepCount %d (want true, 0, 2)", callSucceeded(retried), c.attempts, s.stepCount)
		}
	})

	t.Run("a recorded step for another call is still a divergence", func(t *testing.T) {
		c := &scriptedCaller{}
		s := retrySession{replaying: true, history: []EventRecord{before,
			{Step: 1, EventType: EventTypeCall, Service: "other", Op: "op", Request: `{}`, Response: `{}`}}}.build(c)
		_, retried := guestBody(ctx, s, sh)
		if callSucceeded(retried) || c.attempts != 0 {
			t.Errorf("a divergent history was not reported: ok=%v, %d service attempt(s)", callSucceeded(retried), c.attempts)
		}
	})

	t.Run("attempts already spent are resumed, not restarted", func(t *testing.T) {
		c := &scriptedCaller{failFirst: 1 << 20}
		s := retrySession{replaying: true, history: []EventRecord{before,
			{Step: 1, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op", Request: `{}`, Err: "boom", Attempt: 1}}}.build(c)
		sh := retryShape{maxAttempts: 3, nonRetryable: `[]`}
		guestBody(ctx, s, sh)
		if c.attempts != 2 {
			t.Errorf("a policy of 3 with one attempt recorded made %d further attempt(s), want 2 (cleat#1145)", c.attempts)
		}
	})
}
