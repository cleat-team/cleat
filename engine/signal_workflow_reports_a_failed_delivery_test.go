package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// cleat#4102: signal_workflow returned success to the guest when the signal store refused the delivery, after recording an event that
// said it was sent. Replay returns what the event says, so the signal was lost for good and the workflow was told it was not.
//
// These tests drive the real SignalWorkflow through the unit harness (newTestExecSession, mockSignalStore in host_test.go). The
// real-store control (a foreign tenant's run id and a nonexistent one are indistinguishable) is in
// a_signal_to_a_foreign_or_nonexistent_target_reports_one_outcome_test.go, because it needs a database per dialect.

// orderProbe is a SignalStore that records how many events the session held when DeliverSignal was called.
type orderProbe struct {
	mockSignalStore
	sess          *execSession
	historyAtCall int
	calls         int
}

func (p *orderProbe) DeliverSignal(ctx context.Context, wf, name, payload string) error {
	p.calls++
	p.historyAtCall = len(p.sess.history)
	return p.mockSignalStore.DeliverSignal(ctx, wf, name, payload)
}

func lastEvent(t *testing.T, s *execSession) EventRecord {
	t.Helper()
	if len(s.history) == 0 {
		t.Fatal("no event was recorded")
	}
	return s.history[len(s.history)-1]
}

func TestASignalTheStoreRefusesIsReportedToTheGuestAndRecorded(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		want    int64
		outcome string
	}{
		{"a store error is a retryable delivery failure", fmt.Errorf("db down"), errSignalDeliveryFailedInt, signalOutcomeDeliveryFailed},
		{"not found is terminal", ErrWorkflowNotFound, errSignalTargetNotFoundInt, signalOutcomeNotFound},
		{"a wrapped not found is still not found", fmt.Errorf("deliver: %w", ErrWorkflowNotFound), errSignalTargetNotFoundInt, signalOutcomeNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestExecSession()
			s.engine.signalStore = &mockSignalStore{deliverErr: c.err}
			got := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)
			if got != c.want {
				t.Fatalf("result = %d, want %d: the guest must be told the delivery did not happen", got, c.want)
			}
			ev := lastEvent(t, s)
			if ev.EventType != EventTypeSignalReceived || ev.Service != SignalEventService || ev.Op != SignalOpSend || ev.Response != c.outcome {
				t.Fatalf("recorded %s %s/%s outcome %q, want signal_received signal/send %q", ev.EventType, ev.Service, ev.Op, ev.Response, c.outcome)
			}
		})
	}

	t.Run("CONTROL: a delivered signal is still success, with an empty outcome", func(t *testing.T) {
		s := newTestExecSession()
		mock := &mockSignalStore{}
		s.engine.signalStore = mock
		if got := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`); got != 0 {
			t.Fatalf("result = %d, want 0", got)
		}
		if ev := lastEvent(t, s); ev.Response != "" || ev.Op != SignalOpSend {
			t.Fatalf("recorded op %q outcome %q, want send with no outcome", ev.Op, ev.Response)
		}
		if mock.lastDeliverWFID != "target-run-123" || mock.lastDeliverName != "my-signal" || mock.lastDeliverPayload != `{"x":1}` {
			t.Fatalf("the store was handed %q/%q/%q", mock.lastDeliverWFID, mock.lastDeliverName, mock.lastDeliverPayload)
		}
	})
}

// The order is the contract. Recorded first, a store error and a crash between the two steps both lose the signal for good.
func TestASignalIsDeliveredBeforeItsEventIsRecorded(t *testing.T) {
	s := newTestExecSession()
	probe := &orderProbe{sess: s}
	s.engine.signalStore = probe
	before := len(s.history)
	s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{}`)
	if probe.calls != 1 {
		t.Fatalf("DeliverSignal was called %d times, want 1", probe.calls)
	}
	if probe.historyAtCall != before {
		t.Fatalf("the session held %d events when DeliverSignal ran, want %d: the event was recorded BEFORE the delivery, which is the order "+
			"that loses a signal on a crash", probe.historyAtCall, before)
	}
	if len(s.history) != before+1 {
		t.Fatalf("%d events after the call, want %d", len(s.history), before+1)
	}
}

// Replay returns the recorded outcome and does not deliver again or check authorization again. Re-running either could succeed where
// the original did not, and then the guest's later events, recorded against the failure, would no longer match.
func TestReplayReturnsTheRecordedOutcomeWithoutTouchingTheStore(t *testing.T) {
	send := func(outcome string) EventRecord {
		return EventRecord{Step: 0, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: SignalOpSend, Response: outcome,
			SignalName: "my-signal", SignalPayload: `{}`, RunID: "target-run-123"}
	}
	for _, c := range []struct {
		name string
		rec  EventRecord
		want int64
	}{
		{"a recorded delivery failure", send(signalOutcomeDeliveryFailed), errSignalDeliveryFailedInt},
		{"a recorded not found", send(signalOutcomeNotFound), errSignalTargetNotFoundInt},
		{"a recorded authorization refusal", send(signalOutcomeAuthRequired), errSignalAuthRequiredInt},
		{"a recorded success", send(""), 0},
		// An outcome this build does not know must not replay as success.
		{"an outcome written by a newer engine", send("some_future_outcome"), errSignalDeliveryFailedInt},
		// What every send recorded before cleat#4102 looks like, and what a delivery looks like: no Service, no Op.
		{"a send recorded before the outcome existed", EventRecord{Step: 0, EventType: EventTypeSignalReceived, SignalName: "my-signal", RunID: "target-run-123"}, 0},
		// A Response on an event with no Op is not an outcome: only a send carries one.
		{"a delivery event is never read as an outcome", EventRecord{Step: 0, EventType: EventTypeSignalReceived, SignalName: "x", Response: signalOutcomeDeliveryFailed}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestExecSession()
			mock := &mockSignalStore{}
			s.engine.signalStore = mock
			authCalls := 0
			s.engine.requireSignalAuth = true
			s.engine.signalAuthCheck = func(context.Context, string, string) error { authCalls++; return nil }
			s.isReplay = true
			s.history = []EventRecord{c.rec}
			if got := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{}`); got != c.want {
				t.Fatalf("replayed result = %d, want %d", got, c.want)
			}
			if mock.lastDeliverWFID != "" || authCalls != 0 {
				t.Fatalf("replay touched the store (%q) or the authorization check (%d calls): it must return what was recorded", mock.lastDeliverWFID, authCalls)
			}
		})
	}
}

// The authorization refusal had the same hole in the other direction: the guest saw the error live and nothing was recorded, so a
// replay re-evaluated authorization, which could answer differently.
func TestAnAuthorizationRefusalIsRecordedAndReplayed(t *testing.T) {
	s := newTestExecSession()
	mock := &mockSignalStore{}
	s.engine.signalStore = mock
	s.engine.requireSignalAuth = true
	s.engine.signalAuthCheck = func(context.Context, string, string) error { return errors.New("not authorized") }
	if got := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{}`); got != errSignalAuthRequiredInt {
		t.Fatalf("result = %d, want errSignalAuthRequiredInt", got)
	}
	if mock.lastDeliverWFID != "" {
		t.Fatal("a refused signal reached the store")
	}
	if ev := lastEvent(t, s); ev.Response != signalOutcomeAuthRequired || ev.Op != SignalOpSend {
		t.Fatalf("recorded op %q outcome %q, want send/%s", ev.Op, ev.Response, signalOutcomeAuthRequired)
	}
}

// The outcome is what replay returns, so it has to be inside the checksum: an edited outcome must break the chain. Rows that are not a
// send must hash exactly as they always did, or every stored chain is invalid.
func TestARecordedSendOutcomeIsInsideTheEventChecksum(t *testing.T) {
	send := func(outcome string) EventRecord {
		return EventRecord{Step: 3, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: SignalOpSend, Response: outcome,
			SignalName: "my-signal", SignalPayload: `{}`}
	}
	chain := func(r EventRecord) string { return computeEventChecksum(r, "prev") }

	delivered, failed, notFound := chain(send("")), chain(send(signalOutcomeDeliveryFailed)), chain(send(signalOutcomeNotFound))
	if delivered == failed || failed == notFound || delivered == notFound {
		t.Fatalf("outcomes hash alike (delivered %s, failed %s, not found %s): editing the recorded outcome would not break the chain", delivered, failed, notFound)
	}

	// The control that makes the inequality above mean something: the same outcome hashes the same.
	if chain(send(signalOutcomeDeliveryFailed)) != failed {
		t.Fatal("the checksum is not deterministic")
	}

	// GOLDEN: a delivery (no Op) and a send recorded before the outcome existed keep their exact payload, so their stored checksums still verify.
	delivery := EventRecord{Step: 3, EventType: EventTypeSignalReceived, SignalName: "my-signal", SignalPayload: `{}`}
	got, err := eventRecordToPayload(delivery)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	if want := `{"signal_name":"my-signal","signal_payload":"{}"}`; string(got) != want {
		t.Fatalf("a delivery's checksum payload is %s, want %s: the payload of every existing signal_received row changed", got, want)
	}
}

// A compacted region is rebuilt from a stored state, and a send's outcome is what replay returns, so compaction must carry it.
func TestACompactedSendKeepsItsOutcome(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: SignalOpSend, Response: signalOutcomeDeliveryFailed,
			SignalName: "a", SignalPayload: "1", RunID: "t", TimestampMs: 1000},
		{Step: 1, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: SignalOpSend, Response: "",
			SignalName: "b", SignalPayload: "2", RunID: "t", TimestampMs: 1001},
		// A delivery: same event type, none of the three fields.
		{Step: 2, EventType: EventTypeSignalReceived, SignalName: "c", SignalPayload: "3", TimestampMs: 1002},
		{Step: 3, EventType: EventTypeCall, Service: "work", Op: "charge", Request: "{}", Response: "{}", TimestampMs: 1003},
	}
	cs, err := extractCompactionState(events)
	if err != nil {
		t.Fatalf("extractCompactionState: %v", err)
	}
	full := buildFullHistoryFromCompaction(nil, cs)
	if len(full) != len(events) {
		t.Fatalf("expanded history has %d events, recorded %d", len(full), len(events))
	}
	for i := 0; i < 3; i++ {
		if got, want := signalSendOutcomeCode(full[i]), signalSendOutcomeCode(events[i]); got != want {
			t.Errorf("step %d: replayed result after compaction = %d, before = %d: compaction changed what the guest is told", i, got, want)
		}
		if full[i].Service != events[i].Service || full[i].Op != events[i].Op || full[i].Response != events[i].Response {
			t.Errorf("step %d: expanded %q/%q/%q, recorded %q/%q/%q", i, full[i].Service, full[i].Op, full[i].Response,
				events[i].Service, events[i].Op, events[i].Response)
		}
	}
	if signalSendOutcomeCode(full[0]) != errSignalDeliveryFailedInt {
		t.Fatal("the failed send must still replay as a failure after compaction, or this test compares two successes")
	}
}
