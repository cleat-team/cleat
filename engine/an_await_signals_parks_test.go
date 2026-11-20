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
		if i > 5 { // a loop that never re-polls would ask forever: refuse after a few so it FAILS (suspends) instead of hanging the package
			return parkRefused
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

// A park's outcome is ADVISORY and the clock decides, as it does for a durable sleep (cleat#4237's FINAL NOT OK): a wake and a deadline both carry
// no payload, a delivery can land with either, and a source can be wrong about the time. So after EVERY return from the source the call polls the
// store once; it is a timeout only if the engine's real clock agrees the deadline has passed; and a deadline the clock does not agree with
// suspends, so the scheduler's own clock decides when the run wakes. Without the clock check an empty wake loop also had no exit but the source.
//
// The scripted source here moves the clock and lands deliveries from inside Wait, which is what a real one does between "registered" and "woke".
func TestAnAwaitParkOutcomeIsAdvisoryAndTheClockDecidesTheTimeout(t *testing.T) {
	const dl = awaitT + 60000
	type row struct {
		name     string
		answers  []parkOutcome // what the source returns on ask 0, 1, ...; a source asked more times than this REFUSES (so a loop that would hang fails loud)
		clockAt  []int64       // the clock after ask i returns; the last value holds
		deliverA int           // deliver "a" during ask i (-1: never)
		want     string        // "timeout", "delivered", "suspended"
		asks     int
		polls    []string
	}
	rows := []row{
		{name: "a deadline outcome with the clock PAST the deadline and nothing delivered is the timeout, after one more poll",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl + 1}, deliverA: -1, want: "timeout", asks: 1, polls: []string{"a", "b", "a", "b"}},
		{name: "a deadline outcome with the clock exactly AT the deadline is the timeout",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl}, deliverA: -1, want: "timeout", asks: 1, polls: []string{"a", "b", "a", "b"}},
		{name: "a deadline outcome 60s EARLY is not a timeout: it suspends and the scheduler's clock decides (the source can be wrong about the time)",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl - 60000}, deliverA: -1, want: "suspended", asks: 1, polls: []string{"a", "b", "a", "b"}},
		{name: "a delivery that lands WITH the deadline is delivered, not timed out (clock past the deadline)",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl + 1}, deliverA: 0, want: "delivered", asks: 1, polls: []string{"a", "b", "a"}},
		{name: "a delivery that lands with an early deadline outcome is delivered too",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl - 60000}, deliverA: 0, want: "delivered", asks: 1, polls: []string{"a", "b", "a"}},
		{name: "a source that only ever says woken cannot hold the call past the deadline: it times out once the clock does",
			answers: []parkOutcome{parkWoke, parkWoke, parkWoke, parkWoke, parkWoke, parkWoke}, clockAt: []int64{awaitT + 25_000, awaitT + 50_000, dl + 15_000}, deliverA: -1, want: "timeout", asks: 3,
			polls: []string{"a", "b", "a", "b", "a", "b", "a", "b"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			clock := awaitT
			w := &awaitWorld{deliveries: map[string][]SignalDelivery{}}
			ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome {
				if i >= len(r.answers) {
					return parkRefused
				}
				if i < len(r.clockAt) {
					clock = r.clockAt[i]
				} else if len(r.clockAt) > 0 {
					clock = r.clockAt[len(r.clockAt)-1]
				}
				if i == r.deliverA {
					w.deliveries["a"] = []SignalDelivery{{ID: 7, Payload: "pa"}}
				}
				return r.answers[i]
			}}
			s := awaitParkSession(w, ws, nil, WithClock(func() int64 { return clock }))
			nameLen, _, timedOut, errCode, raw, buf := callAwaitSignals(s, 60000)
			got := "delivered"
			switch {
			case raw == callSuspendSentinel:
				got = "suspended"
			case timedOut:
				got = "timeout"
			}
			if got != r.want || errCode != 0 {
				t.Errorf("returned %#x (%s, name %q), want %s", raw, got, buf[:nameLen], r.want)
			}
			if len(ws.reqs) != r.asks {
				t.Errorf("source asked %d times, want %d", len(ws.reqs), r.asks)
			}
			if !awaitSameStrings(w.polls, r.polls) {
				t.Errorf("polled %v, want %v: one poll after EVERY return from the source, and the first poll before the first park", w.polls, r.polls)
			}
			wantEvents := []EventType{EventTypeAwaitSignals}
			if r.want == "delivered" {
				wantEvents = append(wantEvents, EventTypeSignalReceived)
			}
			if !awaitSameEventTypes(evTypes(s.history, 0), wantEvents) {
				t.Errorf("recorded %v, want %v (the await once, never one per park)", evTypes(s.history, 0), wantEvents)
			}
			if r.want == "suspended" {
				if s.suspendErr == nil || !s.suspendErr.Until.Equal(time.UnixMilli(dl)) {
					t.Errorf("suspension %+v, want one that wakes at the deadline %d", s.suspendErr, dl)
				}
			} else if s.suspendErr != nil {
				t.Errorf("set a suspension %+v, want none", s.suspendErr)
			}
		})
	}
}

// The replay frontier: the history ended after the await record. The await is NOT recorded again, the deadline is the RECORDED one, and the
// reason says it was replayed. A delivery that arrives with the wake is recorded at the frontier and consumed.
func TestAnAwaitAtTheReplayFrontierParksFromItsRecordedDeadlineAndDoesNotRecordItAgain(t *testing.T) {
	w := &awaitWorld{deliveries: map[string][]SignalDelivery{}}
	ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome {
		w.deliveries["b"] = []SignalDelivery{{ID: 9, Payload: "pb"}}
		if i > 5 { // bounded, so a call that never re-polls FAILS instead of hanging the package
			return parkRefused
		}
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
