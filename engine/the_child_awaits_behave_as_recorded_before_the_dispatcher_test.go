package engine

// W3 of cleat#4152, step 0: a table recorded from AwaitChild, AwaitAllChildren and AwaitAnyChild as they are BEFORE they move onto the step
// dispatcher, written and run against the unmodified code, so that after the move a changed row can only be a deliberate one (the same method as
// the signal await's table, #4250, and the promise await's, W4).
//
// What makes these three different from the awaits already moved, and what the rows exist to pin:
//
//   - AwaitChild and AwaitAllChildren record their answer OVER the pending row. A pending await is recorded EMPTY (no Response, no Err), and the
//     next wake re-checks the store and records the answer at the SAME step: the replay does not consume the empty record (stepCount is not
//     advanced), leaves replay, and the fresh path writes at the step the empty record holds (the store's conflict clause reopens it). A
//     dispatcher step consumes the records it reads, so this is a new shape for it.
//   - AwaitAnyChild records the answer at the NEXT step: it consumes the empty record, then SKIPS every following empty record of the same call
//     (histories written before cleat#3391 hold two) and delivers a non-empty one if it follows; anything else falls to the fresh path with history
//     still unread, and a call that is resuming records no second empty row.
//   - None of the three has a stopBeforeNewWork refusal, and AwaitAnyChild alone has the "a segment that has already suspended does not start
//     another await" guard (cleat#3391's neighbour, the same guard as AwaitSignal).
//   - AwaitChild consumes a replayed answer WITHOUT comparing the child's run id, AwaitAnyChild without comparing its run-id set (only the
//     skipping of empties compares it), and AwaitAllChildren compares its request but consumes the record BEFORE checking the type or the request,
//     so its divergences consume one record where the dispatcher's consume none (cleat#4157).
//   - A store error is not "still running": AwaitChild and AwaitAnyChild and AwaitAllChildren all record it as the child's ANSWER, durably, which is
//     a decision of cleat#1115's, kept by every row below that says so.
//
// It uses a fake child store, because the question is what the CALL does with a store, not what a database does. Rows are recorded as the code
// behaves, whether or not that is right; a row that says KNOWN DEFECT names the defect and is not an endorsement.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

type childAwaitCall int

const (
	callOfAwaitChild childAwaitCall = iota
	callOfAwaitAllChildren
	callOfAwaitAnyChild
)

// childAwaitRec is one record a call wrote, reduced to what the rows assert on.
type childAwaitRec struct {
	typ                EventType
	resp, errText, req string
	runID              string
}

type childAwaitWant struct {
	sentinel  bool   // the call returned packAwaitChildResultSuspend(), the child awaits' own suspend word (see the check below)
	errCode   uint32 // the low 32 bits of the word
	result    string // what was written to the guest's buffer
	resultHas string // when set, the written result must CONTAIN this instead of equal result (a divergence message, an error text)
	recs      []childAwaitRec
	reason    string // the session's suspension reason after the call; "" = none
	step      int    // stepCount after the call
	replaying bool   // isReplay after the call
	gets      int    // store reads
	getOrder  []string
}

type childAwaitRow struct {
	name     string
	call     childAwaitCall
	ids      []string
	idsJSON  string // overrides the marshalled ids, for a request that is not valid JSON
	opts     []EngineOption
	noStore  bool
	outcomes map[string]ChildOutcome
	storeErr map[string]error
	hist     []EventRecord
	mod      func(s *execSession)
	resMax   uint32
	want     childAwaitWant
}

func runChildAwaitRow(t *testing.T, r childAwaitRow) {
	t.Helper()
	var mu sync.Mutex
	var gets []string
	eng := NewEngine(nil, &mockCaller{}, append([]EngineOption{WithWorkflowID("wf-child")}, r.opts...)...)
	if !r.noStore {
		eng.childWfStore = &mockChildStore{getChildResultFn: func(_ context.Context, id string) (ChildOutcome, error) {
			mu.Lock()
			gets = append(gets, id)
			mu.Unlock()
			if err, ok := r.storeErr[id]; ok {
				return ChildOutcome{}, err
			}
			return r.outcomes[id], nil
		}}
	}
	hist := append([]EventRecord(nil), r.hist...)
	for i := range hist {
		hist[i].Step = i
	}
	s := &execSession{
		engine: eng, workflowID: "wf-child", nowMs: 1_800_000_000_000,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: len(hist) > 0, history: hist,
	}
	if r.mod != nil {
		r.mod(s)
	}
	before := len(s.history)
	resMax := r.resMax
	if resMax == 0 {
		resMax = 400
	}
	buf := make([]byte, 600)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	idsJSON := r.idsJSON
	if idsJSON == "" {
		b, _ := json.Marshal(r.ids)
		idsJSON = string(b)
	}
	var got int64
	switch r.call {
	case callOfAwaitChild:
		got = s.AwaitChild(ctx, nil, r.ids[0], 0, resMax)
	case callOfAwaitAllChildren:
		got = s.AwaitAllChildren(ctx, nil, idsJSON, 0, resMax)
	case callOfAwaitAnyChild:
		got = s.AwaitAnyChild(ctx, nil, idsJSON, 0, resMax)
	}

	want := r.want
	// THE SUSPEND WORD IS NOT callSuspendSentinel. The three child awaits answer a suspension with packAwaitChildResultSuspend() (1<<62), and
	// every other call that suspends answers callSuspendSentinel (1<<31); the component callbacks compare against the former
	// (component_callbacks.go), and a guest SDK reads the word its own await's ABI names. So a spec that moves one of these onto the dispatcher
	// has to keep THIS word, and the rows assert it by value, not by the shared constant.
	suspendWord := packAwaitChildResultSuspend()
	if got == callSuspendSentinel {
		t.Errorf("the call answered the generic callSuspendSentinel %#x: the child awaits answer %#x", got, suspendWord)
	}
	if isSentinel := got == suspendWord; isSentinel != want.sentinel {
		t.Errorf("suspend word = %v, want %v (result %#x, the child awaits' suspend word is %#x)", isSentinel, want.sentinel, got, suspendWord)
	}
	if got != suspendWord {
		n := uint32(uint64(got) >> 32)
		if code := uint32(uint64(got) & 0xFFFFFFFF); code != want.errCode {
			t.Errorf("errCode = %d, want %d (result %#x)", code, want.errCode, got)
		}
		if n > uint32(len(buf)) {
			t.Fatalf("length %d is past the buffer (result %#x)", n, got)
		}
		if gotRes := string(buf[:n]); want.resultHas != "" {
			if !strings.Contains(gotRes, want.resultHas) {
				t.Errorf("result written = %q, want it to contain %q", gotRes, want.resultHas)
			}
		} else if gotRes != want.result {
			t.Errorf("result written = %q, want %q", gotRes, want.result)
		}
	}
	var recs []childAwaitRec
	for _, rec := range s.history[before:] {
		recs = append(recs, childAwaitRec{typ: rec.EventType, resp: rec.Response, errText: rec.Err, req: rec.Request, runID: rec.RunID})
	}
	if fmt.Sprint(recs) != fmt.Sprint(want.recs) {
		t.Errorf("recorded %v, want %v", recs, want.recs)
	}
	reason := ""
	if s.suspendErr != nil {
		reason = s.suspendErr.Reason
	}
	if reason != want.reason {
		t.Errorf("suspension reason = %q, want %q", reason, want.reason)
	}
	if s.stepCount != want.step {
		t.Errorf("stepCount = %d, want %d", s.stepCount, want.step)
	}
	if s.isReplay != want.replaying {
		t.Errorf("isReplay = %v, want %v", s.isReplay, want.replaying)
	}
	if len(gets) != want.gets {
		t.Errorf("store reads = %d %v, want %d", len(gets), gets, want.gets)
	}
	if want.getOrder != nil {
		if r.call == callOfAwaitAllChildren {
			sort.Strings(gets)
		}
		if fmt.Sprint(gets) != fmt.Sprint(want.getOrder) {
			t.Errorf("store read order = %v, want %v", gets, want.getOrder)
		}
	}
}

func TestTheChildAwaitsBehaveAsRecordedBeforeTheDispatcher(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	const (
		one  = `["c1"]`
		two  = `["c1","c2"]`
		twoU = `["c2","c1"]` // the unsorted request AwaitAnyChild sorts before polling and records as given
	)
	done := func(res string) ChildOutcome { return ChildOutcome{Completed: true, Result: res} }
	failed := func(msg string) ChildOutcome { return ChildOutcome{Completed: true, Failed: true, Error: msg} }
	pending := ChildOutcome{}
	ac := func(resp, errText string) EventRecord {
		return EventRecord{EventType: EventTypeAwaitChild, RunID: "c1", Response: resp, Err: errText}
	}
	allRec := func(req, resp string) EventRecord {
		return EventRecord{EventType: EventTypeAwaitAllChildren, Request: req, Response: resp}
	}
	anyRec := func(req, resp string) EventRecord {
		return EventRecord{EventType: EventTypeAwaitAnyChild, Request: req, Response: resp}
	}
	foreign := EventRecord{EventType: EventTypeSideEffect, Response: "x"}
	rec := func(typ EventType, resp, errText, req, runID string) childAwaitRec {
		return childAwaitRec{typ: typ, resp: resp, errText: errText, req: req, runID: runID}
	}
	// a session that is resuming at the END of its history: one foreign record, already consumed
	atEnd := func(s *execSession) { s.stepCount = 1 }

	const (
		both   = `[{"run_id":"c1","result":"R1"},{"run_id":"c2","result":"R2"}]`
		failC1 = `[{"run_id":"c1","error":"E"},{"run_id":"c2","result":"R2"}]`
	)
	longR := strings.Repeat("r", 50)

	rows := []childAwaitRow{
		// =============================== AwaitChild ===============================
		// ---- replay of an answered await ----
		{name: "await_child replay: a recorded result is delivered and consumed", call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{ac("R", "")},
			want: childAwaitWant{result: "R", step: 1, replaying: true}},
		{name: "await_child replay: a recorded failure is delivered as an error", call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{ac("", "E")},
			want: childAwaitWant{errCode: 1, result: "E", step: 1, replaying: true}},
		{name: "await_child replay: a result that does not fit reports the truncation", call: callOfAwaitChild, ids: []string{"c1"}, resMax: 10, hist: []EventRecord{ac(longR, "")},
			want: childAwaitWant{errCode: uint32(callErrorOutputTruncated), result: longR[:10], step: 1, replaying: true}},
		{name: "KNOWN DEFECT (no identity): await_child replay consumes a record that names ANOTHER child as this one's", call: callOfAwaitChild, ids: []string{"c1"},
			hist: []EventRecord{{EventType: EventTypeAwaitChild, RunID: "c2", Response: "R of c2"}},
			want: childAwaitWant{result: "R of c2", step: 1, replaying: true}},
		{name: "await_child replay: a record of another type is a divergence, and it consumes nothing", call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{foreign},
			want: childAwaitWant{errCode: 1, resultHas: "replay divergence at step 0: expected await_child, got side_effect", step: 0, replaying: true}},
		// ---- replay of a pending await: the record is NOT consumed, replay ends, and the answer is recorded at the same step ----
		{name: "await_child replay: an empty record and a completed child leave replay and record the result", call: callOfAwaitChild, ids: []string{"c1"},
			hist: []EventRecord{ac("", "")}, outcomes: map[string]ChildOutcome{"c1": done("R")},
			want: childAwaitWant{result: "R", recs: []childAwaitRec{rec(EventTypeAwaitChild, "R", "", "", "c1")}, step: 1, gets: 1}},
		{name: "await_child replay: an empty record and a failed child record the failure", call: callOfAwaitChild, ids: []string{"c1"},
			hist: []EventRecord{ac("", "")}, outcomes: map[string]ChildOutcome{"c1": failed("E")},
			want: childAwaitWant{errCode: 1, result: "E", recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "E", "", "c1")}, step: 1, gets: 1}},
		{name: "await_child replay: an empty record and a child still running record ANOTHER empty record and suspend", call: callOfAwaitChild, ids: []string{"c1"},
			hist: []EventRecord{ac("", "")}, outcomes: map[string]ChildOutcome{"c1": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "", "", "c1")}, reason: "await_child(c1)", step: 1, gets: 1}},
		{name: "await_child replay: an empty record and no child store record another empty record and suspend", call: callOfAwaitChild, ids: []string{"c1"}, noStore: true,
			hist: []EventRecord{ac("", "")},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "", "", "c1")}, reason: "await_child(c1)", step: 1}},
		{name: "await_child replay: an empty record and a store error record the error as the ANSWER", call: callOfAwaitChild, ids: []string{"c1"},
			hist: []EventRecord{ac("", "")}, storeErr: map[string]error{"c1": errors.New("boom")},
			want: childAwaitWant{errCode: 1, result: "boom", recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "boom", "", "c1")}, step: 1, gets: 1}},
		// ---- a pending record with MORE history after it: the shape the guards exempt "by reading, not by measurement" ----
		{name: "KNOWN DEFECT (history left unread): await_child replay of an empty record that is FOLLOWED by another record leaves replay with that record unread, so a call recorded after it would run again",
			call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{ac("", ""), foreign}, outcomes: map[string]ChildOutcome{"c1": done("R")},
			want: childAwaitWant{result: "R", recs: []childAwaitRec{rec(EventTypeAwaitChild, "R", "", "", "c1")}, step: 1, gets: 1}},
		// ---- the end of history ----
		{name: "await_child: the end of history leaves replay and a completed child is recorded", call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{foreign}, mod: atEnd,
			outcomes: map[string]ChildOutcome{"c1": done("R")},
			want:     childAwaitWant{result: "R", recs: []childAwaitRec{rec(EventTypeAwaitChild, "R", "", "", "c1")}, step: 2, gets: 1}},
		{name: "await_child: the end of history and a child still running record an empty await and suspend", call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{foreign}, mod: atEnd,
			outcomes: map[string]ChildOutcome{"c1": pending},
			want:     childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "", "", "c1")}, reason: "await_child(c1)", step: 2, gets: 1}},
		// ---- fresh ----
		{name: "await_child fresh: a completed child records and returns its result", call: callOfAwaitChild, ids: []string{"c1"}, outcomes: map[string]ChildOutcome{"c1": done("R")},
			want: childAwaitWant{result: "R", recs: []childAwaitRec{rec(EventTypeAwaitChild, "R", "", "", "c1")}, step: 1, gets: 1}},
		{name: "await_child fresh: a failed child records the failure", call: callOfAwaitChild, ids: []string{"c1"}, outcomes: map[string]ChildOutcome{"c1": failed("E")},
			want: childAwaitWant{errCode: 1, result: "E", recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "E", "", "c1")}, step: 1, gets: 1}},
		{name: "await_child fresh: Failed without Completed is still a failure (this call reads Failed alone)", call: callOfAwaitChild, ids: []string{"c1"},
			outcomes: map[string]ChildOutcome{"c1": {Failed: true, Error: "E"}},
			want:     childAwaitWant{errCode: 1, result: "E", recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "E", "", "c1")}, step: 1, gets: 1}},
		{name: "await_child fresh: a child still running records an empty await and suspends", call: callOfAwaitChild, ids: []string{"c1"}, outcomes: map[string]ChildOutcome{"c1": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "", "", "c1")}, reason: "await_child(c1)", step: 1, gets: 1}},
		{name: "await_child fresh: no child store records an empty await and suspends", call: callOfAwaitChild, ids: []string{"c1"}, noStore: true,
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "", "", "c1")}, reason: "await_child(c1)", step: 1}},
		{name: "await_child fresh: a store error is recorded as the answer", call: callOfAwaitChild, ids: []string{"c1"}, storeErr: map[string]error{"c1": errors.New("boom")},
			want: childAwaitWant{errCode: 1, result: "boom", recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "boom", "", "c1")}, step: 1, gets: 1}},
		{name: "await_child fresh: a result that does not fit reports the truncation and records the whole result", call: callOfAwaitChild, ids: []string{"c1"}, resMax: 10,
			outcomes: map[string]ChildOutcome{"c1": done(longR)},
			want:     childAwaitWant{errCode: uint32(callErrorOutputTruncated), result: longR[:10], recs: []childAwaitRec{rec(EventTypeAwaitChild, longR, "", "", "c1")}, step: 1, gets: 1}},
		// ---- NOT refused ----
		{name: "await_child fresh: a defer segment's body is NOT refused", call: callOfAwaitChild, ids: []string{"c1"}, opts: []EngineOption{WithDeferPhase()}, outcomes: map[string]ChildOutcome{"c1": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "", "", "c1")}, reason: "await_child(c1)", step: 1, gets: 1}},
		{name: "await_child fresh: a worker that is shutting down is NOT refused", call: callOfAwaitChild, ids: []string{"c1"}, opts: []EngineOption{WithShutdownSignal(closed)},
			outcomes: map[string]ChildOutcome{"c1": done("R")},
			want:     childAwaitWant{result: "R", recs: []childAwaitRec{rec(EventTypeAwaitChild, "R", "", "", "c1")}, step: 1, gets: 1}},
		{name: "KNOWN DEFECT (cleat#933 shape): await_child in a session that has already suspended records a second await and replaces the suspension", call: callOfAwaitChild, ids: []string{"c1"},
			mod: func(s *execSession) { s.suspendErr = &SuspendError{Reason: "earlier"} }, outcomes: map[string]ChildOutcome{"c1": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitChild, "", "", "", "c1")}, reason: "await_child(c1)", step: 1, gets: 1}},

		// =============================== AwaitAllChildren ===============================
		// ---- replay of an answered await ----
		{name: "await_all replay: a recorded answer for the same request is delivered and consumed", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, hist: []EventRecord{allRec(two, both)},
			want: childAwaitWant{result: both, step: 1, replaying: true}},
		{name: "await_all replay: a recorded answer that does not fit reports the truncation", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, resMax: 10, hist: []EventRecord{allRec(two, both)},
			want: childAwaitWant{errCode: uint32(callErrorOutputTruncated), result: both[:10], step: 1, replaying: true}},
		{name: "await_all replay: a request for other run ids is a divergence, and it CONSUMES the record (the dispatcher's consume none)", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"},
			hist: []EventRecord{allRec(`["c9"]`, both)},
			want: childAwaitWant{errCode: 1, resultHas: "await_all_children run IDs mismatch", step: 1, replaying: true}},
		{name: "await_all replay: a record of another type is a divergence, and it CONSUMES the record", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, hist: []EventRecord{foreign},
			want: childAwaitWant{errCode: 1, resultHas: "replay divergence at step 0: expected await_all_children, got side_effect", step: 1, replaying: true}},
		{name: "await_all replay: the same ids in another ORDER are a different request (compared as text)", call: callOfAwaitAllChildren, ids: []string{"c2", "c1"}, hist: []EventRecord{allRec(two, both)},
			want: childAwaitWant{errCode: 1, resultHas: "run IDs mismatch", step: 1, replaying: true}},
		// ---- replay of a pending await ----
		{name: "await_all replay: an empty record and every child complete leave replay and record the answer", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"},
			hist: []EventRecord{allRec(two, "")}, outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			want: childAwaitWant{result: both, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, both, "", two, "")}, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_all replay: an empty record and one child still running record ANOTHER empty record and suspend, naming the first running child", call: callOfAwaitAllChildren,
			ids: []string{"c1", "c2"}, hist: []EventRecord{allRec(two, "")}, outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, "", "", two, "")}, reason: "await_all_children(c2)", step: 1, gets: 2}},
		{name: "await_all replay: an empty record and no child store record another empty record and suspend", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, noStore: true,
			hist: []EventRecord{allRec(two, "")},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, "", "", two, "")}, reason: "await_all_children(c1)", step: 1}},
		{name: "KNOWN DEFECT (no identity on a pending record): await_all replay re-checks the store for THIS call's ids when the empty record names other ids", call: callOfAwaitAllChildren,
			ids: []string{"c1", "c2"}, hist: []EventRecord{allRec(`["c9"]`, "")}, outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			want: childAwaitWant{result: both, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, both, "", two, "")}, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "KNOWN DEFECT (history left unread): await_all replay of an empty record that is FOLLOWED by another record leaves replay with that record unread, so a call recorded after it would run again",
			call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, hist: []EventRecord{allRec(two, ""), foreign},
			outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			want:     childAwaitWant{result: both, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, both, "", two, "")}, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_all: the end of history leaves replay and records the answer", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, hist: []EventRecord{foreign}, mod: atEnd,
			outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			want:     childAwaitWant{result: both, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, both, "", two, "")}, step: 2, gets: 2, getOrder: []string{"c1", "c2"}}},
		// ---- fresh ----
		{name: "await_all fresh: every child complete records one answer with every outcome", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"},
			outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			want:     childAwaitWant{result: both, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, both, "", two, "")}, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_all fresh: a failed child is an outcome with an error, not a failure of the call", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"},
			outcomes: map[string]ChildOutcome{"c1": failed("E"), "c2": done("R2")},
			want:     childAwaitWant{result: failC1, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, failC1, "", two, "")}, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_all fresh: a store error on a child is recorded as that child's outcome", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"},
			outcomes: map[string]ChildOutcome{"c2": done("R2")}, storeErr: map[string]error{"c1": errors.New("E")},
			want: childAwaitWant{result: failC1, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, failC1, "", two, "")}, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_all fresh: one child still running records an empty await and suspends, naming it", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"},
			outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": pending},
			want:     childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, "", "", two, "")}, reason: "await_all_children(c2)", step: 1, gets: 2}},
		{name: "await_all fresh: no child store records an empty await and suspends", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, noStore: true,
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, "", "", two, "")}, reason: "await_all_children(c1)", step: 1}},
		{name: "await_all fresh: an empty list of children records the empty answer", call: callOfAwaitAllChildren, ids: []string{},
			want: childAwaitWant{result: `[]`, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, `[]`, "", `[]`, "")}, step: 1}},
		{name: "await_all fresh: a request that is not valid JSON answers an error and records nothing", call: callOfAwaitAllChildren, idsJSON: `not json`,
			want: childAwaitWant{errCode: 1, resultHas: "invalid runIDs"}},
		{name: "await_all fresh: an answer that does not fit reports the truncation and records the whole answer", call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, resMax: 10,
			outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			want:     childAwaitWant{errCode: uint32(callErrorOutputTruncated), result: both[:10], recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, both, "", two, "")}, step: 1, gets: 2}},
		// ---- NOT refused ----
		{name: "await_all fresh: a defer segment's body is NOT refused", call: callOfAwaitAllChildren, ids: []string{"c1"}, opts: []EngineOption{WithDeferPhase()}, outcomes: map[string]ChildOutcome{"c1": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, "", "", one, "")}, reason: "await_all_children(c1)", step: 1, gets: 1}},
		{name: "await_all fresh: a worker that is shutting down is NOT refused", call: callOfAwaitAllChildren, ids: []string{"c1"}, opts: []EngineOption{WithShutdownSignal(closed)},
			outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want:     childAwaitWant{result: `[{"run_id":"c1","result":"R1"}]`, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, `[{"run_id":"c1","result":"R1"}]`, "", one, "")}, step: 1, gets: 1}},
		{name: "KNOWN DEFECT (cleat#933 shape): await_all in a session that has already suspended records a second await and replaces the suspension", call: callOfAwaitAllChildren, ids: []string{"c1"},
			mod: func(s *execSession) { s.suspendErr = &SuspendError{Reason: "earlier"} }, outcomes: map[string]ChildOutcome{"c1": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAllChildren, "", "", one, "")}, reason: "await_all_children(c1)", step: 1, gets: 1}},

		// =============================== AwaitAnyChild ===============================
		// ---- replay of an answered await ----
		{name: "await_any replay: a recorded answer is delivered and consumed", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, hist: []EventRecord{anyRec(two, `{"run_id":"c1","result":"R1"}`)},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, step: 1, replaying: true}},
		{name: "KNOWN DEFECT (no identity): await_any replay consumes an answer recorded for ANOTHER set of children as this one's", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			hist: []EventRecord{anyRec(`["c9"]`, `{"run_id":"c9","result":"R9"}`)},
			want: childAwaitWant{result: `{"run_id":"c9","result":"R9"}`, step: 1, replaying: true}},
		{name: "await_any replay: a record of another type is a divergence, and it consumes nothing", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, hist: []EventRecord{foreign},
			want: childAwaitWant{errCode: 1, resultHas: "replay divergence at step 0: expected await_any_child, got side_effect", step: 0, replaying: true}},
		// ---- replay of a pending await: the empty record is consumed, then every following empty of THIS call (cleat#3391) ----
		{name: "await_any replay: an empty record followed by its answer delivers the answer", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			hist: []EventRecord{anyRec(two, ""), anyRec(two, `{"run_id":"c1","result":"R1"}`)},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, step: 2, replaying: true}},
		{name: "await_any replay (cleat#3391): two empty records and then the answer deliver the answer", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			hist: []EventRecord{anyRec(two, ""), anyRec(two, ""), anyRec(two, `{"run_id":"c1","result":"R1"}`)},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, step: 3, replaying: true}},
		{name: "await_any replay: the empties are compared as a SET, in any order", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			hist: []EventRecord{anyRec(two, ""), anyRec(twoU, `{"run_id":"c1","result":"R1"}`)},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, step: 2, replaying: true}},
		{name: "await_any replay: an empty record followed by ANOTHER call's answer does not deliver it: it leaves replay with that record unread and re-checks", call: callOfAwaitAnyChild,
			ids: []string{"c1", "c2"}, hist: []EventRecord{anyRec(two, ""), anyRec(`["c9"]`, `{"run_id":"c9","result":"R9"}`)}, outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c1","result":"R1"}`, "", two, "")}, step: 2, gets: 1, getOrder: []string{"c1"}}},
		{name: "await_any replay: an empty record followed by a record of another type leaves replay with that record unread and re-checks", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			hist: []EventRecord{anyRec(two, ""), foreign}, outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c1","result":"R1"}`, "", two, "")}, step: 2, gets: 1, getOrder: []string{"c1"}}},
		{name: "await_any replay: an empty record and a completed child record the answer at the next step", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			hist: []EventRecord{anyRec(two, "")}, outcomes: map[string]ChildOutcome{"c2": done("R2")},
			want: childAwaitWant{result: `{"run_id":"c2","result":"R2"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c2","result":"R2"}`, "", two, "")}, step: 2, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_any replay (cleat#3391): an empty record and every child still running suspend and record NOTHING more", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			hist: []EventRecord{anyRec(two, "")}, outcomes: map[string]ChildOutcome{"c1": pending, "c2": pending},
			want: childAwaitWant{sentinel: true, reason: `await_any_child(["c1","c2"])`, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_any replay: an empty record and no child store suspend and record nothing more", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, noStore: true, hist: []EventRecord{anyRec(two, "")},
			want: childAwaitWant{sentinel: true, reason: `await_any_child(["c1","c2"])`, step: 1}},
		// ---- the end of history ----
		{name: "await_any: the end of history leaves replay and a completed child is recorded", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, hist: []EventRecord{foreign}, mod: atEnd,
			outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want:     childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c1","result":"R1"}`, "", two, "")}, step: 2, gets: 1, getOrder: []string{"c1"}}},
		{name: "await_any: the end of history and no child done record an empty await and suspend", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, hist: []EventRecord{foreign}, mod: atEnd,
			outcomes: map[string]ChildOutcome{"c1": pending, "c2": pending},
			want:     childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, "", "", two, "")}, reason: `await_any_child(["c1","c2"])`, step: 2, gets: 2, getOrder: []string{"c1", "c2"}}},
		// ---- fresh ----
		{name: "await_any fresh: children are polled in SORTED order and the first one done is the answer, recorded with the request as given", call: callOfAwaitAnyChild, ids: []string{"c2", "c1"},
			outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			want:     childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c1","result":"R1"}`, "", twoU, "")}, step: 1, gets: 1, getOrder: []string{"c1"}}},
		{name: "await_any fresh: a failed child is the answer, with its error", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			outcomes: map[string]ChildOutcome{"c1": pending, "c2": failed("E")},
			want:     childAwaitWant{result: `{"run_id":"c2","error":"E"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c2","error":"E"}`, "", two, "")}, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_any fresh: Failed without Completed is not done (this call polls Completed first)", call: callOfAwaitAnyChild, ids: []string{"c1"},
			outcomes: map[string]ChildOutcome{"c1": {Failed: true, Error: "E"}},
			want:     childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, "", "", one, "")}, reason: `await_any_child(["c1"])`, step: 1, gets: 1, getOrder: []string{"c1"}}},
		{name: "await_any fresh: a store error on a child is recorded as the answer, with the error text", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			storeErr: map[string]error{"c1": errors.New("boom")},
			want:     childAwaitWant{result: `{"run_id":"c1","error":"boom"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c1","error":"boom"}`, "", two, "")}, step: 1, gets: 1, getOrder: []string{"c1"}}},
		{name: "await_any fresh: no child done records an empty await and suspends", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, outcomes: map[string]ChildOutcome{"c1": pending, "c2": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, "", "", two, "")}, reason: `await_any_child(["c1","c2"])`, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{name: "await_any fresh: no child store records an empty await and suspends", call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, noStore: true,
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, "", "", two, "")}, reason: `await_any_child(["c1","c2"])`, step: 1}},
		{name: "await_any fresh: a request that is not valid JSON answers an error and records nothing", call: callOfAwaitAnyChild, idsJSON: `not json`,
			want: childAwaitWant{errCode: 1, resultHas: "invalid runIDs"}},
		{name: "await_any fresh: an answer that does not fit reports the truncation and records the whole answer", call: callOfAwaitAnyChild, ids: []string{"c1"}, resMax: 10,
			outcomes: map[string]ChildOutcome{"c1": done(longR)},
			want:     childAwaitWant{errCode: uint32(callErrorOutputTruncated), result: `{"run_id":"c1","result":"`[:10], recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c1","result":"`+longR+`"}`, "", one, "")}, step: 1, gets: 1, getOrder: []string{"c1"}}},
		// ---- the one refusal-shaped guard this call has, and what it does not cover ----
		{name: "await_any fresh: a session that has already suspended does not start another await: it answers suspended, records nothing and keeps its reason", call: callOfAwaitAnyChild,
			ids: []string{"c1"}, mod: func(s *execSession) { s.suspendErr = &SuspendError{Reason: "earlier"} }, outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want: childAwaitWant{sentinel: true, reason: "earlier"}},
		{name: "await_any fresh: a defer drain's await goes through even in a session that has already suspended", call: callOfAwaitAnyChild, ids: []string{"c1"},
			mod: func(s *execSession) {
				s.suspendErr = &SuspendError{Reason: "earlier"}
				s.inDeferDrain = true
			}, outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c1","result":"R1"}`, "", one, "")}, reason: "earlier", step: 1, gets: 1, getOrder: []string{"c1"}}},
		{name: "await_any fresh: a defer segment's body is NOT refused", call: callOfAwaitAnyChild, ids: []string{"c1"}, opts: []EngineOption{WithDeferPhase()}, outcomes: map[string]ChildOutcome{"c1": pending},
			want: childAwaitWant{sentinel: true, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, "", "", one, "")}, reason: `await_any_child(["c1"])`, step: 1, gets: 1, getOrder: []string{"c1"}}},
		{name: "await_any fresh: a worker that is shutting down is NOT refused", call: callOfAwaitAnyChild, ids: []string{"c1"}, opts: []EngineOption{WithShutdownSignal(closed)},
			outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want:     childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{rec(EventTypeAwaitAnyChild, `{"run_id":"c1","result":"R1"}`, "", one, "")}, step: 1, gets: 1, getOrder: []string{"c1"}}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { runChildAwaitRow(t, r) })
	}
}
