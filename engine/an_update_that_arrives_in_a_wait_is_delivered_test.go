package engine

// cleat#4511: an update that arrives after a run suspended in a wait. See an_update_interrupts_a_wait.go for the design, and the issue for the measurement.
//
// These rows are RED on the tree before the fix (the poll answered "nothing found" onto the wait's own row and the wait re-suspended) and say why in their
// messages. The ones about shapes that must NOT change are green on both, and are there because the fix is a replay-core change: each pins a history the
// tree already writes and reads.

import (
	"bytes"
	"context"
	"testing"
	"time"
)

const intT = awaitT // the engine's clock for every row, in ms

type intWait struct {
	name string
	row  EventRecord // the row the wait left when it suspended the run (unanswered)
	// answered is the same row once the wait has an answer, which must not be crossed.
	answered *EventRecord
	opts     func() []EngineOption
}

func intWaits() []intWait {
	return []intWait{
		{
			name: "await_signals",
			row:  EventRecord{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: awaitNames, TimeoutMs: 3_600_000, TimestampMs: intT - 3000},
			opts: func() []EngineOption {
				return []EngineOption{WithSignalStore(&awaitWorld{deliveries: map[string][]SignalDelivery{}})}
			},
		},
		{
			name: "await_promise",
			row:  EventRecord{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p1", TimeoutMs: 3_600_000, TimestampMs: intT - 3000},
			opts: func() []EngineOption { return []EngineOption{WithPromiseStore(&promiseWorld{status: "pending"})} },
		},
		{
			name:     "await_child",
			row:      EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "c1", TimestampMs: intT - 3000},
			answered: &EventRecord{Step: 0, EventType: EventTypeAwaitChild, RunID: "c1", Response: `{"run_id":"c1"}`, TimestampMs: intT - 3000},
			opts:     func() []EngineOption { return nil },
		},
		{
			name:     "await_all_children",
			row:      EventRecord{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1"]`, TimestampMs: intT - 3000},
			answered: &EventRecord{Step: 0, EventType: EventTypeAwaitAllChildren, Request: `["c1"]`, Response: `[{"run_id":"c1"}]`, TimestampMs: intT - 3000},
			opts:     func() []EngineOption { return nil },
		},
		{
			name:     "await_any_child",
			row:      EventRecord{Step: 0, EventType: EventTypeAwaitAnyChild, Request: `["c1"]`, TimestampMs: intT - 3000},
			answered: &EventRecord{Step: 0, EventType: EventTypeAwaitAnyChild, Request: `["c1"]`, Response: `{"run_id":"c1"}`, TimestampMs: intT - 3000},
			opts:     func() []EngineOption { return nil },
		},
	}
}

var intUpdate = UpdateRequestInfo{UpdateName: "bump", Payload: `{"i":0}`, PromiseID: "prom-1"}

// intSession is a replaying session over hist with one pending update, a clock the test can move, and the stores a wait needs.
func intSession(t *testing.T, hist []EventRecord, store *fakeUpdateStore, clock *int64, more ...EngineOption) *execSession {
	t.Helper()
	opts := append([]EngineOption{WithClock(func() int64 { return *clock }), WithWorkflowID("wf-4511"), WithUpdateStore(store)}, more...)
	return &execSession{
		engine: NewEngine(nil, &mockCaller{}, opts...), workflowID: "wf-4511", nowMs: *clock,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: len(hist) > 0, history: append([]EventRecord(nil), hist...),
	}
}

func intPoll(t *testing.T, s *execSession) (updateDelivery, bool) {
	t.Helper()
	buf := make([]byte, 1024)
	ctx := ctxWithMem(context.Background(), buf)
	return decodeDelivery(t, ctx, buf, s.DurablePollUpdate(ctx, nil, 0, uint32(len(buf))))
}

func evTypesOf(h []EventRecord) []EventType {
	out := make([]EventType, len(h))
	for i := range h {
		out[i] = h[i].EventType
	}
	return out
}

// THE FIX. The run suspended in the wait, an update arrived, the wake replays: the poll in front of the wait must deliver it, record it marked, and leave
// the wait row where it is (a recorded history takes no event in its middle).
func TestAPollInFrontOfTheWaitASuspendedRunIsWaitingInDeliversTheUpdateThatArrived(t *testing.T) {
	for _, w := range intWaits() {
		t.Run(w.name, func(t *testing.T) {
			clock := intT
			store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
			s := intSession(t, []EventRecord{w.row}, store, &clock, w.opts()...)
			d, found := intPoll(t, s)
			if !found {
				t.Fatalf("the poll answered NOTHING FOUND with an update pending and the run suspended in %s: it replayed onto the wait's own row, consumed nothing, and "+
					"the wait re-suspends with the update still pending (cleat#4511)", w.name)
			}
			if d.Name != "bump" || d.Payload != `{"i":0}` {
				t.Errorf("delivered %+v, want the pending update", d)
			}
			if got, want := evTypesOf(s.history), []EventType{w.row.EventType, EventTypeUpdateReceived}; !awaitSameEventTypes(got, want) {
				t.Fatalf("history is %v, want %v: the wait row stays, the delivery lands after it", got, want)
			}
			if !s.history[1].UpdateInterruptsWait {
				t.Error("the delivery is not marked: a replay could not tell it from a poll that found an update after a wait that timed out")
			}
			if s.history[1].Step != 1 {
				t.Errorf("the delivery is recorded at step %d, want 1", s.history[1].Step)
			}
			if s.isReplay {
				t.Error("replay was not left: a delivery is new work, recorded at the frontier")
			}
		})
	}
}

// What the marker is for: [wait row, update_received] means one thing when the update is marked and another when it is not.
func TestAMarkedDeliveryAfterAWaitRowReplaysFromHistoryAndNeverReadsTheStore(t *testing.T) {
	for _, w := range intWaits() {
		t.Run(w.name, func(t *testing.T) {
			clock := intT
			// The store holds a DIFFERENT request: delivering it would be a poll that reached past history.
			store := &fakeUpdateStore{pending: []UpdateRequestInfo{{UpdateName: "arrived-later", Payload: `{"n":99}`, PromiseID: "prom-9"}}}
			hist := []EventRecord{w.row, {Step: 1, EventType: EventTypeUpdateReceived, UpdateHandlerName: "bump", UpdatePayload: `{"i":0}`, UpdateRequestID: "u3:4:bump0:prom-1", UpdateInterruptsWait: true}}
			s := intSession(t, hist, store, &clock, w.opts()...)
			d, found := intPoll(t, s)
			if !found || d.Name != "bump" || d.Payload != `{"i":0}` {
				t.Fatalf("replayed %+v found=%v, want the recorded delivery of bump", d, found)
			}
			if s.stepCount != 2 || len(s.history) != 2 {
				t.Errorf("stepCount %d, history %d: both records must be consumed and nothing appended", s.stepCount, len(s.history))
			}
		})
	}
}

// THE SHAPE THAT MUST NOT CHANGE. An await that timed out, then the NEXT call's poll found an update, is [await, update_received] with no marker. The
// poll in front of that await answered nothing found on the original run, and still must: reading the pair as an interruption would run the handler
// before the code that followed the timeout.
func TestAnUnmarkedDeliveryAfterAWaitRowIsStillTheNextCallsAndIsNotReadAcrossTheWait(t *testing.T) {
	for _, w := range intWaits() {
		t.Run(w.name, func(t *testing.T) {
			clock := intT
			store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
			hist := []EventRecord{w.row, {Step: 1, EventType: EventTypeUpdateReceived, UpdateHandlerName: "bump", UpdatePayload: `{"i":0}`, UpdateRequestID: "u3:4:bump0:prom-1"}}
			s := intSession(t, hist, store, &clock, w.opts()...)
			if _, found := intPoll(t, s); found {
				t.Fatal("the poll in front of a wait delivered a record that follows that wait UNMARKED: that is the next call's delivery")
			}
			if s.stepCount != 0 || !s.isReplay {
				t.Errorf("stepCount %d isReplay %v: nothing is consumed and replay is not left, as before", s.stepCount, s.isReplay)
			}
		})
	}
}

// A timed wait whose deadline has passed is not the row a suspended run is waiting in: an await records nothing for its timeout, so after it times out
// the guest's next suspension (a Sleep records nothing either) leaves it as the last row. An update that arrives in THAT suspension belongs after the
// code that ran after the timeout (cleat-review2's reading of the design).
func TestAnExpiredTimedWaitRowIsNotCrossedByAPoll(t *testing.T) {
	for _, w := range intWaits() {
		if w.row.TimeoutMs == 0 {
			continue // a child wait has no deadline
		}
		t.Run(w.name, func(t *testing.T) {
			clock := intT + 3_600_000 // the wait's deadline (row stamp + timeout = intT + 3_597_000) has passed
			store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
			s := intSession(t, []EventRecord{w.row}, store, &clock, w.opts()...)
			if _, found := intPoll(t, s); found {
				t.Fatal("the poll delivered across a wait row whose deadline had passed: the update would run before the code that followed the timeout")
			}
			if len(store.pending) != 1 || len(s.history) != 1 {
				t.Errorf("pending %d history %d: nothing is consumed or recorded", len(store.pending), len(s.history))
			}
		})
	}
}

func TestAWaitRowThatIsAnsweredOrNotTheLastIsNotCrossedAndNothingPendingChangesNothing(t *testing.T) {
	for _, w := range intWaits() {
		t.Run(w.name+"/nothing pending", func(t *testing.T) {
			clock := intT
			store := &fakeUpdateStore{}
			s := intSession(t, []EventRecord{w.row}, store, &clock, w.opts()...)
			if _, found := intPoll(t, s); found {
				t.Fatal("delivered with nothing pending")
			}
			if s.stepCount != 0 || !s.isReplay {
				t.Errorf("stepCount %d isReplay %v: the row stays for the wait that owns it, and replay is not left", s.stepCount, s.isReplay)
			}
		})
		t.Run(w.name+"/not the last row", func(t *testing.T) {
			clock := intT
			store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
			later := EventRecord{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `{}`}
			s := intSession(t, []EventRecord{w.row, later}, store, &clock, w.opts()...)
			if _, found := intPoll(t, s); found {
				t.Fatal("delivered across a wait row that history had moved past")
			}
		})
		if w.answered != nil {
			t.Run(w.name+"/answered", func(t *testing.T) {
				clock := intT
				store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
				s := intSession(t, []EventRecord{*w.answered}, store, &clock, w.opts()...)
				if _, found := intPoll(t, s); found {
					t.Fatal("delivered across a wait row that already has its answer")
				}
			})
		}
	}
}

// A defer segment, a stopping worker: a delivery is new work, and the refusal every fresh poll has applies BEFORE the wait row is consumed.
func TestADeliveryAcrossAWaitRowIsRefusedWhereAFreshPollIsAndConsumesNothing(t *testing.T) {
	clock := intT
	w := intWaits()[0]
	store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
	s := intSession(t, []EventRecord{w.row}, store, &clock, w.opts()...)
	s.engine.deferPhase = true // stopBeforeNewWork: a defer segment starts no new work
	if _, found := intPoll(t, s); found {
		t.Fatal("a defer segment delivered an update across a wait row")
	}
	if s.stepCount != 0 || len(s.history) != 1 {
		t.Errorf("stepCount %d history %d: a refused poll leaves the history as it found it", s.stepCount, len(s.history))
	}
}

// ---- the deadline the wait after the update keeps ----

func intCarryRun(t *testing.T, w intWait, complete bool, afterPoll func(s *execSession, clock *int64), guestTimeoutMs int64, names string) (*execSession, EventRecord) {
	t.Helper()
	clock := intT
	store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
	s := intSession(t, []EventRecord{w.row}, store, &clock, w.opts()...)
	d, found := intPoll(t, s)
	if !found {
		t.Fatalf("the update was not delivered, so the deadline carry is unmeasured")
	}
	if afterPoll != nil {
		afterPoll(s, &clock)
	}
	if complete {
		completeIt(t, s, d)
	}
	switch w.row.EventType {
	case EventTypeAwaitSignals:
		callAwaitSignalsNames(s, names, guestTimeoutMs)
	case EventTypeAwaitPromise:
		s.AwaitPromise(contextWithRawMemBuf(context.Background(), make([]byte, 400)), nil, "p1", guestTimeoutMs, 0, 200)
	}
	return s, s.history[len(s.history)-1]
}

func callAwaitSignalsNames(s *execSession, names string, timeoutMs int64) {
	s.DurableAwaitSignals(contextWithRawMemBuf(context.Background(), make([]byte, 600)), nil, names, timeoutMs, 0, 200, 256, 200)
}

func completeIt(t *testing.T, s *execSession, d updateDelivery) {
	t.Helper()
	if r := s.DurableCompleteUpdate(context.Background(), nil, d.RequestID, `{"n":1}`, ""); r != 0 {
		t.Fatalf("complete_update answered %#x", r)
	}
}

// The guest called AwaitSignals once; the SDK's poll and its second wait are inside that call. An update must not restart its timeout.
func TestTheWaitAfterAnUpdateKeepsTheDeadlineOfTheWaitTheUpdateInterrupted(t *testing.T) {
	for _, w := range intWaits() {
		if w.row.TimeoutMs == 0 {
			continue
		}
		t.Run(w.name, func(t *testing.T) {
			clock := intT
			store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
			s := intSession(t, []EventRecord{w.row}, store, &clock, w.opts()...)
			d, found := intPoll(t, s)
			if !found {
				t.Fatal("not delivered")
			}
			completeIt(t, s, d)
			// The SDK's wait call, with the same arguments the first one had.
			if w.row.EventType == EventTypeAwaitSignals {
				callAwaitSignalsNames(s, awaitNames, 3_600_000)
			} else {
				s.AwaitPromise(contextWithRawMemBuf(context.Background(), make([]byte, 400)), nil, "p1", 3_600_000, 0, 200)
			}
			got := s.history[len(s.history)-1]
			if got.EventType != w.row.EventType {
				t.Fatalf("the last record is %s, want the second %s", got.EventType, w.row.EventType)
			}
			wantDeadline := w.row.TimestampMs + w.row.TimeoutMs
			if got.TimestampMs+got.TimeoutMs != wantDeadline {
				t.Errorf("the second wait's deadline is stamp %d + timeout %d = %d, want the first wait's %d: an update restarted the guest's timeout",
					got.TimestampMs, got.TimeoutMs, got.TimestampMs+got.TimeoutMs, wantDeadline)
			}
			if got.TimeoutMs >= 3_600_000 {
				t.Errorf("recorded the guest's full %d ms again", got.TimeoutMs)
			}
		})
	}
}

// A wait the HANDLER makes comes before the completion and is not the one the update interrupted.
func TestAWaitTheHandlerMakesBeforeTheCompletionDoesNotTakeTheCarry(t *testing.T) {
	w := intWaits()[0]
	s, got := intCarryRun(t, w, false, nil, 3_600_000, awaitNames)
	if got.TimeoutMs != 3_600_000 {
		t.Errorf("a wait made before the update's completion recorded timeout %d, want the guest's own 3600000", got.TimeoutMs)
	}
	if s.waitCarry != nil {
		t.Error("a carry is still held after a wait ran")
	}
}

func TestOnlyTheFirstWaitAfterTheCompletionTakesTheCarryAndOnlyIfItIsTheSameWait(t *testing.T) {
	w := intWaits()[0]
	clock := intT
	store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
	s := intSession(t, []EventRecord{w.row}, store, &clock, w.opts()...)
	d, _ := intPoll(t, s)
	completeIt(t, s, d)
	// A DIFFERENT wait first (other names): it keeps its own timeout, and uses up the carry.
	callAwaitSignalsNames(s, `["other"]`, 3_600_000)
	if got := s.history[len(s.history)-1]; got.TimeoutMs != 3_600_000 {
		t.Errorf("a wait on other names recorded timeout %d, want its own 3600000", got.TimeoutMs)
	}
	if s.waitCarry != nil {
		t.Error("the carry survived a wait that did not take it")
	}
}

// A deadline that has passed by the time the second wait is made must come out as a wait that is over, not as no deadline: a timeout of zero or less is read
// by the wait's replay as an invented one and suspends.
func TestACarriedDeadlineThatHasPassedIsOneMillisecondAndNeverZero(t *testing.T) {
	w := intWaits()[0]
	_, got := intCarryRun(t, w, true, func(s *execSession, clock *int64) { *clock = intT + 3_600_000 }, 3_600_000, awaitNames)
	if got.TimeoutMs != 1 {
		t.Errorf("recorded timeout %d for a deadline that had passed, want 1", got.TimeoutMs)
	}
}

func TestACarriedDeadlineNeverOutlastsWhatTheGuestAskedFor(t *testing.T) {
	w := intWaits()[0]
	_, got := intCarryRun(t, w, true, nil, 1000, awaitNames)
	if got.TimeoutMs != 1000 {
		t.Errorf("recorded timeout %d, want the guest's own 1000 (shorter than what is left)", got.TimeoutMs)
	}
}

// A handler that suspends is replayed: the carry has to be rebuilt from the recorded delivery, or the wait after the replay restarts the timeout.
func TestTheCarryIsRebuiltWhenTheDeliveryIsReplayedFromHistory(t *testing.T) {
	w := intWaits()[0]
	clock := intT
	store := &fakeUpdateStore{}
	hist := []EventRecord{w.row, {Step: 1, EventType: EventTypeUpdateReceived, UpdateHandlerName: "bump", UpdatePayload: `{"i":0}`, UpdateRequestID: "u3:4:bump0:prom-1", UpdateInterruptsWait: true}}
	s := intSession(t, hist, store, &clock, w.opts()...)
	d, found := intPoll(t, s)
	if !found {
		t.Fatal("the recorded delivery was not replayed")
	}
	completeIt(t, s, d) // fresh: history ended with the delivery
	callAwaitSignalsNames(s, awaitNames, 3_600_000)
	got := s.history[len(s.history)-1]
	if got.TimestampMs+got.TimeoutMs != w.row.TimestampMs+w.row.TimeoutMs {
		t.Errorf("deadline %d after a replayed delivery, want %d", got.TimestampMs+got.TimeoutMs, w.row.TimestampMs+w.row.TimeoutMs)
	}
}

// ---- the marker is part of the record, and part of nothing that was written before it ----

func TestTheMarkerIsInTheChecksumOnlyWhenSetAndSurvivesTheStoreAndCompaction(t *testing.T) {
	plain := EventRecord{Step: 1, EventType: EventTypeUpdateReceived, UpdateHandlerName: "bump", UpdatePayload: `{"i":0}`, UpdateRequestID: "k"}
	marked := plain
	marked.UpdateInterruptsWait = true

	pp, err := eventRecordToPayload(plain)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := eventRecordToPayload(marked)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pp, []byte("update_interrupts_wait")) {
		t.Errorf("an unmarked record's payload %s carries the key: every update_received written before the marker would verify differently", pp)
	}
	if !bytes.Contains(mp, []byte(`"update_interrupts_wait":true`)) {
		t.Errorf("a marked record's payload %s does not carry the marker", mp)
	}
	if computeEventChecksum(plain, "") == computeEventChecksum(marked, "") {
		t.Error("the marker is not in the checksum: a worker that does not know the key would verify the row and misread the pair (cleat#4511, Rolling back)")
	}

	back := EventRecord{EventType: EventTypeUpdateReceived}
	populateFromPayload(&back, mp)
	if !back.UpdateInterruptsWait {
		t.Error("the marker did not survive the payload round trip")
	}
	plainBack := EventRecord{EventType: EventTypeUpdateReceived}
	populateFromPayload(&plainBack, pp)
	if plainBack.UpdateInterruptsWait {
		t.Error("an unmarked record came back marked")
	}

	cs, err := extractCompactionStateFrom(nil, []EventRecord{
		{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: awaitNames, TimeoutMs: 3_600_000, TimestampMs: intT - 3000}, marked, plain,
	})
	if err != nil {
		t.Fatal(err)
	}
	full := buildFullHistoryFromCompaction(nil, cs)
	if len(full) != 3 || !full[1].UpdateInterruptsWait || full[2].UpdateInterruptsWait {
		t.Errorf("after compaction the markers read %v %v, want true false", full[1].UpdateInterruptsWait, full[2].UpdateInterruptsWait)
	}
}

// A KNOWN GAP, PINNED SO IT CANNOT BE SILENT (cleat#4511 names it in its PR). wait_for_event is a composite: a claim attempt (a plugin_call row) and then an
// await_signals, so a run suspended in it ends in [..., plugin_call, await_signals], and the SDK's poll (Rust, Java and TypeScript dispatch before the call; the Go SDK
// does not dispatch before it at all) sits in front of the CLAIM row, two rows before the end. The fix reads only a poll whose very next row is the wait's own, so
// this poll still answers nothing found and the update waits for the wait to end.
//
// The history here is built by hand from the composite's two record kinds; it was NOT produced by driving the real composite, and nothing here measures a worker.
// Delete this test, and say so, when the follow-up that covers the composite lands: it will be red the day it does.
func TestAnUpdateDuringAWaitForEventIsNotDeliveredYetBecauseThePollSitsInFrontOfItsClaimRow(t *testing.T) {
	clock := intT
	store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate}}
	hist := []EventRecord{
		{Step: 0, EventType: EventTypePluginCall, PluginName: "hooks", PluginFunc: "claim", PluginInput: `{}`, PluginOutput: `{"found":false}`, TimestampMs: intT - 3000},
		{Step: 1, EventType: EventTypeAwaitSignals, SignalNames: `["__evt:hook"]`, TimeoutMs: 3_600_000, TimestampMs: intT - 3000},
	}
	s := intSession(t, hist, store, &clock, WithSignalStore(&awaitWorld{deliveries: map[string][]SignalDelivery{}}))
	if _, found := intPoll(t, s); found {
		t.Fatal("the update WAS delivered: the composite is now covered, so this pin is stale. Delete it, and the 'not covered' lines in the PR and the changelog entry")
	}
	if s.stepCount != 0 || !s.isReplay {
		t.Errorf("stepCount %d isReplay %v: a poll that finds a claim row consumes nothing", s.stepCount, s.isReplay)
	}
}

// A fresh timed wait measures its deadline from the clock AFTER its own record, as it always did: recording moves the session's clock to the event's timestamp, so a
// deadline taken before it is earlier by however long it had been since the last event. The first version of cleat#4511's change took it before, to fit the carry in,
// and a held wait then looked too close to hold; only the real-guest held-signal test saw it. The fake clock the other rows use is in the future of the real one, so
// recording never moves it, and it could not: this row has a clock that is BEHIND the real one.
func TestAFreshTimedWaitMeasuresItsDeadlineFromTheClockAfterItsOwnRecord(t *testing.T) {
	real := func() int64 { return time.Now().UnixMilli() }
	t.Run("await_signals", func(t *testing.T) {
		w := &awaitWorld{deliveries: map[string][]SignalDelivery{}}
		ws := &scriptedWake{answer: func(int, parkRequest) parkOutcome { return parkRefused }}
		s := awaitParkSession(w, ws, nil, WithClock(real))
		s.nowMs = real() - 60_000 // the last event was a minute ago
		callAwaitSignals(s, 60000)
		rec := s.history[len(s.history)-1]
		if rec.EventType != EventTypeAwaitSignals || len(ws.reqs) != 1 {
			t.Fatalf("recorded %v, parked %d times: the wait did not record and park once", evTypesOf(s.history), len(ws.reqs))
		}
		if want := rec.TimestampMs + 60000; ws.reqs[0].DeadlineMs != want {
			t.Errorf("the wait parked against deadline %d, want the record's stamp + 60 s = %d (off by %d ms): the deadline was measured from the clock before the record", ws.reqs[0].DeadlineMs, want, ws.reqs[0].DeadlineMs-want)
		}
	})
	t.Run("await_promise", func(t *testing.T) {
		ws := &scriptedWake{answer: func(int, parkRequest) parkOutcome { return parkRefused }}
		s := awaitParkSession(&awaitWorld{deliveries: map[string][]SignalDelivery{}}, ws, nil, WithClock(real), WithPromiseStore(&promiseWorld{status: "pending"}))
		s.nowMs = real() - 60_000
		s.AwaitPromise(contextWithRawMemBuf(context.Background(), make([]byte, 400)), nil, "p1", 60000, 0, 200)
		rec := s.history[len(s.history)-1]
		if rec.EventType != EventTypeAwaitPromise || len(ws.reqs) != 1 {
			t.Fatalf("recorded %v, parked %d times: the wait did not record and park once", evTypesOf(s.history), len(ws.reqs))
		}
		if want := rec.TimestampMs + 60000; ws.reqs[0].DeadlineMs != want {
			t.Errorf("the wait parked against deadline %d, want the record's stamp + 60 s = %d (off by %d ms)", ws.reqs[0].DeadlineMs, want, ws.reqs[0].DeadlineMs-want)
		}
	})
}

// cleat-review2's GAP on #4536, both cases measured on bf7d911e and pinned here. The carry is per update, but the run can handle SEVERAL updates in one wake (the SDK's dispatch
// loop polls until nothing is pending), and two things then went wrong. A: u2's completion cleared the carry u1's completion had readied, because it had none of its own, so the
// guest's wait got its full timeout back. B: u2's HANDLER made a wait of the same names, which took u1's carry, because "the first wait after u1's completion" was the handler's.
// The fix tracks the updates that are delivered and not yet completed: a completion with no carry of its own leaves a standing one alone, and no wait takes (or clears) the
// carry while an update is open, because a wait made then is the handler's.
var intUpdate2 = UpdateRequestInfo{UpdateName: "bump2", Payload: `{"i":1}`, PromiseID: "prom-2"}

func intTwoUpdateSession(t *testing.T, w intWait, clock *int64) *execSession {
	t.Helper()
	store := &fakeUpdateStore{pending: []UpdateRequestInfo{intUpdate, intUpdate2}}
	return intSession(t, []EventRecord{w.row}, store, clock, w.opts()...)
}

func intWaitAgain(s *execSession, w intWait, names string, timeoutMs int64) {
	if w.row.EventType == EventTypeAwaitSignals {
		callAwaitSignalsNames(s, names, timeoutMs)
	} else {
		s.AwaitPromise(contextWithRawMemBuf(context.Background(), make([]byte, 400)), nil, "p1", timeoutMs, 0, 200)
	}
}

func TestTwoUpdatesHandledInOneWakeLeaveTheGuestsWaitItsOriginalDeadline(t *testing.T) {
	for _, w := range intWaits() {
		if w.row.TimeoutMs == 0 {
			continue
		}
		t.Run(w.name, func(t *testing.T) {
			clock := intT
			s := intTwoUpdateSession(t, w, &clock)
			d1, found := intPoll(t, s)
			if !found {
				t.Fatal("u1 not delivered")
			}
			completeIt(t, s, d1)
			d2, found := intPoll(t, s)
			if !found || d2.RequestID == d1.RequestID {
				t.Fatalf("u2 not delivered as its own request (found %v, ids %q and %q)", found, d1.RequestID, d2.RequestID)
			}
			completeIt(t, s, d2)
			intWaitAgain(s, w, awaitNames, 3_600_000)
			got := s.history[len(s.history)-1]
			if got.EventType != w.row.EventType {
				t.Fatalf("the last record is %s, want the guest's wait %s", got.EventType, w.row.EventType)
			}
			if want := w.row.TimestampMs + w.row.TimeoutMs; got.TimestampMs+got.TimeoutMs != want {
				t.Errorf("after TWO updates in one wake the guest's wait has deadline %d (timeout %d), want the first wait's %d: the second completion cleared the carry the first had readied",
					got.TimestampMs+got.TimeoutMs, got.TimeoutMs, want)
			}
			// The deadline alone cannot tell: this session's clock stands still, so a record with no carry is stamped at the first wait's own timestamp and the sum
			// comes out the same. The timeout is what says whether the guest's full hour was given back.
			if got.TimeoutMs >= 3_600_000 {
				t.Errorf("the guest's wait recorded the full %d ms again after two updates", got.TimeoutMs)
			}
		})
	}
}

func TestAWaitTheSecondUpdatesHandlerMakesDoesNotTakeTheFirstUpdatesCarry(t *testing.T) {
	w := intWaits()[0]
	t.Run("the handler's wait suspends: it records its own timeout", func(t *testing.T) {
		clock := intT
		s := intTwoUpdateSession(t, w, &clock)
		d1, _ := intPoll(t, s)
		completeIt(t, s, d1)
		if _, found := intPoll(t, s); !found {
			t.Fatal("u2 not delivered")
		}
		callAwaitSignalsNames(s, awaitNames, 3_600_000) // u2's handler, before u2's completion
		got := s.history[len(s.history)-1]
		if got.EventType != EventTypeAwaitSignals || got.TimeoutMs != 3_600_000 {
			t.Errorf("the handler's wait recorded %s with timeout %d, want its own 3600000: it took the carry of the update that had already completed", got.EventType, got.TimeoutMs)
		}
		if s.waitCarry == nil {
			t.Error("the handler's wait cleared the carry, so the guest's own wait will not find it")
		}
	})
	t.Run("the handler's wait is answered: the guest's wait still gets the carry", func(t *testing.T) {
		clock := intT
		s := intTwoUpdateSession(t, w, &clock)
		world := &awaitWorld{deliveries: map[string][]SignalDelivery{"a": {{ID: 7, Payload: `{}`}}}}
		s.engine.signalStore = world
		d1, _ := intPoll(t, s)
		completeIt(t, s, d1)
		d2, found := intPoll(t, s)
		if !found {
			t.Fatal("u2 not delivered")
		}
		callAwaitSignalsNames(s, awaitNames, 3_600_000) // answered at once: records no await row
		completeIt(t, s, d2)
		world.deliveries = map[string][]SignalDelivery{} // the handler's signal is consumed: the guest's wait has nothing to find and records its await
		callAwaitSignalsNames(s, awaitNames, 3_600_000)  // the guest's wait
		got := s.history[len(s.history)-1]
		if got.EventType != EventTypeAwaitSignals || got.TimestampMs+got.TimeoutMs != w.row.TimestampMs+w.row.TimeoutMs {
			t.Errorf("the guest's wait recorded %s with deadline %d, want the first wait's %d", got.EventType, got.TimestampMs+got.TimeoutMs, w.row.TimestampMs+w.row.TimeoutMs)
		}
		if got.TimeoutMs >= 3_600_000 {
			t.Errorf("the guest's wait recorded the full %d ms: the handler's answered wait used up the carry", got.TimeoutMs)
		}
	})
}

// The same two cases from a RECORDED history, because the open-update set is rebuilt on a replay by the same two places that fill it on the original run.
func TestTheOpenUpdatesAndTheCarryAreRebuiltWhenTwoUpdatesAreReplayedFromHistory(t *testing.T) {
	w := intWaits()[0]
	clock := intT
	s := intTwoUpdateSession(t, w, &clock)
	d1, _ := intPoll(t, s)
	completeIt(t, s, d1)
	d2, _ := intPoll(t, s)
	completeIt(t, s, d2)
	hist := append([]EventRecord(nil), s.history...)
	// A replay of that history: both deliveries, both completions, then the guest's wait, on a store with nothing pending.
	r := intSession(t, hist, &fakeUpdateStore{}, &clock, w.opts()...)
	if _, found := intPoll(t, r); !found {
		t.Fatal("replay: u1 not delivered from history")
	}
	completeIt(t, r, d1)
	if _, found := intPoll(t, r); !found {
		t.Fatal("replay: u2 not delivered from history")
	}
	// u2 is delivered and not completed: a wait made now is its handler's, and neither takes nor clears the carry u1 readied.
	if len(r.openUpdates) != 1 {
		t.Errorf("a replayed delivery left %d update(s) open, want 1: the open set is not rebuilt from history", len(r.openUpdates))
	}
	if got := r.takeWaitCarry(); got != nil || r.waitCarry == nil {
		t.Errorf("a wait made inside u2's handler took %v and left the carry %v, want it to take nothing and leave the carry", got, r.waitCarry)
	}
	completeIt(t, r, d2)
	if r.waitCarry == nil || len(r.openUpdates) != 0 {
		t.Errorf("after replaying both completions the carry is %v and %d update(s) are open, want the carry held and none open", r.waitCarry, len(r.openUpdates))
	}
}
