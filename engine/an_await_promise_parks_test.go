package engine

// W4 of cleat#4152: AwaitPromise through parkOrSuspend. Production has no wake source, so every await suspends (the characterization table pins that);
// these tests give the call a scripted one, because the first real source (WS-cleat-2's) cannot be written against a wait that never asks. It is the
// shape agreed for await_signals (an_await_signals_parks_test.go) and the same rules: poll; if still pending, park; after EVERY return from the source
// poll once more through the SAME read; the engine's real clock decides the timeout and a deadline outcome it does not agree with suspends; an invented
// deadline is never handed to a source. The await record is written ONCE, before the first park.

import (
	"context"
	"testing"
	"time"
)

func promiseParkSession(w *promiseWorld, ws wakeSource, hist []EventRecord, clock *int64, opts ...EngineOption) *execSession {
	base := []EngineOption{WithClock(func() int64 { return *clock }), WithWorkflowID("wf-promise"), WithPromiseStore(w)}
	s := &execSession{
		engine: NewEngine(nil, &mockCaller{}, append(base, opts...)...), workflowID: "wf-promise", nowMs: promiseT,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: len(hist) > 0, history: hist,
	}
	s.engine.wakeSource = ws
	return s
}

// callAwaitPromise awaits "p1" and decodes the word: the result length, whether it says timed out, the error code and what was written.
func callAwaitPromise(s *execSession, timeoutMs int64) (raw int64, timedOut bool, errCode uint16, written string) {
	buf := make([]byte, 400)
	raw = s.AwaitPromise(contextWithRawMemBuf(context.Background(), buf), nil, "p1", timeoutMs, 0, 200)
	if raw == callSuspendSentinel {
		return raw, false, 0, ""
	}
	return raw, (uint64(raw)>>16)&0xFFFF != 0, uint16(uint64(raw) & 0xFFFF), string(buf[:uint32(uint64(raw)>>32)])
}

func promiseEventTypes(s *execSession, from int) []EventType {
	var out []EventType
	for _, r := range s.history[from:] {
		out = append(out, r.EventType)
	}
	return out
}

// Woken with the promise still pending, then settled: ONE await record, then the outcome; the source is asked once per park with the same request;
// the second poll finds the outcome that arrived with the wake.
func TestAnAwaitPromiseWokenWithNothingParksAgainAndThenDelivers(t *testing.T) {
	clock := promiseT
	w := &promiseWorld{status: "pending"}
	ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome {
		if i == 1 { // the second wake brings the resolution
			w.status, w.result = "resolved", "v"
		}
		if i > 5 { // a loop that never re-polls would ask forever: refuse after a few so it FAILS (suspends) instead of hanging the package
			return parkRefused
		}
		return parkWoke
	}}
	s := promiseParkSession(w, ws, nil, &clock)
	raw, timedOut, errCode, written := callAwaitPromise(s, 60000)
	if raw == callSuspendSentinel || timedOut || errCode != 0 || written != "v" {
		t.Fatalf("returned %#x (timedOut %v, errCode %d, result %q), want the resolution v", raw, timedOut, errCode, written)
	}
	if want := []EventType{EventTypeAwaitPromise, EventTypePromiseResolved}; !sameEventTypeList(promiseEventTypes(s, 0), want) {
		t.Errorf("recorded %v, want %v: the await once, then the resolution (never an await per park)", promiseEventTypes(s, 0), want)
	}
	if len(ws.reqs) != 2 {
		t.Fatalf("parked %d times, want 2 (woken with nothing, then woken with the resolution)", len(ws.reqs))
	}
	for i, r := range ws.reqs {
		if r.Kind != wakePromise || r.Key != "p1" || r.Baseline != 0 || r.DeadlineMs != promiseT+60000 || r.Reason != "await_promise(p1)" {
			t.Errorf("park %d carried %+v, want a promise wait on p1, Baseline 0, the deadline now+60s and the await's reason", i, r)
		}
	}
	// one read before the first park, one after each wake
	if w.gets != 3 || s.suspendErr != nil || len(s.stepViolations) != 0 || len(s.parkViolations) != 0 {
		t.Errorf("store read %d times, suspension %v, violations step %v park %v; want 3, none, none, none", w.gets, s.suspendErr, s.stepViolations, s.parkViolations)
	}
}

// A park's outcome is ADVISORY and the clock decides, exactly as for await_signals and a durable sleep (cleat#4237's FINAL NOT OK, #4249): a wake and a
// deadline both carry no payload, a settlement can land with either, and a source can be wrong about the time. So after EVERY return from the source the
// call reads the promise once; it is a timeout only if the engine's real clock agrees the deadline has passed; and a deadline the clock does not agree
// with suspends. The scripted source moves the clock and settles the promise from inside Wait, which is what a real one does between registered and woke.
func TestAnAwaitPromiseParkOutcomeIsAdvisoryAndTheClockDecidesTheTimeout(t *testing.T) {
	const dl = promiseT + 60000
	type row struct {
		name      string
		answers   []parkOutcome // what the source returns on ask 0, 1, ...; asked more times than this it REFUSES, so a loop that would hang fails loud
		clockAt   []int64       // the clock after ask i returns; the last value holds
		resolveAt int           // settle the promise during ask i (-1: never)
		want      string        // "timeout", "delivered", "suspended"
		asks      int
		gets      int
	}
	rows := []row{
		{name: "a deadline outcome with the clock PAST the deadline and nothing settled is the timeout, after one more read",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl + 1}, resolveAt: -1, want: "timeout", asks: 1, gets: 2},
		{name: "a deadline outcome with the clock exactly AT the deadline is the timeout",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl}, resolveAt: -1, want: "timeout", asks: 1, gets: 2},
		{name: "a deadline outcome 60s EARLY is not a timeout: it suspends and the scheduler's clock decides (the source can be wrong about the time)",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl - 60000}, resolveAt: -1, want: "suspended", asks: 1, gets: 2},
		{name: "a settlement that lands WITH the deadline is delivered, not timed out (clock past the deadline)",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl + 1}, resolveAt: 0, want: "delivered", asks: 1, gets: 2},
		{name: "a settlement that lands with an early deadline outcome is delivered too",
			answers: []parkOutcome{parkDeadline}, clockAt: []int64{dl - 60000}, resolveAt: 0, want: "delivered", asks: 1, gets: 2},
		{name: "a source that only ever says woken cannot hold the call past the deadline: it times out once the clock does",
			answers: []parkOutcome{parkWoke, parkWoke, parkWoke, parkWoke, parkWoke, parkWoke}, clockAt: []int64{promiseT + 25_000, promiseT + 50_000, dl + 15_000}, resolveAt: -1, want: "timeout", asks: 3, gets: 4},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			clock := promiseT
			w := &promiseWorld{status: "pending"}
			ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome {
				if i >= len(r.answers) {
					return parkRefused
				}
				if i < len(r.clockAt) {
					clock = r.clockAt[i]
				} else if len(r.clockAt) > 0 {
					clock = r.clockAt[len(r.clockAt)-1]
				}
				if i == r.resolveAt {
					w.status, w.result = "resolved", "v"
				}
				return r.answers[i]
			}}
			s := promiseParkSession(w, ws, nil, &clock)
			raw, timedOut, _, written := callAwaitPromise(s, 60000)
			got := "delivered"
			switch {
			case raw == callSuspendSentinel:
				got = "suspended"
			case timedOut:
				got = "timeout"
			}
			if got != r.want || (got == "delivered" && written != "v") {
				t.Errorf("the call ended %q (result %q), want %q", got, written, r.want)
			}
			if len(ws.reqs) != r.asks || w.gets != r.gets {
				t.Errorf("asked the source %d times and read the store %d times, want %d and %d", len(ws.reqs), w.gets, r.asks, r.gets)
			}
			if (got == "suspended") != (s.suspendErr != nil) {
				t.Errorf("suspension %v with outcome %q: only a suspended call sets one", s.suspendErr, got)
			}
			if len(s.stepViolations) != 0 || len(s.parkViolations) != 0 {
				t.Errorf("violations step %v park %v", s.stepViolations, s.parkViolations)
			}
		})
	}
}

// A replayed await that has just ended its history parks against its RECORDED deadline (cleat#814), through the same read, and records no second
// await: the await is already in history. The wake that brings the resolution records the OUTCOME only.
func TestAReplayedAwaitPromiseParksAgainstItsRecordedDeadlineAndRecordsOnlyTheOutcome(t *testing.T) {
	clock := promiseT
	w := &promiseWorld{status: "pending"}
	ws := &scriptedWake{answer: func(i int, _ parkRequest) parkOutcome {
		if i == 0 {
			w.status, w.result = "resolved", "late"
		}
		if i > 3 {
			return parkRefused
		}
		return parkWoke
	}}
	hist := []EventRecord{{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 60000, TimestampMs: promiseT - 10_000}}
	s := promiseParkSession(w, ws, hist, &clock)
	raw, timedOut, errCode, written := callAwaitPromise(s, 60000)
	if raw == callSuspendSentinel || timedOut || errCode != 0 || written != "late" {
		t.Fatalf("returned %#x (timedOut %v, errCode %d, result %q), want the resolution", raw, timedOut, errCode, written)
	}
	if want := []EventType{EventTypePromiseResolved}; !sameEventTypeList(promiseEventTypes(s, 1), want) {
		t.Errorf("recorded %v after the await, want only %v", promiseEventTypes(s, 1), want)
	}
	if len(ws.reqs) != 1 || ws.reqs[0].DeadlineMs != promiseT+50_000 || ws.reqs[0].Kind != wakePromise || ws.reqs[0].Key != "p1" {
		t.Errorf("parked %+v, want ONE promise wait on p1 against the RECORDED deadline %d (anchor 10s ago plus 60s)", ws.reqs, promiseT+50_000)
	}
	if s.isReplay || len(s.stepViolations) != 0 {
		t.Errorf("replaying %v, violations %v; want replay ended and none", s.isReplay, s.stepViolations)
	}
}

// THE INVENTED DEADLINE, the defect cleat-review3 found in #4249 round 2, written first for this call. A replayed await whose record has no timeout
// (written before the timeout was persisted) has no deadline to measure against, and the call has always kept waiting from now for the guest's own
// timeout, recording nothing. That deadline is INVENTED. Handed to a wake source it would time out on the source's first return, where the call has
// always suspended; so it suspends directly, and a source is never asked. The with-source row is the one that would catch the defect; the without-source
// row is the production behaviour, which must be the same.
func TestAReplayedAwaitPromiseWithAnInventedDeadlineSuspendsAndNeverAsksTheSource(t *testing.T) {
	for _, withSource := range []bool{true, false} {
		name := "no wake source"
		if withSource {
			name = "a wake source that must not be asked"
		}
		t.Run(name, func(t *testing.T) {
			clock := promiseT
			w := &promiseWorld{status: "pending"}
			var ws *scriptedWake
			var src wakeSource
			if withSource {
				ws = &scriptedWake{answer: always(parkDeadline)} // would read as a timeout if the call trusted it
				src = ws
			}
			hist := []EventRecord{{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 0, TimestampMs: promiseT - 10_000}}
			s := promiseParkSession(w, src, hist, &clock)
			t0 := time.Now().UnixMilli()
			raw, _, _, _ := callAwaitPromise(s, 60000)
			t1 := time.Now().UnixMilli()
			if raw != callSuspendSentinel {
				t.Fatalf("returned %#x, want the suspend sentinel: the call keeps waiting, it does not time out", raw)
			}
			if withSource && len(ws.reqs) != 0 {
				t.Errorf("the source was asked %d time(s) about an invented deadline", len(ws.reqs))
			}
			if s.suspendErr == nil || s.suspendErr.Reason != "await_promise(p1)" {
				t.Fatalf("suspension %+v, want reason await_promise(p1)", s.suspendErr)
			}
			if until := s.suspendErr.Until.UnixMilli(); until < t0+60000 || until > t1+60000 {
				t.Errorf("suspension Until = %d, want within [%d, %d] (time.Now() plus the guest's 60s)", until, t0+60000, t1+60000)
			}
			if len(s.history) != 1 || w.gets != 1 || len(s.stepViolations) != 0 || len(s.parkViolations) != 0 {
				t.Errorf("history %d events, store read %d times, violations step %v park %v; want 1, 1, none, none", len(s.history), w.gets, s.stepViolations, s.parkViolations)
			}
		})
	}
}

// A promise that is already settled is answered at once and a source is never asked; a no-wait poll never waits, so it never asks either.
func TestAnAwaitPromiseThatNeedsNoWaitNeverAsksTheSource(t *testing.T) {
	for name, c := range map[string]struct {
		status    string
		timeoutMs int64
	}{"already resolved": {"resolved", 60000}, "a no-wait poll of a pending promise": {"pending", 0}} {
		t.Run(name, func(t *testing.T) {
			clock := promiseT
			ws := &scriptedWake{answer: always(parkWoke)}
			s := promiseParkSession(&promiseWorld{status: c.status, result: "v"}, ws, nil, &clock)
			callAwaitPromise(s, c.timeoutMs)
			if len(ws.reqs) != 0 {
				t.Errorf("the source was asked %d time(s)", len(ws.reqs))
			}
		})
	}
}
