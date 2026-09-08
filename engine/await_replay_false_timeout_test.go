package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#933: a replay that reached a recorded await_signals with history
// exhausted and no delivery in the store returned timedOut=true WITHOUT
// suspending, so the guest read a timeout that had not happened and ran on to
// its next await.
//
// The fault is one segment earlier than where it shows. The corrupt history
// that segment writes --
//
//	step 0  await_signals
//	step 1  await_signals      <- recorded because step 0 falsely returned
//	step 2  signal_received
//
// -- is then replayed faithfully, pairing the delivery with the SECOND await
// and timing out the first. That misattribution is what the samples-go port
// reports; the replay arm reproduces it correctly from a history that should
// never have existed.

type awaitProbeStore struct{ deliveries map[string]SignalDelivery }

func (a *awaitProbeStore) DeliverSignal(ctx context.Context, wf, n, p string) error { return nil }
func (a *awaitProbeStore) PollSignal(ctx context.Context, wf, n string) (SignalDelivery, bool, error) {
	d, ok := a.deliveries[n]
	return d, ok, nil
}
func (a *awaitProbeStore) ConsumeSignal(ctx context.Context, wf string, id int64) error { return nil }
func (a *awaitProbeStore) PollCancellation(ctx context.Context, wf string) (bool, string, error) {
	return false, "", nil
}

func awaitReplaySession(t *testing.T, store *awaitProbeStore, hist []EventRecord) *execSession {
	t.Helper()
	eng := NewEngine(nil, &mockCaller{}, WithSignalStore(store), WithWorkflowID("wf-1"))
	return &execSession{
		engine: eng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: true, history: hist,
	}
}

func recordedAwait(timeoutMs int64) EventRecord {
	return EventRecord{
		Step: 0, EventType: EventTypeAwaitSignals,
		SignalNames: `["a","b"]`, TimeoutMs: timeoutMs,
		TimestampMs: time.Now().UnixMilli(),
	}
}

func callAwait(t *testing.T, s *execSession) (timedOut bool) {
	t.Helper()
	buf := make([]byte, 512)
	c := contextWithRawMemBuf(context.Background(), buf)
	packed := s.DurableAwaitSignals(c, nil, `["a","b"]`, 60000, 0, 200, 256, 200)
	return (packed>>16)&0xFFFF != 0
}

// TestAnUnresolvedAwaitSuspendsRatherThanReportingATimeout is the defect.
func TestAnUnresolvedAwaitSuspendsRatherThanReportingATimeout(t *testing.T) {
	s := awaitReplaySession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}},
		[]EventRecord{recordedAwait(60_000)})

	callAwait(t, s)

	if s.suspendErr == nil {
		t.Fatal("the await did not suspend.\n\n" +
			"History is exhausted at a recorded await_signals and the store holds no " +
			"delivery, so the await has NOT resolved. Returning without suspending " +
			"hands the guest a timeout that did not happen, and it runs on to its " +
			"next await -- which is what writes the two-await history cleat#933 " +
			"reports.")
	}
}

// TestBothPathsReturnTheSameWordAndOnlyOneSuspends is why this survived, stated
// as a test so the next reader does not have to rediscover it.
//
// The fresh path also returns timedOut=true when no signal is available. It is
// correct there only because it sets suspendErr first, so the segment ends and
// the guest never observes the value. The returned word is not the contract;
// suspendErr is.
func TestBothPathsReturnTheSameWordAndOnlyOneSuspends(t *testing.T) {
	fresh := awaitReplaySession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}}, nil)
	fresh.isReplay = false
	freshTimedOut := callAwait(t, fresh)

	replay := awaitReplaySession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}},
		[]EventRecord{recordedAwait(60_000)})
	replayTimedOut := callAwait(t, replay)

	if !freshTimedOut || !replayTimedOut {
		t.Fatalf("expected both to return timedOut=true (fresh=%v replay=%v); the point "+
			"is that the word is identical", freshTimedOut, replayTimedOut)
	}
	if fresh.suspendErr == nil {
		t.Error("the fresh path did not suspend")
	}
	if replay.suspendErr == nil {
		t.Error("the replay path did not suspend, so the identical return word is " +
			"observed by the guest in one case and discarded in the other")
	}
}

// TestTheCorruptHistoryIsNoLongerWritten asserts the consequence rather than
// the mechanism: the segment must end at the first await, so no second
// await_signals is appended.
func TestTheCorruptHistoryIsNoLongerWritten(t *testing.T) {
	s := awaitReplaySession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}},
		[]EventRecord{recordedAwait(60_000)})

	callAwait(t, s)

	if s.suspendErr == nil {
		t.Fatal("no suspend, so the guest would continue and record a second await")
	}
	if len(s.history) != 1 {
		t.Errorf("history grew to %d records; a suspended segment must not append. "+
			"Two await_signals with no signal_received between them is exactly the "+
			"shape that misattributes the delivery on the next replay.", len(s.history))
	}
}

// TestAnAwaitPastItsDeadlineStillTimesOut is the control. The fix must not turn
// every replayed await into an indefinite wait -- a deadline that has genuinely
// passed is a timeout, and reporting it is correct.
func TestAnAwaitPastItsDeadlineStillTimesOut(t *testing.T) {
	old := recordedAwait(1_000)
	old.TimestampMs = time.Now().Add(-time.Hour).UnixMilli() // expired long ago

	s := awaitReplaySession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}},
		[]EventRecord{old})

	if timedOut := callAwait(t, s); !timedOut {
		t.Error("an await whose deadline passed an hour ago did not report a timeout")
	}
	if s.suspendErr != nil {
		t.Error("an expired await suspended instead of timing out, so the workflow " +
			"would never make progress")
	}
}

// TestTheDeadlineComesFromTheRecordedAwaitNotFromNow. Deriving it from the
// current clock would push the expiry further out on every replay, and an await
// with a timeout would never reach it.
func TestTheDeadlineComesFromTheRecordedAwaitNotFromNow(t *testing.T) {
	rec := recordedAwait(60_000)
	rec.TimestampMs = time.Now().Add(-30 * time.Second).UnixMilli()

	s := awaitReplaySession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}},
		[]EventRecord{rec})
	callAwait(t, s)

	if s.suspendErr == nil {
		t.Fatal("expected a suspend")
	}
	want := time.UnixMilli(rec.TimestampMs + rec.TimeoutMs)
	if got := s.suspendErr.Until; !got.Equal(want) {
		t.Errorf("suspend deadline is %v, want %v (recorded await + its timeout).\n\n"+
			"A deadline derived from now would sit ~30s later here, and would move "+
			"again on the next replay.", got, want)
	}
}

// TestAReplayedAwaitStillTakesADeliveryThatIsThere is the other control: the
// fix must not stop a genuinely available signal from being delivered.
func TestAReplayedAwaitStillTakesADeliveryThatIsThere(t *testing.T) {
	store := &awaitProbeStore{deliveries: map[string]SignalDelivery{
		"a": {ID: 7, Payload: "p"},
	}}
	s := awaitReplaySession(t, store, []EventRecord{recordedAwait(60_000)})

	buf := make([]byte, 512)
	c := contextWithRawMemBuf(context.Background(), buf)
	packed := s.DurableAwaitSignals(c, nil, `["a","b"]`, 60000, 0, 200, 256, 200)

	nameLen := uint32((packed >> 48) & 0xFFFF)
	if got := string(buf[:nameLen]); got != "a" {
		t.Errorf("the await returned %q, want \"a\": a delivery that IS present must "+
			"still be handed over", got)
	}
	if s.suspendErr != nil {
		t.Error("suspended despite a delivery being available")
	}
}
