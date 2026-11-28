package engine

import (
	"context"
	"strings"
	"testing"
)

// cleat#4335 (which closed cleat#4203). create_promise, resolve_promise and reject_promise used to record their event FIRST and write the promise store second, and a write that
// failed was told to the guest and recorded nowhere, so a replay of that history answered success where the original run had been told an error. The event now records the
// failure in Err and replay answers from it. The characterization rows (a_promise_single_step_behaves_as_recorded_before_the_dispatcher_test.go) pin the status words; the
// round trip through a real event history on each dialect is step_dispatcher_roundtrip_test.go. This file pins what those cannot see without a database or a wasm module:
//
//   - the MESSAGE the guest is handed by create_promise, on the original run and on a replay (the characterization rows call with no memory, so they see the status and not the text);
//   - that the failure survives COMPACTION, which copies fields per event type, and the checksum chain, which hashes per event type;
//   - that a history recorded before the field existed still answers success.

// guestBuffer returns a context whose host-call result buffer is a slice the test can read back, and that slice. It is the path writeResult takes for a component
// dispatcher, so no wasm module is needed to see what the guest is handed.
func guestBuffer(size int) (context.Context, []byte) {
	buf := make([]byte, size)
	return context.WithValue(context.Background(), wasmMemBufKey{}, buf), buf
}

func textIn(buf []byte, n uint32) string { return string(buf[:n]) }

func TestACreatePromiseTheStoreRefusedReplaysWithTheMessageTheOriginalRunWasHanded(t *testing.T) {
	store := &countingPromiseStore{refuse: true}
	newSession := func(history []EventRecord) *execSession {
		s := promiseSession(&mockPromiseStore{}, history)
		s.engine = NewEngine(nil, nil, WithPromiseStore(store))
		return s
	}

	ctx, buf := guestBuffer(256)
	fresh := newSession(nil)
	freshRes := fresh.CreatePromise(ctx, nil, "n", 0, 256)
	freshText := textIn(buf, uint32(freshRes>>32))
	if byte(freshRes) != 1 || !strings.Contains(freshText, errRoundTripStoreDown.Error()) {
		t.Fatalf("PRECONDITION FAILED: the original run was told %#x with text %q, want errCode 1 and the store's message, so this test measures nothing", freshRes, freshText)
	}
	if len(fresh.history) != 1 || fresh.history[0].Err != errRoundTripStoreDown.Error() {
		t.Fatalf("PRECONDITION FAILED: the event the refused create recorded is %+v, want one event whose Err is the store's message", fresh.history)
	}
	if store.writes != 1 {
		t.Fatalf("PRECONDITION FAILED: %d store writes on the original run, want 1", store.writes)
	}

	rctx, rbuf := guestBuffer(256)
	replay := newSession(append([]EventRecord(nil), fresh.history...))
	replayRes := replay.CreatePromise(rctx, nil, "n", 0, 256)
	if replayRes != freshRes {
		t.Errorf("the replay answered %#x where the original run was told %#x", replayRes, freshRes)
	}
	if got := textIn(rbuf, uint32(replayRes>>32)); got != freshText {
		t.Errorf("the replay handed the guest %q where the original run was handed %q", got, freshText)
	}
	if store.writes != 1 {
		t.Errorf("%d store writes after the replay, want still 1: replay did the effect again", store.writes)
	}
}

// The three event types a refused write is recorded on, with the call that records each and what its replay must answer.
var refusedWriteEvents = []struct {
	name string
	call func(s *execSession) int64
	typ  EventType
}{
	{"create_promise", func(s *execSession) int64 { return s.CreatePromise(context.Background(), nil, "n", 0, 0) }, EventTypeCreatePromise},
	{"resolve_promise", func(s *execSession) int64 { return s.ResolvePromise(context.Background(), nil, "p-1", "v") }, EventTypePromiseResolved},
	{"reject_promise", func(s *execSession) int64 { return s.RejectPromise(context.Background(), nil, "p-1", "e") }, EventTypePromiseRejected},
}

// A refused write survives compaction and the checksum chain, and the replay of the EXPANDED history still answers the failure. Compaction copies fields per event type in
// both directions (cleat#4231's class), and eventRecordToPayload hashes per event type, so a type that was not taught about Err would lose it silently on one of the two.
func TestARefusedPromiseWriteSurvivesCompactionAndTheChecksumChain(t *testing.T) {
	for _, tc := range refusedWriteEvents {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := promiseSession(&mockPromiseStore{}, nil)
			s.engine = NewEngine(nil, nil, WithPromiseStore(&refusingPromiseStore{}))
			if res := tc.call(s); byte(res) != 1 || len(s.history) != 1 || s.history[0].Err == "" {
				t.Fatalf("PRECONDITION FAILED: the refused call answered %#x and recorded %+v; want errCode 1 and one event with an Err", res, s.history)
			}
			recorded := s.history[0]
			recorded.TimestampMs = 1000

			// compaction, both directions
			cs, err := extractCompactionState([]EventRecord{recorded})
			if err != nil {
				t.Fatalf("extractCompactionState: %v", err)
			}
			full := buildFullHistoryFromCompaction(nil, cs)
			if len(full) != 1 || full[0].Err != recorded.Err {
				t.Fatalf("the expanded history is %+v, want one %s event with Err %q: compaction dropped the failure", full, tc.typ, recorded.Err)
			}

			// the checksum: the chain over the expanded history is the chain over the recorded one, and it DEPENDS on Err (an edited Err breaks it)
			if got, want := computeEventChecksum(full[0], ""), computeEventChecksum(recorded, ""); got != want {
				t.Errorf("the chain over the expanded event is %s, over the recorded one %s", got, want)
			}
			tampered := recorded
			tampered.Err = "something else"
			if computeEventChecksum(tampered, "") == computeEventChecksum(recorded, "") {
				t.Errorf("the checksum does not cover Err: an edited failure message leaves the chain intact, so the failure the replay answers from can be changed without detection")
			}

			// the payload the checksum is computed from reads back into the record
			payload, err := eventRecordToPayload(recorded)
			if err != nil {
				t.Fatal(err)
			}
			var back EventRecord
			back.EventType = recorded.EventType
			populateFromPayload(&back, payload)
			if back.Err != recorded.Err {
				t.Errorf("the payload read back Err = %q, recorded %q", back.Err, recorded.Err)
			}

			// and the replay of the EXPANDED history is still the failure
			rs := promiseSession(&mockPromiseStore{}, full)
			if res := tc.call(rs); byte(res) != 1 {
				t.Errorf("the replay of the compacted history answered %#x, want errCode 1: the failure did not survive", res)
			}
		})
	}
}

// A history recorded before this field existed carries no Err, and answers success exactly as it always did. Written as its own row because that is the compatibility claim of the
// change, and it is the one a careless Decode (`rec.Err == "" ` read the wrong way round) would break for every workflow already in flight.
func TestAPromiseCallRecordedWithNoErrStillReplaysAsSuccess(t *testing.T) {
	for _, tc := range refusedWriteEvents {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := promiseSession(&mockPromiseStore{}, nil)
			if res := tc.call(s); res != 0 || len(s.history) != 1 || s.history[0].Err != "" {
				t.Fatalf("PRECONDITION FAILED: the successful call answered %#x and recorded %+v; want success and one event with no Err", res, s.history)
			}
			rs := promiseSession(&mockPromiseStore{}, append([]EventRecord(nil), s.history...))
			if res := tc.call(rs); res != 0 {
				t.Errorf("a history with no Err replayed as %#x, want success (0)", res)
			}
		})
	}
}
