package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#4177 made a replay at an await_promise's pending record answer from the record that FOLLOWS it. Review (cleat-review, #4178) found
// the first version matched that record by event type and PromiseID alone, and resolve_promise and reject_promise record the same types
// with the same PromiseID: only Service="promise" and Op="resolve"|"reject" (the cleat#4079 discriminator) tell a settlement the workflow
// made from the outcome of the await. A workflow that awaits p, times out, resolves p ITSELF with a default and then makes a call records
// [await_promise, promise_resolved(Op=resolve), call], and a later replay read the settlement as the await's answer: it returned `resolved`
// where the original run was told it timed out, and the call ran again.
//
// The history here is produced by the REAL host functions (AwaitPromise, ResolvePromise) and then replayed, not written by hand, because a
// hand-written history is the mistake #4177 records: the store tests wrote the completing record at the shape the code did not produce.

func awaitResultIsTimeout(res int64) bool { return (res>>16)&1 == 1 }

func promiseSession(store *mockPromiseStore, history []EventRecord) *execSession {
	return &execSession{
		engine:     NewEngine(nil, nil, WithPromiseStore(store)),
		workflowID: "wf-4177",
		nowMs:      time.Now().UnixMilli(),
		isReplay:   len(history) > 0,
		history:    history,
		deferrals:  map[string]string{},
		queryState: map[string]string{},
	}
}

func TestAnAwaitThatTimedOutAndThenResolvedItselfReplaysAsTheTimeout(t *testing.T) {
	ctx := context.Background()
	store := &mockPromiseStore{}

	// Segment 1: the await suspends.
	s1 := promiseSession(store, nil)
	s1.AwaitPromise(ctx, nil, "p1", 60_000, 0, 0)
	if s1.suspendErr == nil || len(s1.history) != 1 || s1.history[0].EventType != EventTypeAwaitPromise {
		t.Fatalf("PRECONDITION FAILED: segment 1 gave suspended=%v history %v", s1.suspendErr != nil, s1.history)
	}

	// Segment 2: resumed past the deadline. The await times out, the workflow resolves p1 itself with a default, then makes a call.
	expired := append([]EventRecord(nil), s1.history...)
	expired[0].TimestampMs -= 61_000
	s2 := promiseSession(store, expired)
	if res := s2.AwaitPromise(ctx, nil, "p1", 60_000, 0, 0); !awaitResultIsTimeout(res) {
		t.Fatalf("PRECONDITION FAILED: the resume past the deadline did not time out (result %#x)", res)
	}
	s2.ResolvePromise(ctx, nil, "p1", `"default"`)
	s2.recordEvent(EventRecord{Step: s2.stepCount, EventType: EventTypeCall, Service: "svc", Op: "after", Response: `{}`})
	h := s2.history
	if len(h) != 3 || h[1].EventType != EventTypePromiseResolved || h[1].Op != PromiseOpResolve || h[2].EventType != EventTypeCall {
		t.Fatalf("PRECONDITION FAILED: segment 2's history is not [await_promise, promise_resolved(Op=resolve), call]: %v", h)
	}

	// Segment 3: a later replay, the promise now settled by the workflow's own resolve. The await must answer what it answered the
	// first time, consume ONLY its own pending record, and leave the settlement and the call to the host calls that recorded them.
	s3 := promiseSession(&mockPromiseStore{status: "resolved", result: `"default"`}, h)
	res := s3.AwaitPromise(ctx, nil, "p1", 60_000, 0, 0)
	if !awaitResultIsTimeout(res) {
		t.Errorf("a later replay answered the await %#x, want the timeout it was told the first time (it read the workflow's own resolve_promise as the await's outcome)", res)
	}
	if s3.stepCount != 1 || !s3.isReplay {
		t.Errorf("after the await: stepCount=%d isReplay=%v, want 1 and true (consumed only its own pending record, still replaying)", s3.stepCount, s3.isReplay)
	}
	s3.ResolvePromise(ctx, nil, "p1", `"default"`)
	if s3.stepCount != 2 || !s3.isReplay {
		t.Errorf("after the workflow's own resolve: stepCount=%d isReplay=%v, want 2 and true", s3.stepCount, s3.isReplay)
	}
	if len(s3.history) != len(h) {
		t.Errorf("a replay recorded %d event(s) (history %d -> %d); a replay records nothing", len(s3.history)-len(h), len(h), len(s3.history))
	}
}

// The consume step is pinned on its own: dropping advanceReplayStep from the look-ahead leaves the step at 1 instead of 2, which no answer
// check notices, and the next host call then replays the await's outcome record as its own.
func TestAnAwaitPromiseReplayConsumesTheOutcomeRecordItAnswersFrom(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		outcome EventRecord
	}{
		{"resolved", EventRecord{EventType: EventTypePromiseResolved, PromiseID: "p1", PromiseResult: `"v"`}},
		{"rejected", EventRecord{EventType: EventTypePromiseRejected, PromiseID: "p1", PromiseError: "no"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := EventRecord{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 60_000, TimestampMs: time.Now().UnixMilli()}
			outcome := tc.outcome
			outcome.Step = 1
			call := EventRecord{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "after", Response: `{}`}
			s := promiseSession(&mockPromiseStore{}, []EventRecord{pending, outcome, call})
			res := s.AwaitPromise(ctx, nil, "p1", 60_000, 0, 0)
			if awaitResultIsTimeout(res) {
				t.Errorf("a recorded %s outcome replayed as a timeout", tc.name)
			}
			if s.stepCount != 2 || !s.isReplay {
				t.Errorf("after the await: stepCount=%d isReplay=%v, want 2 and true (the pending record AND its outcome consumed)", s.stepCount, s.isReplay)
			}
		})
	}

	// An outcome for a DIFFERENT promise is not this await's: the await timed out, and the record is somebody else's.
	t.Run("an outcome for another promise is not this await's", func(t *testing.T) {
		pending := EventRecord{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 60_000, TimestampMs: time.Now().UnixMilli()}
		other := EventRecord{Step: 1, EventType: EventTypePromiseResolved, PromiseID: "p2", PromiseResult: `"v"`}
		s := promiseSession(&mockPromiseStore{}, []EventRecord{pending, other})
		if res := s.AwaitPromise(ctx, nil, "p1", 60_000, 0, 0); !awaitResultIsTimeout(res) {
			t.Errorf("an outcome for promise p2 was read as p1's answer (result %#x)", res)
		}
		if s.stepCount != 1 {
			t.Errorf("stepCount=%d, want 1: only the pending record is the await's", s.stepCount)
		}
	})

	// An outcome with NO PromiseID is not this await's either. The await always records the id it was given, and nothing writes an empty
	// one, so the first version's "empty id matches" fallback could only ever match something that is not the await's outcome.
	t.Run("an outcome with no promise id is not this await's", func(t *testing.T) {
		pending := EventRecord{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 60_000, TimestampMs: time.Now().UnixMilli()}
		anon := EventRecord{Step: 1, EventType: EventTypePromiseResolved, PromiseResult: `"v"`}
		s := promiseSession(&mockPromiseStore{}, []EventRecord{pending, anon})
		if res := s.AwaitPromise(ctx, nil, "p1", 60_000, 0, 0); !awaitResultIsTimeout(res) {
			t.Errorf("an outcome with an empty PromiseID was read as p1's answer (result %#x)", res)
		}
	})
}
