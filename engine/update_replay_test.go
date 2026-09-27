package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeUpdateStore is an UpdateStore whose queue and settlements a test can
// inspect. It is not a mock of a database; it is the smallest thing that lets
// the replay behaviour below be asserted without one.
type fakeUpdateStore struct {
	pending   []UpdateRequestInfo
	completed []string // "name|result|err"
	resolved  []string // "promiseID|value"
	rejected  []string // "promiseID|err"
}

func (f *fakeUpdateStore) GetPendingUpdateRequests(_ context.Context, _ string) ([]UpdateRequestInfo, error) {
	return f.pending, nil
}

func (f *fakeUpdateStore) CompleteUpdateRequest(_ context.Context, _, updateName, result, errMsg string) error {
	f.completed = append(f.completed, updateName+"|"+result+"|"+errMsg)
	for i, p := range f.pending {
		if p.UpdateName == updateName {
			f.pending = append(f.pending[:i], f.pending[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeUpdateStore) ResolvePromise(_ context.Context, promiseID, value string) error {
	f.resolved = append(f.resolved, promiseID+"|"+value)
	return nil
}

func (f *fakeUpdateStore) RejectPromise(_ context.Context, promiseID, errMsg string) error {
	f.rejected = append(f.rejected, promiseID+"|"+errMsg)
	return nil
}

func newUpdateSession(t *testing.T, store UpdateStore, history []EventRecord) *execSession {
	t.Helper()
	e := NewEngine(nil, &mockCaller{}, WithUpdateStore(store), WithWorkflowID("wf-1"))
	return &execSession{
		engine:   e,
		history:  history,
		isReplay: len(history) > 0,
	}
}

// decodeDelivery reads back what DurablePollUpdate wrote. writeResult needs a
// memory buffer in the context; ctxWithMem supplies one, and the envelope is
// the leading bytes of it.
func decodeDelivery(t *testing.T, ctx context.Context, buf []byte, packed int64) (updateDelivery, bool) {
	t.Helper()
	found := uint32(packed)&updateFoundFlag != 0
	if !found {
		return updateDelivery{}, false
	}
	n := uint32(uint64(packed) >> 32)
	var d updateDelivery
	if err := json.Unmarshal(buf[:n], &d); err != nil {
		t.Fatalf("decoding the delivery envelope %q: %v", string(buf[:n]), err)
	}
	return d, true
}

// TestAFreshPollRecordsTheDeliveryBeforeAnythingElseCanHappen is the property
// the whole design rests on: the delivery is written to the history at the step
// the guest asked for it, BEFORE the handler runs and before anything settles.
//
// Recording after would lose the update entirely on a crash between the two,
// and the caller would wait forever on a promise nothing settles.
func TestAFreshPollRecordsTheDeliveryBeforeAnythingElseCanHappen(t *testing.T) {
	store := &fakeUpdateStore{pending: []UpdateRequestInfo{
		{UpdateName: "add", Payload: `{"n":5}`, PromiseID: "prom-1"},
	}}
	s := newUpdateSession(t, store, nil)

	buf := make([]byte, 1024)
	ctx := ctxWithMem(context.Background(), buf)
	d, found := decodeDelivery(t, ctx, buf, s.DurablePollUpdate(ctx, nil, 0, uint32(len(buf))))

	if !found {
		t.Fatal("a pending update was not delivered")
	}
	if d.Name != "add" || d.Payload != `{"n":5}` {
		t.Errorf("delivered %+v, want name=add payload={\"n\":5}", d)
	}
	if len(s.history) != 1 {
		t.Fatalf("recorded %d events, want 1", len(s.history))
	}
	rec := s.history[0]
	if rec.EventType != EventTypeUpdateReceived {
		t.Errorf("recorded %q, want %q", rec.EventType, EventTypeUpdateReceived)
	}
	if rec.UpdatePayload != `{"n":5}` || rec.UpdateHandlerName != "add" {
		t.Errorf("the recorded event does not carry the handler's input: %+v", rec)
	}
	if len(store.completed) != 0 || len(store.resolved) != 0 {
		t.Error("the poll settled something. Delivery must record and return; the handler has " +
			"not run yet, so there is no outcome to report.")
	}
}

// TestReplayDeliversFromHistoryAndNeverConsultsTheStore is the other half, and
// it is what makes the interleaving correct.
//
// A request that arrived AFTER the original run must not be delivered at an
// earlier step on replay -- that would insert an event into the middle of a
// recorded history, which recordEvent cannot even express (it appends), and
// would hand the handler an input the original run never saw.
func TestReplayDeliversFromHistoryAndNeverConsultsTheStore(t *testing.T) {
	// The store holds something DIFFERENT from what history records. If replay
	// consulted it, the assertions below would see "arrived-later".
	store := &fakeUpdateStore{pending: []UpdateRequestInfo{
		{UpdateName: "arrived-later", Payload: `{"n":99}`, PromiseID: "prom-2"},
	}}
	history := []EventRecord{{
		Step:              0,
		EventType:         EventTypeUpdateReceived,
		UpdateHandlerName: "add",
		UpdatePayload:     `{"n":5}`,
		UpdateRequestID:   "add\x00prom-1",
	}}
	s := newUpdateSession(t, store, history)

	buf := make([]byte, 1024)
	ctx := ctxWithMem(context.Background(), buf)
	d, found := decodeDelivery(t, ctx, buf, s.DurablePollUpdate(ctx, nil, 0, uint32(len(buf))))

	if !found {
		t.Fatal("replay did not deliver the recorded update")
	}
	if d.Name != "add" || d.Payload != `{"n":5}` {
		t.Errorf("replay delivered %+v -- it must come from history, not from the store. "+
			"The store holds a DIFFERENT request, so seeing it here means a request that "+
			"arrived after the original run was delivered at an earlier step.", d)
	}
	if len(s.history) != 1 {
		t.Errorf("replay appended to history (%d events, want 1); a replayed delivery must "+
			"consume the recorded event, not record a new one", len(s.history))
	}
}

// TestAPollThatFindsNoUpdateDoesNotEndReplay. The SDK polls before every
// suspension, so most polls legitimately find nothing. Treating a
// non-update_received event at the current step as a history mismatch would end
// replay at the first such poll and re-execute the rest of the workflow as
// fresh work -- duplicating every call after it.
func TestAPollThatFindsNoUpdateDoesNotEndReplay(t *testing.T) {
	store := &fakeUpdateStore{}
	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op"},
	}
	s := newUpdateSession(t, store, history)

	buf := make([]byte, 1024)
	ctx := ctxWithMem(context.Background(), buf)
	if _, found := decodeDelivery(t, ctx, buf, s.DurablePollUpdate(ctx, nil, 0, uint32(len(buf)))); found {
		t.Fatal("delivered an update that history does not record at this step")
	}
	if !s.isReplay {
		t.Error("a poll that found nothing ended replay. Every suspension polls, so most polls " +
			"find nothing; ending replay there would re-execute the rest of the workflow as " +
			"fresh work and duplicate every call after it.")
	}
	if s.stepCount != 0 {
		t.Errorf("a poll that found nothing advanced the step counter to %d, "+
			"desynchronising it from the replay position", s.stepCount)
	}
}

// TestCompletionSettlesOnceAndReplayDoesNotSettleAgain.
//
// Without an update_completed event every replay would settle the caller's
// promise again, and a settle matching no promise reports not-found (#818) --
// so replay would start erroring on a workflow that had done nothing wrong.
func TestCompletionSettlesOnceAndReplayDoesNotSettleAgain(t *testing.T) {
	store := &fakeUpdateStore{pending: []UpdateRequestInfo{
		{UpdateName: "add", Payload: "{}", PromiseID: "prom-1"},
	}}
	s := newUpdateSession(t, store, nil)
	ctx := context.Background()

	if r := s.DurableCompleteUpdate(ctx, nil, "add\x00prom-1", `{"ok":true}`, ""); r != 0 {
		t.Fatalf("DurableCompleteUpdate returned %d, want 0", r)
	}
	if len(store.resolved) != 1 || store.resolved[0] != `prom-1|{"ok":true}` {
		t.Fatalf("resolved = %v, want one prom-1 resolution", store.resolved)
	}
	if len(store.completed) != 1 {
		t.Fatalf("completed = %v, want one", store.completed)
	}

	// Now replay the same step.
	replayed := newUpdateSession(t, store, s.history)
	if r := replayed.DurableCompleteUpdate(ctx, nil, "add\x00prom-1", `{"ok":true}`, ""); r != 0 {
		t.Fatalf("replayed DurableCompleteUpdate returned %d, want 0", r)
	}
	if len(store.resolved) != 1 {
		t.Errorf("replay settled the promise again: resolved = %v.\n\n"+
			"The promise was settled on the original run. Settling again reports not-found "+
			"(#818), so every replay of a completed update would error.", store.resolved)
	}
}

// TestAFailedUpdateRejectsRatherThanResolving: errMsg being non-empty is the
// whole of the distinction, so a handler returning an empty result and no error
// must still resolve -- an empty result is an outcome, not a missing one.
func TestAFailedUpdateRejectsRatherThanResolving(t *testing.T) {
	store := &fakeUpdateStore{}
	s := newUpdateSession(t, store, nil)
	ctx := context.Background()

	s.DurableCompleteUpdate(ctx, nil, "approve\x00prom-1", "", "amount must be positive")
	if len(store.rejected) != 1 || len(store.resolved) != 0 {
		t.Errorf("a handler error must reject: rejected=%v resolved=%v", store.rejected, store.resolved)
	}

	s2 := newUpdateSession(t, store, nil)
	s2.DurableCompleteUpdate(ctx, nil, "ping\x00prom-2", "", "")
	if len(store.resolved) != 1 {
		t.Errorf("an empty result with no error must RESOLVE, not reject: resolved=%v rejected=%v.\n"+
			"An empty result is an outcome; treating it as a failure would make every "+
			"handler that returns nothing look like an error to its caller.",
			store.resolved, store.rejected)
	}
}

// TestARequestWithNoPromiseIsCompletedAndSettlesNothing. promise_id is
// nullable, so a request can exist with no caller blocked on it.
func TestARequestWithNoPromiseIsCompletedAndSettlesNothing(t *testing.T) {
	store := &fakeUpdateStore{}
	s := newUpdateSession(t, store, nil)

	s.DurableCompleteUpdate(context.Background(), nil, "fire-and-forget\x00", "done", "")
	if len(store.completed) != 1 {
		t.Errorf("the request row was not completed: %v", store.completed)
	}
	if len(store.resolved) != 0 || len(store.rejected) != 0 {
		t.Errorf("settled something for a request carrying no promise: resolved=%v rejected=%v.\n"+
			"ResolvePromise is keyed by promise ID alone, so an empty one would address "+
			"whatever a blank ID matches.", store.resolved, store.rejected)
	}
}

// TestCompactionPreservesTheUpdateEvents asserts that the two update events
// survive a compaction round trip with every field intact.
//
// It exists because the fuzzer that is supposed to cover this could not reach
// them, twice over, and neither failure announced itself:
//
//   - parseFuzzEvents clamped its type byte with a hardcoded `% 30` while the
//     new codes are 34 and 35, so they were unreachable. That literal had
//     already rotted once before (`% 27` left three cron codes unfuzzed) and
//     had rotted again since (31, 32, 33 were unreachable too). It is now
//     derived from codeToEventType.
//   - even reachable, FuzzCompactionEquivalence run WITHOUT -fuzz executes only
//     its seed corpus, and no seed happens to produce these codes.
//
// Deleting `rec.UpdatePayload = ce.Request` from compaction.go left the fuzz
// test green both times. It fails this one. A fuzzer is a way of finding cases
// nobody thought of; it is not a substitute for asserting the case you know
// about.
func TestCompactionPreservesTheUpdateEvents(t *testing.T) {
	events := []EventRecord{
		{
			Step:              0,
			EventType:         EventTypeUpdateReceived,
			TimestampMs:       1_700_000_000_000,
			UpdateHandlerName: "add",
			UpdateRequestID:   "add\x00prom-1",
			UpdatePayload:     `{"n":5}`,
		},
		{
			Step:              1,
			EventType:         EventTypeUpdateCompleted,
			TimestampMs:       1_700_000_000_001,
			UpdateHandlerName: "add",
			UpdateRequestID:   "add\x00prom-1",
			UpdateResponse:    `{"ok":true}`,
			UpdateError:       "",
		},
		{
			Step:              2,
			EventType:         EventTypeUpdateCompleted,
			TimestampMs:       1_700_000_000_002,
			UpdateHandlerName: "approve",
			UpdateRequestID:   "approve\x00prom-2",
			UpdateError:       "amount must be positive",
		},
	}

	cs, err := extractCompactionState(events)
	if err != nil {
		t.Fatalf("extractCompactionState: %v", err)
	}
	got := buildFullHistoryFromCompaction(nil, cs)

	if len(got) != len(events) {
		t.Fatalf("round trip produced %d events, want %d", len(got), len(events))
	}
	for i := range events {
		if !eventFieldsMatch(events[i], got[i]) {
			t.Errorf("event %d did not survive compaction.\n  before: %+v\n   after: %+v\n\n"+
				"An update's replay depends on exactly these fields: the payload is the "+
				"handler's input and the response/error is what settles the caller's promise.",
				i, events[i], got[i])
		}
	}
}
