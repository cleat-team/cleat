package engine

// W5 of cleat#4152, step 0: a table recorded from DurableAwaitSignals as it is BEFORE it moves onto the step dispatcher, written and run against the
// unmodified code, so that after the move a changed row can only be a deliberate one. It is the most accreted wait in the engine: nearly every row
// below has a bug number behind it (cleat#933, #947, #975, #1331, #3378, #4105, #4144, #4153, #4157), and the function's own comments are the history.
//
// Each row is one situation and records everything the call does that anything can observe: the result word, what it wrote to the guest's memory, the
// events it recorded, which names it polled and which deliveries it consumed, the suspension it set, and where it left the session (step, replaying).
//
// It uses a fake signal store because the question is what the CALL does with a store, not what a database does (the dialects' own tests cover that).

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const awaitT = int64(1_800_000_000_000) // the engine's clock for every row, in ms

// awaitWorld is a signal store that records how the call used it.
type awaitWorld struct {
	deliveries map[string][]SignalDelivery
	pollErr    error
	polls      []string
	consumed   []int64
}

func (w *awaitWorld) DeliverSignal(context.Context, string, string, string) error { return nil }
func (w *awaitWorld) PollSignal(_ context.Context, _ string, name string) (SignalDelivery, bool, error) {
	w.polls = append(w.polls, name)
	if w.pollErr != nil {
		// An error that arrives WITH a delivery: the call reads err != nil as "nothing found" and must not hand it out. A fake that returned
		// found=false on error could not tell that rule from its absence (cleat-review3's condition on the first version of this table).
		if ds := w.deliveries[name]; len(ds) > 0 {
			return ds[0], true, w.pollErr
		}
		return SignalDelivery{}, false, w.pollErr
	}
	if ds := w.deliveries[name]; len(ds) > 0 {
		return ds[0], true, nil
	}
	return SignalDelivery{}, false, nil
}
func (w *awaitWorld) ConsumeSignal(_ context.Context, _ string, id int64) error {
	w.consumed = append(w.consumed, id)
	return nil
}
func (w *awaitWorld) PollCancellation(context.Context, string) (bool, string, error) {
	return false, "", nil
}

// awaitWant is what a row expects. Zero values mean "nothing": no events, no polls, no suspension.
type awaitWant struct {
	sentinel   bool // the call returned callSuspendSentinel
	timedOut   bool
	diverged   bool // an errCode of 1 and a message that says "replay divergence"
	errNonZero bool // some other non-zero errCode (a truncated write)
	name, payl string
	events     []EventType
	polls      []string
	consumed   []int64
	reason     string // "" = no suspension
	until      int64  // the suspension's wake time in ms; -1 = anywhere after the clock
	step       int
	replaying  bool
}

func evTypes(h []EventRecord, from int) []EventType {
	out := []EventType{}
	for _, r := range h[from:] {
		out = append(out, r.EventType)
	}
	return out
}

func TestAnAwaitSignalsBehavesAsRecordedBeforeTheDispatcher(t *testing.T) {
	const names = `["a","b"]`
	longPayload := strings.Repeat("x", 300)
	longName := strings.Repeat("n", 40)
	shortName := longName[:10] // what a 10-byte name buffer receives, with the truncation reported alongside it
	longNames := `["` + longName + `"]`
	await := func(ts, timeoutMs int64) EventRecord {
		return EventRecord{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: names, TimeoutMs: timeoutMs, TimestampMs: ts}
	}
	delivered := func(step int, name, payload string) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeSignalReceived, SignalName: name, SignalPayload: payload}
	}
	closed := make(chan struct{})
	close(closed)
	reasonFresh := `await_signals(["a","b"], 60000ms)`
	reasonReplayed := func(timeoutMs int64) string {
		return fmt.Sprintf(`await_signals(["a","b"], %dms) [replayed]`, timeoutMs)
	}

	rows := []struct {
		name      string
		opts      []EngineOption
		noStore   bool
		deliver   map[string][]SignalDelivery
		pollErr   error
		hist      []EventRecord
		stepCount int
		mod       func(s *execSession)
		timeoutMs int64
		payMax    uint32
		nameMax   uint32
		sigNames  string // the names the guest asks about; default ["a","b"]
		want      awaitWant
	}{
		// ---- replay ----
		{name: "replay: a recorded delivery with no await before it (a first-poll hit)", hist: []EventRecord{delivered(0, "a", "pa")}, timeoutMs: 60000,
			want: awaitWant{name: "a", payl: "pa", step: 1, replaying: true}},
		{name: "replay: an await and then its delivery", hist: []EventRecord{await(awaitT-1000, 60000), delivered(1, "b", "pb")}, timeoutMs: 60000,
			want: awaitWant{name: "b", payl: "pb", step: 2, replaying: true}},
		{name: "replay: a recorded no-wait await replays as the timeout it was (cleat#4144)", hist: []EventRecord{await(awaitT-1000, 0)}, timeoutMs: 0,
			want: awaitWant{timedOut: true, step: 1, replaying: true}},
		{name: "replay: an await that history continues past, with no delivery, timed out (cleat#947)", hist: []EventRecord{await(awaitT-1000, 60000), {Step: 1, EventType: fakeEvent}}, timeoutMs: 60000,
			want: awaitWant{timedOut: true, step: 1, replaying: true}},
		{name: "replay: an await at the end of history and a delivery in the store: replay ends, the delivery is recorded and consumed",
			deliver: map[string][]SignalDelivery{"a": {{ID: 7, Payload: "pa"}}}, hist: []EventRecord{await(awaitT-1000, 60000)}, timeoutMs: 60000,
			want: awaitWant{name: "a", payl: "pa", events: []EventType{EventTypeSignalReceived}, polls: []string{"a"}, consumed: []int64{7}, step: 2}},
		{name: "replay: an await at the end of history, nothing delivered, deadline ahead: suspends from the RECORDED deadline (cleat#933)",
			hist: []EventRecord{await(awaitT-10_000, 60000)}, timeoutMs: 60000,
			want: awaitWant{sentinel: true, polls: []string{"a", "b"}, reason: reasonReplayed(60000), until: awaitT + 50_000, step: 1}},
		{name: "replay: ... and the deadline already passed is a genuine timeout", hist: []EventRecord{await(awaitT-120_000, 60000)}, timeoutMs: 60000,
			want: awaitWant{timedOut: true, polls: []string{"a", "b"}, step: 1}},
		{name: "replay: ... a record with no timestamp cannot yield a deadline, so it suspends from now", hist: []EventRecord{await(0, 60000)}, timeoutMs: 60000,
			want: awaitWant{sentinel: true, polls: []string{"a", "b"}, reason: reasonReplayed(60000), until: awaitT + 60000, step: 1}},
		{name: "replay: ... a recorded timeout of 0 under a positive argument suspends with no usable deadline", hist: []EventRecord{await(awaitT, 0)}, timeoutMs: 60000,
			want: awaitWant{sentinel: true, polls: []string{"a", "b"}, reason: reasonReplayed(0), until: awaitT, step: 1}},
		{name: "replay: ... a recorded timeout of math.MaxInt64 does not wrap into the past (cleat#4105)", hist: []EventRecord{await(awaitT-1000, math.MaxInt64)}, timeoutMs: math.MaxInt64,
			want: awaitWant{sentinel: true, polls: []string{"a", "b"}, reason: reasonReplayed(math.MaxInt64), until: -1, step: 1}},
		{name: "replay: neither a delivery nor an await, with a wait: a divergence that consumes nothing (cleat#4157)", hist: []EventRecord{{Step: 0, EventType: fakeEvent}}, timeoutMs: 60000,
			want: awaitWant{diverged: true, step: 0, replaying: true}},
		{name: "replay: neither, and a no-wait poll: the timeout, the event left for the next call (histories before cleat#4151)", hist: []EventRecord{{Step: 0, EventType: fakeEvent}}, timeoutMs: 0,
			want: awaitWant{timedOut: true, step: 0, replaying: true}},
		{name: "replay: a delivery whose payload does not fit reports the truncation", hist: []EventRecord{delivered(0, "a", longPayload)}, timeoutMs: 60000, payMax: 200,
			want: awaitWant{errNonZero: true, name: "a", step: 1, replaying: true}},

		// ---- fresh ----
		{name: "fresh: a delivery waiting is recorded, consumed and returned", deliver: map[string][]SignalDelivery{"a": {{ID: 1, Payload: "pa"}}}, timeoutMs: 60000,
			want: awaitWant{name: "a", payl: "pa", events: []EventType{EventTypeSignalReceived}, polls: []string{"a"}, consumed: []int64{1}, step: 1}},
		{name: "fresh: the second name delivers when the first has none", deliver: map[string][]SignalDelivery{"b": {{ID: 2, Payload: "pb"}}}, timeoutMs: 60000,
			want: awaitWant{name: "b", payl: "pb", events: []EventType{EventTypeSignalReceived}, polls: []string{"a", "b"}, consumed: []int64{2}, step: 1}},
		{name: "fresh: nothing waiting records the await and suspends until now plus the timeout", timeoutMs: 60000,
			want: awaitWant{sentinel: true, events: []EventType{EventTypeAwaitSignals}, polls: []string{"a", "b"}, reason: reasonFresh, until: awaitT + 60000, step: 1}},
		{name: "fresh: a store error is read as nothing found, even when it arrives with a delivery", pollErr: fmt.Errorf("store down"), deliver: map[string][]SignalDelivery{"a": {{ID: 1, Payload: "pa"}}}, timeoutMs: 60000,
			want: awaitWant{sentinel: true, events: []EventType{EventTypeAwaitSignals}, polls: []string{"a", "b"}, reason: reasonFresh, until: awaitT + 60000, step: 1}},
		{name: "fresh: no signal store records the await and suspends without polling", noStore: true, timeoutMs: 60000,
			want: awaitWant{sentinel: true, events: []EventType{EventTypeAwaitSignals}, reason: reasonFresh, until: awaitT + 60000, step: 1}},
		{name: "fresh: a no-wait poll records the await and returns the timeout (cleat#4144)", timeoutMs: 0,
			want: awaitWant{timedOut: true, events: []EventType{EventTypeAwaitSignals}, polls: []string{"a", "b"}, step: 1}},
		{name: "fresh: a no-wait poll still reports a delivery that is waiting", deliver: map[string][]SignalDelivery{"a": {{ID: 1, Payload: "pa"}}}, timeoutMs: 0,
			want: awaitWant{name: "a", payl: "pa", events: []EventType{EventTypeSignalReceived}, polls: []string{"a"}, consumed: []int64{1}, step: 1}},
		{name: "fresh: a second await in a session that has already suspended is refused and records nothing (cleat#933)", timeoutMs: 60000,
			mod:  func(s *execSession) { s.suspendErr = &SuspendError{Reason: "earlier"} },
			want: awaitWant{sentinel: true, reason: "earlier"}},
		{name: "fresh: a defer segment's body is refused", opts: []EngineOption{WithDeferPhase()}, timeoutMs: 60000, want: awaitWant{sentinel: true}},
		{name: "fresh: a worker that is shutting down refuses", opts: []EngineOption{WithShutdownSignal(closed)}, timeoutMs: 60000, want: awaitWant{sentinel: true}},
		{name: "fresh: a delivery whose payload does not fit reports the truncation", deliver: map[string][]SignalDelivery{"a": {{ID: 1, Payload: longPayload}}}, timeoutMs: 60000, payMax: 200,
			want: awaitWant{errNonZero: true, name: "a", events: []EventType{EventTypeSignalReceived}, polls: []string{"a"}, consumed: []int64{1}, step: 1}},
		// ---- added on cleat-review3's condition: rows no earlier mutation reached ----
		{name: "replay: a store error on the frontier poll is read as nothing found, even when it arrives with a delivery",
			pollErr: fmt.Errorf("store down"), deliver: map[string][]SignalDelivery{"a": {{ID: 7, Payload: "pa"}}},
			hist: []EventRecord{await(awaitT-10_000, 60000)}, timeoutMs: 60000,
			want: awaitWant{sentinel: true, polls: []string{"a", "b"}, reason: reasonReplayed(60000), until: awaitT + 50_000, step: 1}},
		{name: "replay: the frontier with no signal store at all suspends from the recorded deadline without polling",
			noStore: true, hist: []EventRecord{await(awaitT-10_000, 60000)}, timeoutMs: 60000,
			want: awaitWant{sentinel: true, reason: reasonReplayed(60000), until: awaitT + 50_000, step: 1}},
		{name: "replay: an await then a delivery whose payload does not fit reports the truncation",
			hist: []EventRecord{await(awaitT-1000, 60000), delivered(1, "b", longPayload)}, timeoutMs: 60000, payMax: 200,
			want: awaitWant{errNonZero: true, name: "b", step: 2, replaying: true}},
		{name: "replay: the frontier poll's delivery whose payload does not fit reports the truncation",
			deliver: map[string][]SignalDelivery{"a": {{ID: 7, Payload: longPayload}}}, hist: []EventRecord{await(awaitT-1000, 60000)}, timeoutMs: 60000, payMax: 200,
			want: awaitWant{errNonZero: true, name: "a", events: []EventType{EventTypeSignalReceived}, polls: []string{"a"}, consumed: []int64{7}, step: 2}},
		{name: "replay: a delivery with no await before it: a delivery whose NAME does not fit reports the truncation",
			hist: []EventRecord{delivered(0, longName, "pa")}, timeoutMs: 60000, nameMax: 10,
			want: awaitWant{errNonZero: true, name: shortName, step: 1, replaying: true}},
		{name: "replay: an await then a delivery: a delivery whose NAME does not fit reports the truncation",
			hist: []EventRecord{await(awaitT-1000, 60000), delivered(1, longName, "pb")}, timeoutMs: 60000, nameMax: 10,
			want: awaitWant{errNonZero: true, name: shortName, step: 2, replaying: true}},
		{name: "replay: the frontier poll: a delivery whose NAME does not fit reports the truncation",
			deliver: map[string][]SignalDelivery{longName: {{ID: 7, Payload: "pa"}}}, hist: []EventRecord{{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: longNames, TimeoutMs: 60000, TimestampMs: awaitT - 1000}}, timeoutMs: 60000, nameMax: 10,
			want: awaitWant{errNonZero: true, name: shortName, events: []EventType{EventTypeSignalReceived}, polls: []string{longName}, consumed: []int64{7}, step: 2}},
		{name: "fresh: a delivery whose NAME does not fit reports the truncation",
			deliver: map[string][]SignalDelivery{longName: {{ID: 1, Payload: "pa"}}}, sigNames: longNames, timeoutMs: 60000, nameMax: 10,
			want: awaitWant{errNonZero: true, name: shortName, events: []EventType{EventTypeSignalReceived}, polls: []string{longName}, consumed: []int64{1}, step: 1}},
		{name: "fresh: arriving at the frontier of a replay leaves replay and behaves as fresh", hist: []EventRecord{{Step: 0, EventType: fakeEvent}}, stepCount: 1, timeoutMs: 60000,
			want: awaitWant{sentinel: true, events: []EventType{EventTypeAwaitSignals}, polls: []string{"a", "b"}, reason: reasonFresh, until: awaitT + 60000, step: 2}},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			w := &awaitWorld{deliveries: r.deliver, pollErr: r.pollErr}
			opts := append([]EngineOption{WithClock(func() int64 { return awaitT }), WithWorkflowID("wf-await")}, r.opts...)
			if !r.noStore {
				opts = append(opts, WithSignalStore(w))
			}
			s := &execSession{
				engine: NewEngine(nil, &mockCaller{}, opts...), workflowID: "wf-await", nowMs: awaitT,
				deferrals: map[string]string{}, queryState: map[string]string{},
				isReplay: len(r.hist) > 0, history: append([]EventRecord(nil), r.hist...), stepCount: r.stepCount,
			}
			if r.mod != nil {
				r.mod(s)
			}
			before := len(s.history)
			priorSuspend := s.suspendErr
			buf := make([]byte, 600)
			payMax := r.payMax
			if payMax == 0 {
				payMax = 200
			}
			sigNames := r.sigNames
			if sigNames == "" {
				sigNames = names
			}
			nameMax := r.nameMax
			if nameMax == 0 {
				nameMax = 200
			}
			got := s.DurableAwaitSignals(contextWithRawMemBuf(context.Background(), buf), nil, sigNames, r.timeoutMs, 0, nameMax, 256, payMax)

			nameLen, payLen, timedOut, errCode := decodeAwaitSignalsResultGuest(got)
			w2 := r.want
			isSentinel := got == callSuspendSentinel
			if isSentinel != w2.sentinel {
				t.Errorf("sentinel = %v, want %v (result %#x)", isSentinel, w2.sentinel, got)
			}
			if !isSentinel {
				if w2.diverged {
					if errCode != 1 {
						t.Errorf("want a divergence (errCode 1), got %#x", got)
					}
				} else {
					if timedOut != w2.timedOut {
						t.Errorf("timedOut = %v, want %v (result %#x)", timedOut, w2.timedOut, got)
					}
					if (errCode != 0) != w2.errNonZero {
						t.Errorf("errCode = %d, want non-zero=%v (result %#x)", errCode, w2.errNonZero, got)
					}
					if gotName := string(buf[:nameLen]); gotName != w2.name {
						t.Errorf("name written = %q, want %q", gotName, w2.name)
					}
					if !w2.errNonZero {
						if gotPay := string(buf[256 : 256+payLen]); gotPay != w2.payl {
							t.Errorf("payload written = %q, want %q", gotPay, w2.payl)
						}
					}
				}
			}
			if ev := evTypes(s.history, before); !awaitSameEventTypes(ev, w2.events) {
				t.Errorf("recorded %v, want %v", ev, w2.events)
			}
			if !awaitSameStrings(w.polls, w2.polls) {
				t.Errorf("polled %v, want %v", w.polls, w2.polls)
			}
			if !awaitSameInt64s(w.consumed, w2.consumed) {
				t.Errorf("consumed %v, want %v", w.consumed, w2.consumed)
			}
			switch {
			case w2.reason == "" && s.suspendErr != nil && s.suspendErr != priorSuspend:
				t.Errorf("set a suspension %+v, want none", s.suspendErr)
			case w2.reason != "":
				if s.suspendErr == nil {
					t.Fatalf("no suspension, want %q", w2.reason)
				}
				if s.suspendErr.Reason != w2.reason {
					t.Errorf("suspension reason = %q, want %q", s.suspendErr.Reason, w2.reason)
				}
				if w2.until == -1 {
					if !s.suspendErr.Until.After(time.UnixMilli(awaitT)) {
						t.Errorf("suspension Until = %v, want after the clock", s.suspendErr.Until)
					}
				} else if w2.until != 0 && !s.suspendErr.Until.Equal(time.UnixMilli(w2.until)) {
					t.Errorf("suspension Until = %v, want %v", s.suspendErr.Until, time.UnixMilli(w2.until))
				}
			}
			if s.stepCount != w2.step {
				t.Errorf("stepCount = %d, want %d", s.stepCount, w2.step)
			}
			if s.isReplay != w2.replaying {
				t.Errorf("isReplay = %v, want %v", s.isReplay, w2.replaying)
			}
		})
	}
}

func awaitSameStrings(a, b []string) bool {
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

func awaitSameInt64s(a, b []int64) bool {
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

func awaitSameEventTypes(a, b []EventType) bool {
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
