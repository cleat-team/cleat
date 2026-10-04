package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// WaitForEvent's central claim, made measurable: A REPLAYED LOOP MAKES NO
// PLUGIN CALLS THAT THE HISTORY DOES NOT ALREADY ANSWER.
//
// The loop is only safe to move into the engine because replay cannot re-decide
// anything. Two mechanisms enforce that, and this test is aimed at both:
//
//  1. every attempt routes through s.PluginCall, which IS the fresh/replay
//     dispatcher -- so the loop never chooses an arm, and there is no path from
//     it to the fresh path during replay;
//  2. the awaited function is registered the way await_webhook is, satisfying
//     neither Idempotent nor SameValueOnReplay, so replayPluginCall serves the
//     RECORDED output rather than re-invoking (cleat#1318).
//
// If either fails, the counter below is non-zero on replay, and the failure is
// not a slower loop -- it is a live claim on mutable state, answering
// found=false where the record says true, which diverges at the next step.
//
// FALSIFICATION. The counter is asserted in BOTH directions: a FRESH session
// against the same counter must invoke the function exactly once. Without that
// arm a counter that never incremented would pass the replay assertion while
// measuring nothing.

type eventClaimCounter struct {
	calls    int
	outputs  []string
	returned string
}

// call is the plugin function. It answers from `outputs` in order, so a test can
// script a claim sequence; when the queue empties it repeats the last answer.
func (c *eventClaimCounter) call(ctx context.Context, input string) (string, error) {
	c.calls++
	c.returned = input
	if len(c.outputs) == 0 {
		return `{"found":false}`, nil
	}
	out := c.outputs[0]
	if len(c.outputs) > 1 {
		c.outputs = c.outputs[1:]
	}
	return out, nil
}

// waitForEventSession builds a session around a registry holding one
// claim-shaped function, registered EXACTLY as webhookingest registers
// await_webhook -- neither policy flag set. That registration is half of what
// this test measures, so it is stated rather than inherited from a helper that
// might drift.
func waitForEventSession(t *testing.T, counter *eventClaimCounter, hist []EventRecord, replay bool) *execSession {
	t.Helper()
	pr := NewPluginRegistry()
	if err := pr.Register("webhook-ingest", "await_webhook", counter.call); err != nil {
		t.Fatalf("registering the claim function: %v", err)
	}
	_, policy, _, ok := pr.Lookup("webhook-ingest", "await_webhook")
	if !ok {
		t.Fatal("the function did not register")
	}
	if policy.MayReInvokeOnReplay() {
		t.Fatal("this test is vacuous with a re-invocable function: replay would " +
			"legitimately call it, and the counter would prove nothing about the loop. " +
			"await_webhook sets neither Idempotent nor SameValueOnReplay.")
	}

	eng := NewEngine(nil, &mockCaller{}, WithPluginRegistry(pr), WithWorkflowID("wf-1"))
	return &execSession{
		engine: eng, workflowID: "wf-1", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: replay, history: hist,
	}
}

// pluginClaimRecord is one attempt's recorded plugin_call, named so the replay
// arm's divergence check (which compares EventType, PluginName and PluginFunc,
// and NOT the input) is satisfied.
func pluginClaimRecord(step int, output string) EventRecord {
	return EventRecord{
		Step: step, EventType: EventTypePluginCall,
		PluginName: "webhook-ingest", PluginFunc: "await_webhook",
		PluginOutput: output,
	}
}

// The recorded history a successful claim leaves behind, and it is the same
// sequence the application's hand-written loop produced:
//
//	plugin_call(found=false) -> await_signals -> signal_received -> plugin_call(found=true)
func recordedClaimSequence() []EventRecord {
	return []EventRecord{
		pluginClaimRecord(0, `{"found":false}`),
		{
			Step: 1, EventType: EventTypeAwaitSignals,
			SignalNames: `["__evt:payment"]`, TimeoutMs: eventRecheckMs,
			TimestampMs: time.Now().UnixMilli(),
		},
		{Step: 2, EventType: EventTypeSignalReceived, SignalName: "__evt:payment"},
		pluginClaimRecord(3, `{"found":true,"payload":"PAID"}`),
	}
}

func callWaitForEvent(s *execSession, buf []byte) (string, bool) {
	ctx := contextWithRawMemBuf(context.Background(), buf)
	packed := s.WaitForEvent(ctx, nil, "webhook-ingest", "await_webhook", "{}",
		`["__evt:payment"]`, 60_000, 0, 400)
	if byte(packed) != 0 || byte(packed>>8) != 0 {
		return "", false
	}
	n := uint32(uint64(packed) >> 40)
	return string(buf[:n]), true
}

// TestAReplayedWaitForEventDoesNotCallThePlugin is the claim.
func TestAReplayedWaitForEventDoesNotCallThePlugin(t *testing.T) {
	counter := &eventClaimCounter{}
	s := waitForEventSession(t, counter, recordedClaimSequence(), true)

	buf := make([]byte, 512)
	got, ok := callWaitForEvent(s, buf)
	if !ok {
		t.Fatalf("the replayed wait reported a failure; got %q", string(buf[:64]))
	}
	if got != `{"found":true,"payload":"PAID"}` {
		t.Errorf("replayed wait returned %q, want the recorded claim.\n\n"+
			"The output must come out of HISTORY. A different value means the loop "+
			"re-derived a decision the record already held.", got)
	}
	if counter.calls != 0 {
		t.Errorf("replay called the plugin function %d time(s), want 0.\n\n"+
			"await_webhook's registration satisfies neither Idempotent nor "+
			"SameValueOnReplay, so replayPluginCall serves the recorded output. A "+
			"non-zero count here means the loop reached the FRESH path during replay "+
			"-- and then it is claiming live, not replaying: found=false where the "+
			"record says true, diverging at the next step. cleat#1318.", counter.calls)
	}
}

// TestAFreshWaitForEventCallsThePluginOnce is the control, and it is the half
// that makes the assertion above mean something: the counter must be able to
// count.
func TestAFreshWaitForEventCallsThePluginOnce(t *testing.T) {
	counter := &eventClaimCounter{outputs: []string{`{"found":true,"payload":"PAID"}`}}
	s := waitForEventSession(t, counter, nil, false)

	buf := make([]byte, 512)
	got, ok := callWaitForEvent(s, buf)
	if !ok {
		t.Fatalf("the fresh wait reported a failure; got %q", string(buf[:64]))
	}
	if counter.calls != 1 {
		t.Fatalf("a fresh claim called the plugin %d time(s), want exactly 1.\n\n"+
			"If this is 0 the counter cannot count, and "+
			"TestAReplayedWaitForEventDoesNotCallThePlugin passes while measuring "+
			"nothing.", counter.calls)
	}
	if got != `{"found":true,"payload":"PAID"}` {
		t.Errorf("fresh wait returned %q, want the claim's own output", got)
	}
}

// TestAFreshWaitForEventThatFindsNothingSuspendsRatherThanSpinning. The
// not-found path must end the SEGMENT, not loop hot: a wake is not proof of a
// claimable event, but a loop that re-claims without ever suspending would burn
// the whole budget in one segment and never let the worker release the run.
func TestAFreshWaitForEventThatFindsNothingSuspendsRatherThanSpinning(t *testing.T) {
	counter := &eventClaimCounter{} // always {"found":false}
	s := waitForEventSession(t, counter, nil, false)

	buf := make([]byte, 512)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	packed := s.WaitForEvent(ctx, nil, "webhook-ingest", "await_webhook", "{}",
		`["__evt:payment"]`, 60_000, 0, 400)

	if s.suspendErr == nil {
		t.Fatal("the wait neither claimed nor suspended.\n\n" +
			"Nothing was claimed and no suspend was set, so the guest would run on " +
			"having waited for nothing.")
	}
	if counter.calls > 1 {
		t.Errorf("the wait attempted the claim %d times inside one segment; it must "+
			"suspend after the first miss and re-claim on the WAKE", counter.calls)
	}

	// AND THE GUEST MUST BE TOLD, which is a separate claim from the one above:
	// s.suspendErr is internal, and this is what crosses the ABI. A decodable
	// value reads as an empty SUCCESSFUL event, so a caller proceeds with an
	// event that never arrived and records whatever it did with it -- cleat#933,
	// "the fault is that the history was written". Asserting only that a suspend
	// was set cannot see that, so this asserts the value the guest decodes.
	if packed != callSuspendSentinel {
		t.Errorf("the suspended wait returned %#x, want callSuspendSentinel (%#x).\n\n"+
			"Any other value is DECODED by the guest. packDurableCallResult(0,0,0) "+
			"reads as responseLen=0, errCode=0 -- an empty successful event -- so the "+
			"workflow runs on with an event that never arrived, exactly as a suspecting "+
			"guest cannot tell a suspending await from a timeout (cleat#933).",
			packed, callSuspendSentinel)
	}
}

// TestWaitForEventRejectsAnOutputThatIsNotClaimShaped is the guard a caller
// depends on: a function that never answers the question must be reported, not
// waited on until the budget runs out and then blamed on a timeout.
func TestWaitForEventRejectsAnOutputThatIsNotClaimShaped(t *testing.T) {
	for _, out := range []string{`{"payload":"x"}`, `{}`, `not json`} {
		t.Run(fmt.Sprintf("%q", out), func(t *testing.T) {
			counter := &eventClaimCounter{outputs: []string{out}}
			s := waitForEventSession(t, counter, nil, false)

			buf := make([]byte, 512)
			ctx := contextWithRawMemBuf(context.Background(), buf)
			packed := s.WaitForEvent(ctx, nil, "webhook-ingest", "await_webhook", "{}",
				`["__evt:payment"]`, 60_000, 0, 400)

			if byte(packed) == 0 && byte(packed>>8) == 0 {
				t.Fatalf("an output with no boolean \"found\" was accepted as a claim")
			}
			if s.suspendErr != nil {
				t.Error("it suspended instead of reporting the malformed output, so the " +
					"caller would wait out the whole budget for an answer that cannot come")
			}
			if n := uint32(uint64(packed) >> 40); n == 0 || !strings.Contains(string(buf[:n]), "found") {
				t.Errorf("the failure message should name the missing field; got %q", string(buf[:n]))
			}
		})
	}
}
