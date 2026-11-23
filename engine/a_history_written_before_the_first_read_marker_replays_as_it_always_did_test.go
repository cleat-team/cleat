package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// cleat#4277, the other half of the upgrade: a history that develop wrote BEFORE the FirstRead marker existed.
//
// The two records below are what the timed-out-then-settled scenario wrote on develop 4ef47c71, printed there by a scratch test in a worktree at that
// commit (not built by this code): the await at step 0 with its 100 ms timeout, then a promise_resolved at step 1 whose timestamp is an hour after the await
// began. It carries no marker, and it is byte for byte the shape ONE await answered on a wake writes. The fix cannot tell the two apart, and the
// owner's ruling is that it does not need to: such a history keeps replaying as it always did, the first await getting the resolution. This test pins
// that as the documented limit, so a later change that starts answering it the other way is a decision somebody made.
//
// It is also where the operator's criterion for finding the affected runs is pinned (see affectedByTheOldAmbiguity). The criterion is a SUPERSET: it
// matches the shape that was ambiguous, not only the runs that were told a timeout.
const prefixHistoryFromDevelop = `[
 {"step":0,"type":"await_promise","timestamp_ms":1795414693590,"timeout_ms":100,"promise_id":"p1"},
 {"step":1,"type":"promise_resolved","timestamp_ms":1795418293590,"promise_id":"p1","promise_result":"v"}
]`

// affectedByTheOldAmbiguity is the criterion the upgrade note gives an operator: an await_promise event followed at the next step by a promise_resolved or
// promise_rejected of the same promise that carries no first_read marker, where the settlement's timestamp is later than the await's timestamp plus its
// recorded timeout. A settlement recorded inside the await's window is an answer to it and is never the other shape; one recorded after the window
// MAY be either (a wake that found the promise settled after its deadline records the same pair), which is why this is a superset.
func affectedByTheOldAmbiguity(await, settled EventRecord) bool {
	return await.EventType == EventTypeAwaitPromise && settled.Step == await.Step+1 &&
		(settled.EventType == EventTypePromiseResolved || settled.EventType == EventTypePromiseRejected) &&
		settled.PromiseID == await.PromiseID && !settled.FirstRead &&
		settled.TimestampMs > await.TimestampMs+await.TimeoutMs
}

func TestAHistoryWrittenBeforeTheFirstReadMarkerReplaysAsItAlwaysDid(t *testing.T) {
	var hist []EventRecord
	if err := json.Unmarshal([]byte(prefixHistoryFromDevelop), &hist); err != nil {
		t.Fatalf("the literal history does not parse: %v", err)
	}
	// Through the payload encoding too, which is how a store hands records back: the old records carry no first_read key and must read back unmarked.
	for i := range hist {
		payload, err := eventRecordToPayload(hist[i])
		if err != nil {
			t.Fatalf("eventRecordToPayload: %v", err)
		}
		var back EventRecord
		populateFromPayload(&back, payload)
		if back.FirstRead {
			t.Fatalf("record %d of an old history reads back MARKED: %s", i, payload)
		}
	}

	w := &promiseWorld{status: "resolved", result: "v"}
	s := &execSession{
		engine:     NewEngine(nil, &mockCaller{}, WithClock(func() int64 { return time.Now().UnixMilli() }), WithWorkflowID("wf-promise"), WithPromiseStore(w)),
		workflowID: "wf-promise", nowMs: promiseT, deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: true, history: append([]EventRecord(nil), hist...),
	}
	buf := make([]byte, 400)
	got := s.AwaitPromise(contextWithRawMemBuf(context.Background(), buf), nil, "p1", 100, 0, 200)
	timedOut := (uint64(got)>>16)&0xFFFF != 0
	if timedOut || string(buf[:uint32(uint64(got)>>32)]) != "v" {
		t.Errorf("an unmarked [await, resolved] pair now answers the first await timedOut=%v result=%q; it has always answered the resolution, and the ruling is that it still does", timedOut, buf[:uint32(uint64(got)>>32)])
	}
	if s.stepCount != 2 {
		t.Errorf("stepCount = %d after the first await, want 2: it consumed both records, as it always did", s.stepCount)
	}
	if w.gets != 0 {
		t.Errorf("the replay read the promise store %d times, want 0: a replay answers from the history", w.gets)
	}
	if len(s.stepViolations) != 0 {
		t.Errorf("the dispatcher noted spec violations: %v", s.stepViolations)
	}

	t.Run("the operator's criterion finds this pair and not an await answered inside its window", func(t *testing.T) {
		if !affectedByTheOldAmbiguity(hist[0], hist[1]) {
			t.Errorf("the criterion does not match the pair develop wrote: await %+v, settlement %+v", hist[0], hist[1])
		}
		answered := hist[1]
		answered.TimestampMs = hist[0].TimestampMs + 50 // inside the 100 ms window: an answer, never the other shape
		if affectedByTheOldAmbiguity(hist[0], answered) {
			t.Errorf("the criterion matches a settlement recorded inside the await's own window")
		}
		marked := hist[1]
		marked.FirstRead = true // written by this version: not ambiguous
		if affectedByTheOldAmbiguity(hist[0], marked) {
			t.Errorf("the criterion matches a marked settlement, which this version replays correctly")
		}
	})
}
