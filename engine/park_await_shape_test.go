package engine

// The shape every wait will have once it parks (cleat#4152, W2b; agreed with WS-cleat-2, who builds the real wake source): a poll, then a loop of
// park and re-poll, with the await record written ONCE before the first park. A wake carries no payload and can be spurious (a wake for another
// wait, a batched check that saw another run's sequence move), so the caller re-polls the live store and, finding nothing, parks again with the
// SAME deadline and an UPDATED baseline. Over a fake multi-record spec and the real dispatcher: no real wait is on it yet. Its first real
// consumers are the signal, promise and child waits, each in its own PR.

import (
	"context"
	"testing"
)

// fakeInbox is the live store a fake await polls: seq moves when anything happens, and the awaited thing is there once seq reaches deliverAt.
type fakeInbox struct{ seq, deliverAt int64 }

func (i *fakeInbox) poll() (found bool, seq int64) { return i.seq >= i.deliverAt, i.seq }

const (
	awaitDelivered = 9 // what the fake returns when the awaited thing arrived
	awaitSuspended = 5 // what it returns when the park was refused and the run suspends
	awaitDecoded   = 7 // what a replay of its history decodes to
)

// awaitSpec is the shape of a wait whose completion is a separate event from its await record: [await][outcome], or just [outcome] when the
// first poll already found the answer. Its Role is the multi-record run W2a reads: the await record is the prefix, the outcome the final.
func awaitSpec(inbox *fakeInbox) *stepSpec {
	return &stepSpec{
		Name: "fake_await", Result: layoutSimple, Effect: effectEffect, Records: recordsMany, Want: runFinalEvent,
		NoIdentity: "a fake await with nothing to compare",
		Role: func(_ stepArgs, rec *EventRecord) recordRole {
			switch rec.EventType {
			case runPrefixEvent:
				return rolePrefix
			case runFinalEvent:
				return roleFinal
			}
			return roleForeign
		},
		DecodeRun: func(*stepCtx, []EventRecord) int64 { return awaitDecoded },
		Fresh: func(c *stepCtx) int64 {
			found, seq := inbox.poll()
			if found {
				c.Record(EventRecord{Step: c.s.stepCount, EventType: runFinalEvent})
				return awaitDelivered
			}
			// The await record is written once, before the FIRST park, and never per iteration. If the history ended inside this run the record
			// is already there (c.Prefix), and writing it again would make a second await for one wait.
			if len(c.Prefix) == 0 {
				c.Record(EventRecord{Step: c.s.stepCount, EventType: runPrefixEvent})
			}
			for {
				req := parkRequest{Kind: wakeSignal, Key: "sig", Baseline: seq, Reason: "fake_await", DeadlineMs: 0}
				if c.s.parkOrSuspend(c.ctx, req) == parkRefused {
					return awaitSuspended
				}
				if found, seq = inbox.poll(); found {
					c.Record(EventRecord{Step: c.s.stepCount, EventType: runFinalEvent})
					return awaitDelivered
				}
				// A wake with nothing there: park again, same deadline, updated baseline.
			}
		},
	}
}

func typesOf(h []EventRecord) []EventType {
	out := make([]EventType, 0, len(h))
	for _, r := range h {
		out = append(out, r.EventType)
	}
	return out
}

func sameTypes(got, want []EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Woken twice with nothing, then delivered: ONE await record, the outcome as the second event, three parks, and each park's baseline is what the
// poll before it read, which moved. A baseline that did not update would hand the wake source a stale value to compare against.
func TestAnAwaitWokenWithNothingParksAgainWithoutRecordingTheAwaitAgain(t *testing.T) {
	inbox := &fakeInbox{deliverAt: 3}
	// Each wake is the store moving without delivering: seq goes 0 -> 1 -> 2 -> 3, and delivery is at 3.
	ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome { inbox.seq = int64(i) + 1; return parkWoke }}
	s := parkSession(ws)

	if got := s.step(context.Background(), nil, awaitSpec(inbox), stepArgs{}); got != awaitDelivered {
		t.Fatalf("the await returned %d, want %d (delivered)", got, awaitDelivered)
	}
	if want := []EventType{runPrefixEvent, runFinalEvent}; !sameTypes(typesOf(s.history), want) {
		t.Errorf("recorded %v, want %v: exactly one await record, then the outcome", typesOf(s.history), want)
	}
	if len(ws.reqs) != 3 {
		t.Fatalf("parked %d times, want 3 (woken twice with nothing, then delivered on the third wake)", len(ws.reqs))
	}
	for i, wantBase := range []int64{0, 1, 2} {
		if ws.reqs[i].Baseline != wantBase {
			t.Errorf("park %d carried baseline %d, want %d (what the poll before it read)", i, ws.reqs[i].Baseline, wantBase)
		}
		if ws.reqs[i].Kind != wakeSignal || ws.reqs[i].DeadlineMs != 0 {
			t.Errorf("park %d changed the request's kind or deadline: %+v", i, ws.reqs[i])
		}
	}
	if len(s.stepViolations) != 0 || len(s.parkViolations) != 0 {
		t.Errorf("violations: step %v, park %v", s.stepViolations, s.parkViolations)
	}
}

// The first poll already finds the answer: only the outcome is recorded, and the wake source is never asked.
func TestAnAwaitWhoseFirstPollFindsTheAnswerRecordsOnlyTheOutcome(t *testing.T) {
	inbox := &fakeInbox{deliverAt: 0}
	ws := &scriptedWake{answer: always(parkWoke)}
	s := parkSession(ws)
	if got := s.step(context.Background(), nil, awaitSpec(inbox), stepArgs{}); got != awaitDelivered {
		t.Fatalf("returned %d, want %d", got, awaitDelivered)
	}
	if want := []EventType{runFinalEvent}; !sameTypes(typesOf(s.history), want) || len(ws.reqs) != 0 {
		t.Errorf("recorded %v and parked %d times, want just the outcome and no park", typesOf(s.history), len(ws.reqs))
	}
}

// The run was suspended or lost while blocked, so its history ends after the await record. The replay consumes that record, ends replay, and Fresh
// continues from it: the await is NOT written a second time.
func TestAnAwaitWhoseHistoryEndsAfterTheAwaitRecordContinuesWithoutRecordingItAgain(t *testing.T) {
	inbox := &fakeInbox{deliverAt: 1}
	ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome { inbox.seq = 1; return parkWoke }}
	s := stepSession(nil, []EventRecord{{Step: 0, EventType: runPrefixEvent}})
	s.engine.wakeSource = ws

	if got := s.step(context.Background(), nil, awaitSpec(inbox), stepArgs{}); got != awaitDelivered {
		t.Fatalf("returned %d, want %d", got, awaitDelivered)
	}
	if want := []EventType{runPrefixEvent, runFinalEvent}; !sameTypes(typesOf(s.history), want) {
		t.Errorf("history is %v, want [await, outcome]: the await was recorded once, in the earlier segment", typesOf(s.history))
	}
	if len(ws.reqs) != 1 {
		t.Errorf("parked %d times, want 1", len(ws.reqs))
	}
}

// A refused park suspends with only the await record written: the outcome is not recorded for a wait that did not finish.
func TestAnAwaitThatIsRefusedAParkSuspendsHavingRecordedOnlyTheAwait(t *testing.T) {
	inbox := &fakeInbox{deliverAt: 5}
	s := parkSession(nil) // no wake source: every wait suspends
	if got := s.step(context.Background(), nil, awaitSpec(inbox), stepArgs{}); got != awaitSuspended {
		t.Fatalf("returned %d, want %d (suspended)", got, awaitSuspended)
	}
	if want := []EventType{runPrefixEvent}; !sameTypes(typesOf(s.history), want) || s.suspendErr == nil {
		t.Errorf("recorded %v with suspension %v, want just the await and a suspension", typesOf(s.history), s.suspendErr)
	}
}

// Replaying the finished history [await, outcome] decodes it and parks nothing: a replay never waits.
func TestReplayingAFinishedAwaitParksNothing(t *testing.T) {
	inbox := &fakeInbox{deliverAt: 99}
	ws := &scriptedWake{answer: always(parkWoke)}
	s := stepSession(nil, []EventRecord{{Step: 0, EventType: runPrefixEvent}, {Step: 1, EventType: runFinalEvent}})
	s.engine.wakeSource = ws
	if got := s.step(context.Background(), nil, awaitSpec(inbox), stepArgs{}); got != awaitDecoded {
		t.Fatalf("replay returned %d, want %d (decoded)", got, awaitDecoded)
	}
	if len(ws.reqs) != 0 || s.stepCount != 2 {
		t.Errorf("replay parked %d times and stopped at step %d, want 0 and 2", len(ws.reqs), s.stepCount)
	}
}
