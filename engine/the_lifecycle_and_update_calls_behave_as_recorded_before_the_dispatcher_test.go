package engine

// W1 of cleat#4152: continue_as_new, continue_as_new_versioned, register_update_handler, poll_update and complete_update moved onto the step dispatcher. This table was
// written against the OLD code (develop 3f78dbb7, before the move) and every row passes on it and on the moved code alike, which is what makes it a statement about the
// move and not about the new code: a row that only the moved code passed would be the new code agreeing with itself. It was run on both.
//
// It pins what the move must NOT change, including the things that look like accidents, because each was a decision the old code made:
//
//   - continue_as_new and register_update_handler compare NOTHING on replay: a record of the right type is consumed whatever input or name it carries, and the run
//     continues with the RECORDED input. A tightening would fail a workflow in flight across a deploy that renamed a handler or built its input differently, so it is a
//     separate ruling and the rows say "no identity".
//   - register_update_handler is NEVER refused in a stopping state (the old code had no stopBeforeNewWork), where continue_as_new, poll_update and complete_update are.
//   - poll_update with no update store answers 0 BEFORE any refusal, so a poll with nothing to poll is not newly refused in a stopping or a suspended session.
//   - poll_update answers "nothing found" for another call's record WITHOUT consuming it, without reading the store and without leaving replay.
//   - complete_update on a replay does not settle again (#818), and settles once on a fresh run, after recording.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type lifecycleCall func(s *execSession, ctx context.Context) int64

type lifecycleWant struct {
	word      int64  // compared exactly unless skipWord
	skipWord  bool   // poll_update packs a length into the word: its delivery is compared instead
	delivered string // "name|payload|requestID" of the update a poll handed the guest; "" for none
	recs      []string
	step      int
	replaying bool
	susp      string // "reason|input|version" of the suspension the call left on the session, "" for none
	completed []string
	resolved  []string
	rejected  []string
	reads     int // GetPendingUpdateRequests calls
}

type lifecycleRow struct {
	name    string
	call    lifecycleCall
	hist    []EventRecord
	noStore bool
	pend    []UpdateRequestInfo
	mod     func(s *execSession)
	want    lifecycleWant
}

type countingUpdateStore struct {
	fakeUpdateStore
	reads int
}

func (c *countingUpdateStore) GetPendingUpdateRequests(ctx context.Context, id string) ([]UpdateRequestInfo, error) {
	c.reads++
	return c.fakeUpdateStore.GetPendingUpdateRequests(ctx, id)
}

func lifecycleRecSummary(r EventRecord) string {
	return fmt.Sprintf("%s step=%d new_input=%q version=%d handler=%q request=%q payload=%q response=%q err=%q",
		r.EventType, r.Step, r.NewInput, r.NewVersion, r.UpdateHandlerName, r.UpdateRequestID, r.UpdatePayload, r.UpdateResponse, r.UpdateError)
}

func runLifecycleRow(t *testing.T, r lifecycleRow) {
	t.Helper()
	store := &countingUpdateStore{fakeUpdateStore: fakeUpdateStore{pending: append([]UpdateRequestInfo(nil), r.pend...)}}
	opts := []EngineOption{WithWorkflowID("wf-1")}
	if !r.noStore {
		opts = append(opts, WithUpdateStore(store))
	}
	s := &execSession{
		engine:   NewEngine(nil, &mockCaller{}, opts...),
		history:  append([]EventRecord(nil), r.hist...),
		isReplay: len(r.hist) > 0,
	}
	if r.mod != nil {
		r.mod(s)
	}
	buf := make([]byte, 1024)
	ctx := ctxWithMem(context.Background(), buf)
	word := r.call(s, ctx)

	var gotDelivered string
	if r.want.skipWord {
		// The length in the high 32 bits is the delivery's and is checked through the delivery. The LOW 32 bits are compared exactly: 0 when nothing was delivered and the
		// found flag when something was. A first version compared only the delivery, and a divergence word (1) and the stop word (bit 31) both decode as "nothing
		// delivered", so a poll that answered either looked exactly like one that correctly answered 0 (three mutants survived it).
		wantLow := uint32(0)
		if r.want.delivered != "" {
			wantLow = updateFoundFlag
		}
		if low := uint32(word); low != wantLow {
			t.Errorf("poll answered low word %#x, want %#x (0 = nothing found, %#x = a delivery)", low, wantLow, updateFoundFlag)
		}
		if d, found := decodeDelivery(t, ctx, buf, word); found {
			gotDelivered = d.Name + "|" + d.Payload + "|" + d.RequestID
		}
	} else if word != r.want.word {
		t.Errorf("word = %#x, want %#x", word, r.want.word)
	}
	if gotDelivered != r.want.delivered {
		t.Errorf("delivered %q, want %q", gotDelivered, r.want.delivered)
	}
	var got []string
	for _, rec := range s.history[len(r.hist):] {
		got = append(got, lifecycleRecSummary(rec))
	}
	if fmt.Sprint(got) != fmt.Sprint(r.want.recs) {
		t.Errorf("recorded %v, want %v", got, r.want.recs)
	}
	if s.stepCount != r.want.step {
		t.Errorf("stepCount = %d, want %d", s.stepCount, r.want.step)
	}
	if s.isReplay != r.want.replaying {
		t.Errorf("isReplay = %v, want %v", s.isReplay, r.want.replaying)
	}
	susp := ""
	if s.suspendErr != nil {
		susp = fmt.Sprintf("%s|%s|%d", s.suspendErr.Reason, s.suspendErr.NewInput, s.suspendErr.NewVersion)
	}
	if susp != r.want.susp {
		t.Errorf("suspension = %q, want %q", susp, r.want.susp)
	}
	for _, c := range []struct {
		label     string
		got, want []string
	}{{"completed", store.completed, r.want.completed}, {"resolved", store.resolved, r.want.resolved}, {"rejected", store.rejected, r.want.rejected}} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("store %s = %v, want %v", c.label, c.got, c.want)
		}
	}
	if store.reads != r.want.reads {
		t.Errorf("GetPendingUpdateRequests called %d time(s), want %d", store.reads, r.want.reads)
	}
	if len(s.stepViolations) != 0 {
		t.Errorf("the dispatcher reported spec violations: %v", s.stepViolations)
	}
}

func TestTheLifecycleAndUpdateCallsBehaveAsRecordedBeforeTheDispatcher(t *testing.T) {
	var (
		divergence = packSimpleResult(1) // the divergence word of every layoutSimple call: error code 1, no length
		stopWord   = callSuspendSentinel
		stopping   = func(s *execSession) { s.eventWriteFailure = errors.New("a recorded event could not be written") }
		suspended  = func(s *execSession) { s.suspendErr = &SuspendError{Reason: "earlier"} }
		atTheEnd   = func(s *execSession) { s.stepCount = len(s.history) }
		can        = func(in string) lifecycleCall {
			return func(s *execSession, ctx context.Context) int64 { return s.ContinueAsNew(ctx, nil, in) }
		}
		canVersioned = func(in string, v int) lifecycleCall {
			return func(s *execSession, ctx context.Context) int64 { return s.ContinueAsNewWithVersion(ctx, nil, in, v) }
		}
		register = func(name string) lifecycleCall {
			return func(s *execSession, ctx context.Context) int64 { return s.RegisterUpdateHandler(ctx, nil, name) }
		}
		poll     lifecycleCall = func(s *execSession, ctx context.Context) int64 { return s.DurablePollUpdate(ctx, nil, 0, 1024) }
		complete               = func(key, result, errMsg string) lifecycleCall {
			return func(s *execSession, ctx context.Context) int64 {
				return s.DurableCompleteUpdate(ctx, nil, key, result, errMsg)
			}
		}
		recorded = EventRecord{Step: 0, EventType: EventTypeContinueAsNew, NewInput: "recorded", NewVersion: 3}
		aCall    = EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op"}
		upd      = []UpdateRequestInfo{{UpdateName: "add", Payload: `{"n":5}`, PromiseID: "prom-1"}}
		key      = updateRequestKey(upd[0])
	)
	rows := []lifecycleRow{
		// ---- continue_as_new ----
		{name: "continue_as_new replay: the recorded record is consumed and the run continues with the RECORDED input, not the guest's (no identity)", call: can("the guest's input"), hist: []EventRecord{recorded},
			want: lifecycleWant{word: 0, step: 1, replaying: true, susp: "continue_as_new|recorded|0"}},
		{name: "continue_as_new replay: a record of another type is a divergence that consumes nothing", call: can("x"), hist: []EventRecord{aCall},
			want: lifecycleWant{word: divergence, step: 0, replaying: true}},
		{name: "continue_as_new fresh: records the guest's input and suspends with it", call: can("next"),
			want: lifecycleWant{word: 0, recs: []string{`continue_as_new step=0 new_input="next" version=0 handler="" request="" payload="" response="" err=""`}, step: 1, susp: "continue_as_new|next|0"}},
		{name: "continue_as_new fresh at the end of history: leaves replay, records, suspends", call: can("next"), hist: []EventRecord{aCall}, mod: atTheEnd,
			want: lifecycleWant{word: 0, recs: []string{`continue_as_new step=1 new_input="next" version=0 handler="" request="" payload="" response="" err=""`}, step: 2, susp: "continue_as_new|next|0"}},
		{name: "continue_as_new fresh in a stopping state: refused with the stop word, records nothing, suspends nothing", call: can("next"), mod: stopping,
			want: lifecycleWant{word: stopWord}},
		{name: "continue_as_new fresh after a suspension: refused with the stop word, the first suspension stays", call: can("next"), mod: suspended,
			want: lifecycleWant{word: stopWord, susp: "earlier||0"}},
		// ---- continue_as_new_versioned ----
		{name: "continue_as_new_versioned replay: continues with the RECORDED input and version, not the guest's", call: canVersioned("the guest's", 9), hist: []EventRecord{recorded},
			want: lifecycleWant{word: 0, step: 1, replaying: true, susp: "continue_as_new|recorded|3"}},
		{name: "continue_as_new_versioned replay: a record of another type is a divergence", call: canVersioned("x", 1), hist: []EventRecord{aCall},
			want: lifecycleWant{word: divergence, step: 0, replaying: true}},
		{name: "continue_as_new_versioned fresh: records the input and the version and suspends with both", call: canVersioned("next", 5),
			want: lifecycleWant{word: 0, recs: []string{`continue_as_new step=0 new_input="next" version=5 handler="" request="" payload="" response="" err=""`}, step: 1, susp: "continue_as_new|next|5"}},
		{name: "continue_as_new_versioned fresh in a stopping state: refused with the stop word", call: canVersioned("next", 5), mod: stopping, want: lifecycleWant{word: stopWord}},
		{name: "continue_as_new_versioned fresh after a suspension: refused with the stop word", call: canVersioned("next", 5), mod: suspended, want: lifecycleWant{word: stopWord, susp: "earlier||0"}},
		// ---- register_update_handler ----
		{name: "register_update_handler replay: any update_handler record is consumed whatever name it carries (no identity)", call: register("add"),
			hist: []EventRecord{{Step: 0, EventType: EventTypeUpdateHandler, UpdateHandlerName: "a different name"}}, want: lifecycleWant{word: 0, step: 1, replaying: true}},
		{name: "register_update_handler replay: a record of another type is a divergence", call: register("add"), hist: []EventRecord{aCall}, want: lifecycleWant{word: divergence, step: 0, replaying: true}},
		{name: "register_update_handler fresh: records the handler's name", call: register("add"),
			want: lifecycleWant{word: 0, recs: []string{`update_handler step=0 new_input="" version=0 handler="add" request="" payload="" response="" err=""`}, step: 1}},
		{name: "register_update_handler fresh in a STOPPING state is NOT refused: it registers (the old code had no stopBeforeNewWork)", call: register("add"), mod: stopping,
			want: lifecycleWant{word: 0, recs: []string{`update_handler step=0 new_input="" version=0 handler="add" request="" payload="" response="" err=""`}, step: 1}},
		{name: "register_update_handler fresh after a suspension: the ordinary error code, nothing recorded (it is not a stop site)", call: register("add"), mod: suspended,
			want: lifecycleWant{word: 1, susp: "earlier||0"}},
		// ---- poll_update ----
		{name: "poll_update replay: the recorded delivery is handed back and the store is never read", call: poll,
			hist: []EventRecord{{Step: 0, EventType: EventTypeUpdateReceived, UpdateHandlerName: "add", UpdatePayload: `{"n":5}`, UpdateRequestID: "k1"}},
			want: lifecycleWant{skipWord: true, delivered: `add|{"n":5}|k1`, step: 1, replaying: true}},
		{name: "poll_update replay: ANOTHER call's record means nothing was delivered here: answers nothing found, consumes nothing, reads no store, stays in replay", call: poll, hist: []EventRecord{aCall},
			pend: upd, want: lifecycleWant{skipWord: true, step: 0, replaying: true}},
		{name: "poll_update fresh with a pending request: records the delivery before anything else and hands it to the guest", call: poll, pend: upd,
			want: lifecycleWant{skipWord: true, delivered: `add|{"n":5}|` + key,
				recs: []string{fmt.Sprintf(`update_received step=0 new_input="" version=0 handler="add" request=%q payload="{\"n\":5}" response="" err=""`, key)}, step: 1, reads: 1}},
		{name: "poll_update fresh with nothing pending: answers nothing found and RECORDS NOTHING", call: poll, want: lifecycleWant{skipWord: true, reads: 1}},
		{name: "poll_update at the end of history: leaves replay and polls the store", call: poll, hist: []EventRecord{aCall}, mod: atTheEnd, pend: upd,
			want: lifecycleWant{skipWord: true, delivered: `add|{"n":5}|` + key,
				recs: []string{fmt.Sprintf(`update_received step=1 new_input="" version=0 handler="add" request=%q payload="{\"n\":5}" response="" err=""`, key)}, step: 2, reads: 1}},
		{name: "poll_update with NO update store answers 0 and records nothing", call: poll, noStore: true, want: lifecycleWant{skipWord: true}},
		{name: "poll_update with NO update store in a STOPPING state still answers 0, not the stop word (the old code asked for the store first)", call: poll, noStore: true, mod: stopping,
			want: lifecycleWant{skipWord: true}},
		{name: "poll_update with NO update store after a suspension still answers 0, not the stop word", call: poll, noStore: true, mod: suspended, want: lifecycleWant{skipWord: true, susp: "earlier||0"}},
		{name: "poll_update fresh in a stopping state with a store: refused with the stop word, the store is not read", call: poll, pend: upd, mod: stopping, want: lifecycleWant{word: stopWord}},
		{name: "poll_update fresh after a suspension with a store: refused with the stop word, the store is not read", call: poll, pend: upd, mod: suspended, want: lifecycleWant{word: stopWord, susp: "earlier||0"}},
		// ---- complete_update ----
		{name: "complete_update replay: the recorded outcome is consumed and NOTHING is settled again (#818)", call: complete(key, "r", ""),
			hist: []EventRecord{{Step: 0, EventType: EventTypeUpdateCompleted, UpdateHandlerName: "add", UpdateRequestID: key}}, want: lifecycleWant{word: 0, step: 1, replaying: true}},
		{name: "complete_update replay: a record of another type is a divergence", call: complete(key, "r", ""), hist: []EventRecord{aCall}, want: lifecycleWant{word: divergence, step: 0, replaying: true}},
		{name: "complete_update fresh: records the outcome, completes the row and resolves the caller's promise", call: complete(key, "ok", ""),
			want: lifecycleWant{word: 0, recs: []string{fmt.Sprintf(`update_completed step=0 new_input="" version=0 handler="add" request=%q payload="" response="ok" err=""`, key)}, step: 1,
				completed: []string{"add|ok|"}, resolved: []string{"prom-1|ok"}}},
		{name: "complete_update fresh with an error: the caller's promise is REJECTED and not resolved", call: complete(key, "", "boom"),
			want: lifecycleWant{word: 0, recs: []string{fmt.Sprintf(`update_completed step=0 new_input="" version=0 handler="add" request=%q payload="" response="" err="boom"`, key)}, step: 1,
				completed: []string{"add||boom"}, rejected: []string{"prom-1|boom"}}},
		{name: "complete_update fresh for a request with no promise: the row is completed and nothing is settled", call: complete(updateRequestKey(UpdateRequestInfo{UpdateName: "add"}), "ok", ""),
			want: lifecycleWant{word: 0, recs: []string{fmt.Sprintf(`update_completed step=0 new_input="" version=0 handler="add" request=%q payload="" response="ok" err=""`, updateRequestKey(UpdateRequestInfo{UpdateName: "add"}))}, step: 1,
				completed: []string{"add|ok|"}}},
		{name: "complete_update fresh with NO update store: records the outcome and answers success", call: complete(key, "ok", ""), noStore: true,
			want: lifecycleWant{word: 0, recs: []string{fmt.Sprintf(`update_completed step=0 new_input="" version=0 handler="add" request=%q payload="" response="ok" err=""`, key)}, step: 1}},
		{name: "complete_update fresh in a stopping state: refused with the stop word, nothing recorded or settled", call: complete(key, "ok", ""), mod: stopping, want: lifecycleWant{word: stopWord}},
		{name: "complete_update fresh after a suspension: refused with the stop word, nothing recorded or settled", call: complete(key, "ok", ""), mod: suspended, want: lifecycleWant{word: stopWord, susp: "earlier||0"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { runLifecycleRow(t, r) })
	}
}
