package engine

// W5 of cleat#4152: DurableAwaitSignals through parkOrSuspend. Production has no wake source, so every await suspends (the characterization
// table pins that); these tests give the call a scripted one, because the first real source (WS-cleat-2's) cannot be written against a wait that
// never asks. The shape is the one agreed with WS-cleat-2: poll; if nothing, park; on a wake re-poll through the SAME read and park again with the
// SAME deadline; a passed deadline is the timeout; a refusal suspends. The await record is written ONCE, before the first park.

import (
	"context"
	"testing"
	"time"
)

func awaitParkSession(w *awaitWorld, ws wakeSource, hist []EventRecord, opts ...EngineOption) *execSession {
	base := []EngineOption{WithClock(func() int64 { return awaitT }), WithWorkflowID("wf-await"), WithSignalStore(w)}
	s := &execSession{
		engine: NewEngine(nil, &mockCaller{}, append(base, opts...)...), workflowID: "wf-await", nowMs: awaitT,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: len(hist) > 0, history: hist,
	}
	s.engine.wakeSource = ws
	return s
}

func callAwaitSignals(s *execSession, timeoutMs int64) (nameLen, payloadLen uint32, timedOut bool, errCode uint32, raw int64, buf []byte) {
	buf = make([]byte, 600)
	raw = s.DurableAwaitSignals(contextWithRawMemBuf(context.Background(), buf), nil, awaitNames, timeoutMs, 0, 200, 256, 200)
	nameLen, payloadLen, timedOut, errCode = decodeAwaitSignalsResultGuest(raw)
	return
}

// Woken with nothing, then delivered: ONE await record, then the delivery; the source is asked once per park with the same request; the second
// poll finds the delivery that arrived with the wake.
func TestAnAwaitWokenWithNothingParksAgainAndThenDelivers(t *testing.T) {
	w := &awaitWorld{deliveries: map[string][]SignalDelivery{}}
	ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome {
		if i == 1 { // the second wake brings the delivery
			w.deliveries["a"] = []SignalDelivery{{ID: 7, Payload: "pa"}}
		}
		return parkWoke
	}}
	s := awaitParkSession(w, ws, nil)
	nameLen, payloadLen, timedOut, errCode, raw, buf := callAwaitSignals(s, 60000)
	if raw == callSuspendSentinel || timedOut || errCode != 0 || string(buf[:nameLen]) != "a" || string(buf[256:256+payloadLen]) != "pa" {
		t.Fatalf("returned %#x (name %q payload %q), want the delivery of a", raw, buf[:nameLen], buf[256:256+payloadLen])
	}
	if want := []EventType{EventTypeAwaitSignals, EventTypeSignalReceived}; !awaitSameEventTypes(evTypes(s.history, 0), want) {
		t.Errorf("recorded %v, want %v: the await once, then the delivery (never an await per park)", evTypes(s.history, 0), want)
	}
	if len(ws.reqs) != 2 {
		t.Fatalf("parked %d times, want 2 (woken with nothing, then woken with the delivery)", len(ws.reqs))
	}
	for i, r := range ws.reqs {
		if r.Kind != wakeSignal || r.Key != awaitNames || r.Baseline != 0 || r.DeadlineMs != awaitT+60000 || r.Reason != `await_signals(["a","b"], 60000ms)` {
			t.Errorf("park %d carried %+v, want a signal wait on the asked names, Baseline 0, the deadline now+60s and the fresh reason", i, r)
		}
	}
	// a, b polled before the first park; a, b after the first wake (nothing); a after the second (found)
	if want := []string{"a", "b", "a", "b", "a"}; !awaitSameStrings(w.polls, want) {
		t.Errorf("polled %v, want %v: the same read before the first park and after each wake", w.polls, want)
	}
	if !awaitSameInt64s(w.consumed, []int64{7}) || s.suspendErr != nil || len(s.stepViolations) != 0 || len(s.parkViolations) != 0 {
		t.Errorf("consumed %v, suspension %v, violations step %v park %v; want [7], none, none, none", w.consumed, s.suspendErr, s.stepViolations, s.parkViolations)
	}
}

// A passed deadline while parked is the timeout, and the await is the only thing recorded. CONTROL: the same call woken once with nothing then a
// deadline asked the source twice, with the SAME request both times (a deadline that moved would never expire).
func TestAnAwaitWhoseParkReachesItsDeadlineTimesOutHavingRecordedOnlyTheAwait(t *testing.T) {
	w := &awaitWorld{deliveries: map[string][]SignalDelivery{}}
	ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome {
		if i == 0 {
			return parkWoke
		}
		return parkDeadline
	}}
	s := awaitParkSession(w, ws, nil)
	_, _, timedOut, errCode, raw, _ := callAwaitSignals(s, 60000)
	if !timedOut || errCode != 0 || raw == callSuspendSentinel {
		t.Fatalf("returned %#x, want the timeout", raw)
	}
	if want := []EventType{EventTypeAwaitSignals}; !awaitSameEventTypes(evTypes(s.history, 0), want) || s.suspendErr != nil {
		t.Errorf("recorded %v with suspension %v, want only the await and no suspension", evTypes(s.history, 0), s.suspendErr)
	}
	if len(ws.reqs) != 2 || ws.reqs[0] != ws.reqs[1] {
		t.Errorf("source asked %d times with %+v, want 2 identical requests", len(ws.reqs), ws.reqs)
	}
}

// The replay frontier: the history ended after the await record. The await is NOT recorded again, the deadline is the RECORDED one, and the
// reason says it was replayed. A delivery that arrives with the wake is recorded at the frontier and consumed.
func TestAnAwaitAtTheReplayFrontierParksFromItsRecordedDeadlineAndDoesNotRecordItAgain(t *testing.T) {
	w := &awaitWorld{deliveries: map[string][]SignalDelivery{}}
	ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome {
		w.deliveries["b"] = []SignalDelivery{{ID: 9, Payload: "pb"}}
		return parkWoke
	}}
	hist := []EventRecord{{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: awaitNames, TimeoutMs: 60000, TimestampMs: awaitT - 10_000}}
	s := awaitParkSession(w, ws, hist)
	nameLen, _, timedOut, _, raw, buf := callAwaitSignals(s, 60000)
	if raw == callSuspendSentinel || timedOut || string(buf[:nameLen]) != "b" {
		t.Fatalf("returned %#x (name %q), want the delivery of b", raw, buf[:nameLen])
	}
	if want := []EventType{EventTypeAwaitSignals, EventTypeSignalReceived}; !awaitSameEventTypes(evTypes(s.history, 0), want) {
		t.Errorf("history is %v, want [await, delivery]: the await was recorded in the earlier segment and is not written again", evTypes(s.history, 0))
	}
	if len(ws.reqs) != 1 || ws.reqs[0].DeadlineMs != awaitT+50_000 || ws.reqs[0].Reason != `await_signals(["a","b"], 60000ms) [replayed]` || !ws.reqs[0].Until.Equal(time.UnixMilli(awaitT+50_000)) {
		t.Errorf("source saw %+v, want ONE request for the RECORDED deadline (now+50s) with the replayed reason", ws.reqs)
	}
	if s.isReplay {
		t.Error("the session is still replaying after the frontier's await recorded a delivery: replay must end there")
	}
}

// A no-wait poll never parks, whatever the source would do: it records its await and returns the timeout at once.
func TestANoWaitAwaitNeverAsksTheWakeSource(t *testing.T) {
	w := &awaitWorld{deliveries: map[string][]SignalDelivery{}}
	ws := &scriptedWake{answer: always(parkWoke)}
	s := awaitParkSession(w, ws, nil)
	_, _, timedOut, _, _, _ := callAwaitSignals(s, 0)
	if !timedOut || len(ws.reqs) != 0 {
		t.Errorf("a no-wait poll timed out %v and asked the source %d times, want true and 0", timedOut, len(ws.reqs))
	}
}
