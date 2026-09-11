package engine

import (
	"context"
	"errors"
	"testing"
)

// countingCaller fails every call and counts how many it was asked to make.
type countingCaller struct{ n int }

func (c *countingCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	c.n++
	return "", errors.New("connection reset")
}

// MaxAttempts must bound attempts per WORKFLOW, not per incarnation.
//
// cleat#1145. The host retry loop recorded an event only when the call
// finished, so a worker lost mid-backoff replayed into a step with no history,
// restarted the policy at attempt 1, and spent the caller's whole budget a
// second time. Measured on the port harness before the fix: a 3-attempt policy
// made FOUR calls across a crash, against a control of three with no crash.
//
// The control is what makes the number readable. Four is not obviously wrong
// until you know the same policy makes three when nothing is killed -- so both
// halves are asserted here, in one test, for the same reason the original
// measurement needed both.
func TestACrashMidPolicyDoesNotGrantAFreshMaxAttempts(t *testing.T) {
	const maxAttempts = 3

	// Control: no crash. The policy spends exactly its budget.
	control := &countingCaller{}
	cs := &execSession{engine: NewEngine(nil, control)}
	cs.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
		maxAttempts, 1, 100, 1, "", 0, 0)
	if control.n != maxAttempts {
		t.Fatalf("control: %d calls for a %d-attempt policy with no crash", control.n, maxAttempts)
	}

	// The history that run produced: maxAttempts-1 failed attempts, then the
	// terminal call event.
	if len(cs.history) != maxAttempts {
		t.Fatalf("expected %d events (%d attempts + terminal), got %d",
			maxAttempts, maxAttempts-1, len(cs.history))
	}

	// The crash: the worker is lost during the first backoff, so only the
	// first attempt reached the database. Truncating the real history rather
	// than hand-writing one keeps the writer and the reader honest about the
	// shape -- a literal would let this test agree with itself.
	crashed := []EventRecord{cs.history[0]}
	if crashed[0].EventType != EventTypeCallAttemptFailed {
		t.Fatalf("history[0] is %s, so this test is not simulating what it claims",
			crashed[0].EventType)
	}

	resumed := &countingCaller{}
	rs := &execSession{
		engine:   NewEngine(nil, resumed),
		isReplay: true,
		history:  crashed,
	}
	rs.stepCount = 0
	rs.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
		maxAttempts, 1, 100, 1, "", 0, 0)

	if want := maxAttempts - 1; resumed.n != want {
		t.Errorf("a crash after attempt 1 of a %d-attempt policy made %d further calls, want %d.\n\n"+
			"Total across both incarnations: %d. MaxAttempts is the caller's bound on how many "+
			"times a side effect may be attempted, and a crash is not consent to exceed it "+
			"(cleat#1145). The loop reads the recorded call_attempt_failed events to learn how "+
			"much budget is already spent; if it starts at attempt 1 regardless, this is %d.",
			maxAttempts, resumed.n, want, 1+resumed.n, maxAttempts)
	}
}

// Two crashes must not grant two fresh budgets either -- the mechanism has to
// compose, not just work once.
//
// This is the case a counter kept in memory would pass and a recorded history
// would not: after resuming, the second incarnation records ITS failed attempt
// with the next attempt number, so a third incarnation reads two spent and has
// exactly one left. A resumed run that restarted its own numbering would look
// correct here on the first resume and wrong on the second.
func TestASecondCrashResumesFromTheBudgetTheFirstOneLeft(t *testing.T) {
	const maxAttempts = 3

	// Incarnation 1: one attempt, then lost.
	first := &execSession{engine: NewEngine(nil, &countingCaller{})}
	first.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
		maxAttempts, 1, 100, 1, "", 0, 0)
	afterFirstCrash := []EventRecord{first.history[0]}

	// Incarnation 2: resumes, and is lost again after its own attempt.
	second := &countingCaller{}
	rs := &execSession{engine: NewEngine(nil, second), isReplay: true, history: afterFirstCrash}
	rs.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
		maxAttempts, 1, 100, 1, "", 0, 0)

	if rs.history[1].EventType != EventTypeCallAttemptFailed {
		t.Fatalf("the resumed run recorded %s where its own failed attempt should be",
			rs.history[1].EventType)
	}
	if got := rs.history[1].Attempt; got != 2 {
		t.Fatalf("the resumed policy numbered its first attempt %d; it must continue from the "+
			"spent budget, or a further crash reads the wrong total", got)
	}
	afterSecondCrash := rs.history[:2]

	// Incarnation 3: two spent, exactly one left.
	third := &countingCaller{}
	ts := &execSession{engine: NewEngine(nil, third), isReplay: true, history: afterSecondCrash}
	ts.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
		maxAttempts, 1, 100, 1, "", 0, 0)

	if third.n != 1 {
		t.Errorf("after two crashes of a %d-attempt policy the third incarnation made %d calls, want 1.\n\n"+
			"Total across all three: %d. Each incarnation must read the whole spent budget from "+
			"the history, not just the part it wrote (cleat#1145).",
			maxAttempts, third.n, 2+third.n)
	}
}
