package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#933: a session that has set suspendErr kept doing new work, because
// the guest cannot tell a suspend from a timeout.
//
// DurableAwaitSignals returns packAwaitSignalsResult(0, 0, true, 0) when it
// suspends -- byte-identical to a genuine timeout. The guest runs on, and every
// later host call recorded another event into a segment that had already ended.
//
// The observable consequence is the history the samples-go port dumps: two
// await_signals at the SAME millisecond, recorded three seconds before the
// signal existed, after which the delivery pairs with the second await and the
// first times out.

type suspendProbeStore struct{ deliveries map[string]SignalDelivery }

func (a *suspendProbeStore) DeliverSignal(ctx context.Context, wf, n, p string) error { return nil }
func (a *suspendProbeStore) PollSignal(ctx context.Context, wf, n string) (SignalDelivery, bool, error) {
	d, ok := a.deliveries[n]
	return d, ok, nil
}
func (a *suspendProbeStore) ConsumeSignal(ctx context.Context, wf string, id int64) error { return nil }
func (a *suspendProbeStore) PollCancellation(ctx context.Context, wf string) (bool, string, error) {
	return false, "", nil
}

func freshSuspendSession(t *testing.T) *execSession {
	t.Helper()
	eng := NewEngine(nil, &mockCaller{},
		WithSignalStore(&suspendProbeStore{deliveries: map[string]SignalDelivery{}}),
		WithWorkflowID("wf-1"))
	return &execSession{
		engine: eng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: false,
	}
}

func awaitOnce(s *execSession) int64 {
	buf := make([]byte, 512)
	c := contextWithRawMemBuf(context.Background(), buf)
	return s.DurableAwaitSignals(c, nil, `["a","b"]`, 20000, 0, 200, 256, 200)
}

// TestASecondAwaitAfterASuspendRecordsNothing is the defect, and the history it
// asserts is the one dumped from the real system.
func TestASecondAwaitAfterASuspendRecordsNothing(t *testing.T) {
	s := freshSuspendSession(t)

	awaitOnce(s)
	if s.suspendErr == nil {
		t.Fatal("the first await did not suspend, so this test is not measuring what it claims")
	}
	if len(s.history) != 1 {
		t.Fatalf("the first await recorded %d events, want 1", len(s.history))
	}

	awaitOnce(s)

	if len(s.history) != 1 {
		t.Errorf("history grew to %d after a second await in a suspended session.\n\n"+
			"Two await_signals in one segment is the shape the samples-go port dumps, "+
			"and it is what makes the next replay pair the delivery with the SECOND "+
			"await and time out the first.", len(s.history))
	}
}

// TestOnlyAwaitsAreGatedAfterASuspend records the SCOPE of the fix, including
// what it deliberately does not cover, so the limit is asserted rather than
// left to be rediscovered.
//
// A second await after a suspend is refused. A DurableCall or a SideEffect is
// NOT, and that is not an oversight: widening stopBeforeNewWork to cover every
// host call was tried and two tests refused it. At a continue-as-new boundary
// the GUEST drains its own defer table by returning through its wrapper, with
// no inDeferDrain bracket to tell it apart from ordinary work, so a general
// gate refused the cleanup itself --
// TestTheEventCapDoesNotDispatchTheCallItRefused: "the cleanup a workflow
// registered did not run".
//
// So this test asserts the gap on purpose. If someone later finds a way to
// distinguish a guest-driven defer drain, this is the test that should change,
// and its failure will say why the gap existed.
func TestOnlyAwaitsAreGatedAfterASuspend(t *testing.T) {
	s := freshSuspendSession(t)
	awaitOnce(s)
	if s.suspendErr == nil {
		t.Fatal("no suspend; the rest of this test would prove nothing")
	}

	if got := awaitOnce(s); uint64(got)&uint64(callSuspendSentinel) == 0 {
		t.Errorf("a second await returned %#x, which does not carry the stop sentinel", got)
	}

	// The documented gap. These still do new work.
	buf := make([]byte, 512)
	c := contextWithRawMemBuf(context.Background(), buf)
	before := len(s.history)
	s.DurableCall(c, nil, "svc", "op", "{}", 0, 200)
	s.SideEffect(c, nil, "x", 0, 200)
	if len(s.history) == before {
		t.Log("DurableCall and SideEffect no longer record after a suspend. If that is " +
			"deliberate, delete this test and the scoping comment in " +
			"DurableAwaitSignals -- the gap they describe has been closed.")
	}
}

// TestTheFirstSuspendingCallStillWorks is the control. The fix must gate what
// comes AFTER a suspend, not the call that causes it -- gating too early would
// stop a workflow suspending at all.
func TestTheFirstSuspendingCallStillWorks(t *testing.T) {
	s := freshSuspendSession(t)

	packed := awaitOnce(s)

	if s.suspendErr == nil {
		t.Error("the first await did not suspend")
	}
	if len(s.history) != 1 {
		t.Errorf("the first await recorded %d events, want 1: it must still write its "+
			"await_signals", len(s.history))
	}
	if uint64(packed)&uint64(callSuspendSentinel) != 0 {
		t.Error("the suspending call itself returned the stop sentinel; only calls after " +
			"it should")
	}
}

// TestADeliveryStillSatisfiesTheFirstAwait is the other control: a session that
// has NOT suspended must be unaffected.
//
// It passes a BARE name rather than the JSON array a guest actually sends, and
// that is not tidiness -- with `["a","b"]` this control fails on develop, before
// and after this change. The fresh path parses names with splitSignalNames,
// which splits on commas only, so `["a","b"]` becomes `["a` and `"b]` and
// matches nothing in the store. The replay arm json.Unmarshals with that as a
// fallback (engine/signaller.go:110); the fresh path does not (:152).
//
// Filed separately. Using the format this path can parse keeps the control
// measuring what it claims -- that a suspended session is gated and an
// unsuspended one is not -- instead of failing for an unrelated reason.
func TestADeliveryStillSatisfiesTheFirstAwait(t *testing.T) {
	eng := NewEngine(nil, &mockCaller{},
		WithSignalStore(&suspendProbeStore{deliveries: map[string]SignalDelivery{
			"a": {ID: 1, Payload: "p"},
		}}),
		WithWorkflowID("wf-1"))
	s := &execSession{
		engine: eng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
	}

	buf := make([]byte, 512)
	c := contextWithRawMemBuf(context.Background(), buf)
	packed := s.DurableAwaitSignals(c, nil, "a,b", 20000, 0, 200, 256, 200)

	nameLen := uint32((packed >> 48) & 0xFFFF)
	if got := string(buf[:nameLen]); got != "a" {
		t.Errorf("the await returned %q, want \"a\"", got)
	}
	if s.suspendErr != nil {
		t.Error("suspended even though a delivery was available")
	}
}
