package engine

// W4 of cleat#4152, step 0: a table recorded from AwaitPromise as it is BEFORE it moves onto the step dispatcher, written and run against the
// unmodified code, so that after the move a changed row can only be a deliberate one. Like DurableAwaitSignals it is an accreted wait (cleat#814,
// #3383, #4105, #4144, #4153, #4177, #4178, #4079), and its comments are the record of those bugs.
//
// What makes it different from the signal await, and what the table is for: it has NO stopBeforeNewWork refusal at all (a defer segment, a worker
// shutting down and a session that has already suspended all record and suspend as usual), it matches a recorded resolution by event TYPE alone,
// it looks one record AHEAD after a recorded await and stays in replay, and a first record that is none of its three types is not a divergence:
// the old code falls through to the FRESH path with history still unread. Rows pin each of those as they are, whether or not they are right.
//
// It uses a fake promise store because the question is what the CALL does with a store, not what a database does. It measures the deadline against
// time.Now(), as the code does (and not the engine's clock), so the histories are anchored to the real clock with a margin of an hour either way.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const promiseT = int64(1_800_000_000_000) // the engine's clock for the fresh rows, in ms

// promiseWorld is a promise store that records how the call used it.
type promiseWorld struct {
	status, result, errMsg string
	getErr                 error
	gets                   int
}

func (w *promiseWorld) CreatePromise(context.Context, string, string, string) error { return nil }
func (w *promiseWorld) ResolvePromise(context.Context, string, string) error        { return nil }
func (w *promiseWorld) RejectPromise(context.Context, string, string) error         { return nil }
func (w *promiseWorld) GetPromise(context.Context, string, string) (string, string, string, error) {
	w.gets++
	if w.getErr != nil {
		return "", "", "", w.getErr
	}
	return w.status, w.result, w.errMsg, nil
}

// promiseWant is what a row expects. Zero values mean "nothing": no events, no suspension.
type promiseWant struct {
	sentinel   bool   // the call returned callSuspendSentinel
	timedOut   bool   // the word says timed out
	errCode    uint16 // the word's error code
	result     string // what was written to the guest's buffer
	rejected   bool   // lastAwaitPromiseRejected after the call
	events     []EventType
	recordedAs string // PromiseResult or PromiseError of the first recorded event, when it records an outcome
	awaitTO    int64  // TimeoutMs of a recorded await_promise; ignored when -1... see awaitTOSet
	awaitTOSet bool
	reason     string // "" = no suspension
	untilMs    int64  // exact wake time in ms; 0 = not checked
	untilNear  int64  // wake time must be within [now, now+untilNear] (the call used time.Now()); 0 = not checked
	untilFar   bool   // wake time must be more than a year ahead
	gets       int    // promise-store reads
	step       int
	replaying  bool
}

type promiseRow struct {
	name      string
	opts      []EngineOption
	noStore   bool
	status    string
	result    string
	errMsg    string
	getErr    error
	hist      []EventRecord
	mod       func(s *execSession)
	timeoutMs int64
	resMax    uint32
	want      promiseWant
}

func runPromiseRow(t *testing.T, r promiseRow) {
	w := &promiseWorld{status: r.status, result: r.result, errMsg: r.errMsg, getErr: r.getErr}
	opts := append([]EngineOption{WithClock(func() int64 { return promiseT }), WithWorkflowID("wf-promise")}, r.opts...)
	if !r.noStore {
		opts = append(opts, WithPromiseStore(w))
	}
	s := &execSession{
		engine: NewEngine(nil, &mockCaller{}, opts...), workflowID: "wf-promise", nowMs: promiseT,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: len(r.hist) > 0, history: append([]EventRecord(nil), r.hist...),
	}
	if r.mod != nil {
		r.mod(s)
	}
	before := len(s.history)
	priorSuspend := s.suspendErr
	resMax := r.resMax
	if resMax == 0 {
		resMax = 200
	}
	buf := make([]byte, 400)
	t0 := time.Now().UnixMilli()
	got := s.AwaitPromise(contextWithRawMemBuf(context.Background(), buf), nil, "p1", r.timeoutMs, 0, resMax)
	t1 := time.Now().UnixMilli()

	want := r.want
	if isSentinel := got == callSuspendSentinel; isSentinel != want.sentinel {
		t.Errorf("sentinel = %v, want %v (result %#x)", isSentinel, want.sentinel, got)
	}
	if got != callSuspendSentinel {
		resultLen := uint32(uint64(got) >> 32)
		timedOut := (uint64(got)>>16)&0xFFFF != 0
		errCode := uint16(uint64(got) & 0xFFFF)
		if timedOut != want.timedOut {
			t.Errorf("timedOut = %v, want %v (result %#x)", timedOut, want.timedOut, got)
		}
		if errCode != want.errCode {
			t.Errorf("errCode = %d, want %d (result %#x)", errCode, want.errCode, got)
		}
		if gotRes := string(buf[:resultLen]); gotRes != want.result {
			t.Errorf("result written = %q, want %q", gotRes, want.result)
		}
	}
	if s.lastAwaitPromiseRejected() != want.rejected {
		t.Errorf("rejected flag = %v, want %v", s.lastAwaitPromiseRejected(), want.rejected)
	}
	var ev []EventType
	for _, rec := range s.history[before:] {
		ev = append(ev, rec.EventType)
	}
	if !sameEventTypeList(ev, want.events) {
		t.Errorf("recorded %v, want %v", ev, want.events)
	}
	if len(s.history) > before {
		first := s.history[before]
		if want.recordedAs != "" {
			if gotAs := first.PromiseResult + first.PromiseError; gotAs != want.recordedAs {
				t.Errorf("recorded outcome %q, want %q", gotAs, want.recordedAs)
			}
		}
		if want.awaitTOSet && first.EventType == EventTypeAwaitPromise && first.TimeoutMs != want.awaitTO {
			t.Errorf("recorded await TimeoutMs = %d, want %d", first.TimeoutMs, want.awaitTO)
		}
	}
	switch {
	case want.reason == "" && s.suspendErr != nil && s.suspendErr != priorSuspend:
		t.Errorf("set a suspension %+v, want none", s.suspendErr)
	case want.reason != "":
		if s.suspendErr == nil {
			t.Fatalf("no suspension, want %q", want.reason)
		}
		if s.suspendErr.Reason != want.reason {
			t.Errorf("suspension reason = %q, want %q", s.suspendErr.Reason, want.reason)
		}
		until := s.suspendErr.Until.UnixMilli()
		switch {
		case want.untilMs != 0 && until != want.untilMs:
			t.Errorf("suspension Until = %d, want %d", until, want.untilMs)
		case want.untilNear != 0 && (until < t0 || until > t1+want.untilNear):
			t.Errorf("suspension Until = %d, want within [%d, %d] (time.Now() plus the guest's timeout)", until, t0, t1+want.untilNear)
		case want.untilFar && until < t1+365*24*3600*1000:
			t.Errorf("suspension Until = %d, want more than a year ahead (the recorded timeout is clamped, not wrapped)", until)
		}
	}
	if w.gets != want.gets {
		t.Errorf("promise store read %d times, want %d", w.gets, want.gets)
	}
	if s.stepCount != want.step {
		t.Errorf("stepCount = %d, want %d", s.stepCount, want.step)
	}
	if s.isReplay != want.replaying {
		t.Errorf("isReplay = %v, want %v", s.isReplay, want.replaying)
	}
	if len(s.stepViolations) != 0 {
		t.Errorf("the dispatcher noted spec violations: %v", s.stepViolations)
	}
}

func sameEventTypeList(a, b []EventType) bool {
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

func TestAnAwaitPromiseBehavesAsRecordedBeforeTheDispatcher(t *testing.T) {
	now := time.Now().UnixMilli()
	const hour = int64(3_600_000)
	await := func(anchor, timeoutMs int64) EventRecord {
		return EventRecord{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: timeoutMs, TimestampMs: anchor}
	}
	resolved := func(step int, id, result string) EventRecord {
		return EventRecord{Step: step, EventType: EventTypePromiseResolved, PromiseID: id, PromiseResult: result}
	}
	rejected := func(step int, id, msg string) EventRecord {
		return EventRecord{Step: step, EventType: EventTypePromiseRejected, PromiseID: id, PromiseError: msg}
	}
	longResult := strings.Repeat("r", 300)
	closed := make(chan struct{})
	close(closed)
	reason := "await_promise(p1)"

	rows := []promiseRow{
		// ---- replay: the first record is the outcome (the await answered at once, so no await record was written) ----
		{name: "replay: a recorded resolution replays its result", hist: []EventRecord{resolved(0, "p1", "v")}, timeoutMs: 60000,
			want: promiseWant{result: "v", step: 1, replaying: true}},
		{name: "replay: a recorded rejection replays its error, with errCode 1 and the rejection flag", hist: []EventRecord{rejected(0, "p1", "boom")}, timeoutMs: 60000,
			want: promiseWant{result: "boom", errCode: 1, rejected: true, step: 1, replaying: true}},
		{name: "replay: a recorded resolution of ANOTHER promise is consumed too (matched by type alone)", hist: []EventRecord{resolved(0, "other", "v")}, timeoutMs: 60000,
			want: promiseWant{result: "v", step: 1, replaying: true}},
		{name: "replay: a result that does not fit reports the truncation", hist: []EventRecord{resolved(0, "p1", longResult)}, timeoutMs: 60000, resMax: 10,
			want: promiseWant{result: longResult[:10], errCode: uint16(callErrorOutputTruncated), step: 1, replaying: true}},

		// ---- replay: an await record, and what follows it ----
		{name: "replay: an await then its resolution", hist: []EventRecord{await(now-10_000, hour), resolved(1, "p1", "v")}, timeoutMs: 60000,
			want: promiseWant{result: "v", step: 2, replaying: true}},
		{name: "replay: an await then its rejection", hist: []EventRecord{await(now-10_000, hour), rejected(1, "p1", "boom")}, timeoutMs: 60000,
			want: promiseWant{result: "boom", errCode: 1, rejected: true, step: 2, replaying: true}},
		{name: "replay: an await then a resolution of ANOTHER promise: it timed out (cleat#4177)", hist: []EventRecord{await(now-10_000, hour), resolved(1, "other", "v")}, timeoutMs: 60000,
			want: promiseWant{timedOut: true, step: 1, replaying: true}},
		{name: "replay: an await then a settlement the WORKFLOW made itself (Service promise, Op resolve): it timed out (cleat#4178)",
			hist:      []EventRecord{await(now-10_000, hour), {Step: 1, EventType: EventTypePromiseResolved, PromiseID: "p1", PromiseResult: "v", Service: PromiseEventService, Op: PromiseOpResolve}},
			timeoutMs: 60000, want: promiseWant{timedOut: true, step: 1, replaying: true}},
		{name: "replay: an await then any other event: it timed out", hist: []EventRecord{await(now-10_000, hour), {Step: 1, EventType: fakeEvent}}, timeoutMs: 60000,
			want: promiseWant{timedOut: true, step: 1, replaying: true}},
		{name: "replay: a recorded no-wait await replays as the timeout it was, from the record alone (cleat#4144)", hist: []EventRecord{await(now-10_000, 0)}, timeoutMs: 0,
			want: promiseWant{timedOut: true, step: 1, replaying: true}},
		{name: "replay: ... and the answer is the record's even when a resolution follows it", hist: []EventRecord{await(now-10_000, 0), resolved(1, "p1", "v")}, timeoutMs: 0,
			want: promiseWant{timedOut: true, step: 1, replaying: true}},

		// ---- replay: an await record at the END of history: replay ends and the call decides from the store and the recorded deadline ----
		{name: "replay: an await at the end of history, the promise resolved since: recorded and delivered", status: "resolved", result: "v",
			hist: []EventRecord{await(now-10_000, hour)}, timeoutMs: 60000,
			want: promiseWant{result: "v", events: []EventType{EventTypePromiseResolved}, recordedAs: "v", gets: 1, step: 2}},
		{name: "replay: an await at the end of history, the promise rejected since", status: "rejected", errMsg: "boom",
			hist: []EventRecord{await(now-10_000, hour)}, timeoutMs: 60000,
			want: promiseWant{result: "boom", errCode: 1, rejected: true, events: []EventType{EventTypePromiseRejected}, recordedAs: "boom", gets: 1, step: 2}},
		{name: "replay: an await at the end of history, still pending, deadline ahead: suspends until the RECORDED deadline, recording nothing (cleat#814)",
			status: "pending", hist: []EventRecord{await(now-10_000, hour)}, timeoutMs: 60000,
			want: promiseWant{sentinel: true, reason: reason, untilMs: now - 10_000 + hour, gets: 1, step: 1}},
		{name: "replay: ... and the deadline already passed is a timeout, recording nothing", status: "pending",
			hist: []EventRecord{await(now-2*hour, hour)}, timeoutMs: 60000,
			want: promiseWant{timedOut: true, gets: 1, step: 1}},
		{name: "replay: ... a recorded timeout of 0 under a positive argument keeps waiting from now, with no record", status: "pending",
			hist: []EventRecord{await(now-10_000, 0)}, timeoutMs: 60000,
			want: promiseWant{sentinel: true, reason: reason, untilNear: 60000, gets: 1, step: 1}},
		{name: "replay: ... a recorded timeout of math.MaxInt64 does not wrap into the past (cleat#4105)", status: "pending",
			hist: []EventRecord{await(now-10_000, math.MaxInt64)}, timeoutMs: math.MaxInt64,
			want: promiseWant{sentinel: true, reason: reason, untilFar: true, gets: 1, step: 1}},
		{name: "replay: ... with no promise store configured it reports the error", noStore: true, hist: []EventRecord{await(now-10_000, hour)}, timeoutMs: 60000,
			want: promiseWant{result: errNoPromiseStore, errCode: 1, step: 1}},

		// ---- replay: a first record that is none of the three types. NOT a divergence: the old code falls through to the FRESH path with history
		// still unread and replay still on (the cleat#4157 class), so the call records at a step that already holds an event. ----
		{name: "replay: a first record of another type runs the FRESH path with history unread: a resolved promise is recorded and delivered",
			status: "resolved", result: "v", hist: []EventRecord{{Step: 0, EventType: fakeEvent}}, timeoutMs: 60000,
			want: promiseWant{result: "v", events: []EventType{EventTypePromiseResolved}, recordedAs: "v", gets: 1, step: 1, replaying: true}},
		{name: "replay: a first record of another type, a pending promise: the await is recorded and the run suspends, still replaying",
			status: "pending", hist: []EventRecord{{Step: 0, EventType: fakeEvent}}, timeoutMs: 60000,
			want: promiseWant{sentinel: true, events: []EventType{EventTypeAwaitPromise}, awaitTO: 60000, awaitTOSet: true, reason: reason, untilMs: promiseT + 60000, gets: 1, step: 1, replaying: true}},

		// ---- fresh ----
		{name: "fresh: a resolved promise is recorded and delivered", status: "resolved", result: "v", timeoutMs: 60000,
			want: promiseWant{result: "v", events: []EventType{EventTypePromiseResolved}, recordedAs: "v", gets: 1, step: 1}},
		{name: "fresh: a rejected promise is recorded and its error delivered with the rejection flag", status: "rejected", errMsg: "boom", timeoutMs: 60000,
			want: promiseWant{result: "boom", errCode: 1, rejected: true, events: []EventType{EventTypePromiseRejected}, recordedAs: "boom", gets: 1, step: 1}},
		{name: "fresh: a pending promise records the await WITH its timeout and suspends until now plus the timeout", status: "pending", timeoutMs: 60000,
			want: promiseWant{sentinel: true, events: []EventType{EventTypeAwaitPromise}, awaitTO: 60000, awaitTOSet: true, reason: reason, untilMs: promiseT + 60000, gets: 1, step: 1}},
		{name: "fresh: a store error is read as still pending", getErr: fmt.Errorf("store down"), timeoutMs: 60000,
			want: promiseWant{sentinel: true, events: []EventType{EventTypeAwaitPromise}, awaitTO: 60000, awaitTOSet: true, reason: reason, untilMs: promiseT + 60000, gets: 1, step: 1}},
		{name: "fresh: a no-wait poll of a pending promise records the await (timeout 0) and returns the timeout (cleat#4144)", status: "pending", timeoutMs: 0,
			want: promiseWant{timedOut: true, events: []EventType{EventTypeAwaitPromise}, awaitTO: 0, awaitTOSet: true, gets: 1, step: 1}},
		{name: "fresh: a no-wait poll still reports a promise that is settled", status: "resolved", result: "v", timeoutMs: 0,
			want: promiseWant{result: "v", events: []EventType{EventTypePromiseResolved}, recordedAs: "v", gets: 1, step: 1}},
		{name: "fresh: no promise store configured reports the error and records nothing (IMPROVEMENT-PLAN 3.231)", noStore: true, timeoutMs: 60000,
			want: promiseWant{result: errNoPromiseStore, errCode: 1}},
		{name: "fresh: a result that does not fit reports the truncation", status: "resolved", result: longResult, timeoutMs: 60000, resMax: 10,
			want: promiseWant{result: longResult[:10], errCode: uint16(callErrorOutputTruncated), events: []EventType{EventTypePromiseResolved}, recordedAs: longResult, gets: 1, step: 1}},

		// ---- NOT refused. Unlike await_signals, this call has no stopBeforeNewWork check: all three states below record the await and suspend. A
		// move onto the dispatcher with its default refusal would CHANGE every one of these rows. ----
		{name: "fresh: a defer segment's body is NOT refused: it records the await and suspends", opts: []EngineOption{WithDeferPhase()}, status: "pending", timeoutMs: 60000,
			want: promiseWant{sentinel: true, events: []EventType{EventTypeAwaitPromise}, awaitTO: 60000, awaitTOSet: true, reason: reason, untilMs: promiseT + 60000, gets: 1, step: 1}},
		{name: "fresh: a defer segment's body still gets a settled promise", opts: []EngineOption{WithDeferPhase()}, status: "resolved", result: "v", timeoutMs: 60000,
			want: promiseWant{result: "v", events: []EventType{EventTypePromiseResolved}, recordedAs: "v", gets: 1, step: 1}},
		{name: "fresh: a worker that is shutting down is NOT refused", opts: []EngineOption{WithShutdownSignal(closed)}, status: "pending", timeoutMs: 60000,
			want: promiseWant{sentinel: true, events: []EventType{EventTypeAwaitPromise}, awaitTO: 60000, awaitTOSet: true, reason: reason, untilMs: promiseT + 60000, gets: 1, step: 1}},
		{name: "fresh: a second await in a session that has already suspended is NOT refused: it records a second await and replaces the suspension",
			mod:    func(s *execSession) { s.suspendErr = &SuspendError{Reason: "earlier"} },
			status: "pending", timeoutMs: 60000,
			want: promiseWant{sentinel: true, events: []EventType{EventTypeAwaitPromise}, awaitTO: 60000, awaitTOSet: true, reason: reason, untilMs: promiseT + 60000, gets: 1, step: 1}},
		{name: "fresh: arriving at the frontier of a replay leaves replay and behaves as fresh", status: "pending", hist: []EventRecord{{Step: 0, EventType: fakeEvent}}, timeoutMs: 60000,
			want: promiseWant{sentinel: true, events: []EventType{EventTypeAwaitPromise}, awaitTO: 60000, awaitTOSet: true, reason: reason, untilMs: promiseT + 60000, gets: 1, step: 1, replaying: true}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { runPromiseRow(t, r) })
	}
}
