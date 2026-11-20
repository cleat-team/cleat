package engine

// cleat#4157. A host call's replay half looks at history[stepCount]. If that event is its own it answers from it. If it is some OTHER event
// and history still continues, the original run reached a later event without recording this call, and the call must say so (a replay
// divergence) rather than leave replay: leaving replay runs the REST of the history fresh, so a durable call already recorded after this step
// executes again (a duplicate side effect) and recordEvent appends at a step that already has an event.
//
// This complements a_replay_to_fresh_hand_off_refuses_or_is_named_test.go (cleat#4143). That one asks whether every hand-off reaches a
// refusal, from the source text. This one asks whether every hand-off is reached ONLY at the frontier, by running the call. The second cannot
// be read off the text: a census keyed on `exitReplay()` finds every site and cannot tell a frontier-only one from one reached with history
// unread, which is why the first draft of the sweep (42 sites) classified by reading and this one measures.
//
// IT RUNS EVERY HostHandler METHOD, from the interface, so a host call added later is probed without anyone remembering to list it. Each is
// called with a foreign event at step 0 and an unrelated durable call recorded after it, for every EventType, and must not leave replay
// with the second unread, append to the history, or run the recorded call.

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// allEventTypes reads the EventType constants from the source rather than listing them, so a new event type is probed too.
func allEventTypes(t *testing.T) []EventType {
	t.Helper()
	src, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []EventType
	for _, m := range regexp.MustCompile(`(?m)^\s*EventType\w+\s+EventType\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		out = append(out, EventType(m[1]))
	}
	if len(out) < 30 {
		t.Fatalf("found %d EventType constants in types.go; the scan is reading the wrong thing", len(out))
	}
	return out
}

// resumesItsOwnPendingEvent are (call, event type) pairs that leave replay with history unread, and are EXEMPTED FROM THIS GUARD BY READING,
// NOT BY MEASUREMENT. The reason given for the exemption was that the call is resuming the pending await it recorded itself, which is the
// history's last event when a run is resumed, so the fresh path records the outcome at the same step and overwrites the empty event. THAT REASON
// IS FALSE FOR AwaitPromise: measured on the real Go guest (cleat#4177), the outcome is recorded at the NEXT step, and a timed-out await records
// nothing, so a later replay leaves replay at the await and a recorded call after it runs again (2 calls, want 1). The entry stays only because
// removing it fails the probe for the right reason, and cleat#4177 owns the fix; when it lands, delete the entry and let the guard cover it.
//
// The other pending-resume sites this guard's neighbours exempt (AwaitChild, AwaitAnyChild, replayAwaitAllChildren) are
// NOT MEASURED either: whether a defer segment's drain can append events after the pending record has not been tried. Each is a grant, and an
// entry added to silence a failure is the next duplicate effect.
var resumesItsOwnPendingEvent = map[string]EventType{
	"AwaitPromise": EventTypeAwaitPromise,
}

// probeArgs builds arguments for a HostHandler method by parameter type: strings are "x", a pointer is 0 followed by a 256-byte limit.
func probeArgs(mt reflect.Type, ctx context.Context) []reflect.Value {
	args := make([]reflect.Value, mt.NumIn())
	for i := range args {
		pt := mt.In(i)
		switch {
		case pt == reflect.TypeOf((*context.Context)(nil)).Elem():
			args[i] = reflect.ValueOf(ctx)
		case pt == reflect.TypeOf((*api.Module)(nil)).Elem():
			args[i] = reflect.Zero(pt)
		case pt.Kind() == reflect.String:
			args[i] = reflect.ValueOf("x")
		case pt.Kind() == reflect.Uint32 && i > 0 && mt.In(i-1).Kind() == reflect.Uint32:
			args[i] = reflect.ValueOf(uint32(256))
		default:
			args[i] = reflect.Zero(pt)
		}
	}
	return args
}

// foreignReplay returns a session replaying [foreign event, a recorded durable call].
func foreignReplay(et EventType) (*execSession, *mockCaller) {
	c := &mockCaller{}
	return &execSession{
		engine: NewEngine(nil, c, WithWorkflowID("wf-4157")), workflowID: "wf-4157", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{}, isReplay: true,
		history: []EventRecord{
			{Step: 0, EventType: et, Service: "work", Op: "charge", Request: `{}`, Response: `{"ok":true}`},
			{Step: 1, EventType: EventTypeCall, Service: "work", Op: "charge", Request: `{}`, Response: `{"ok":true}`},
		},
	}, c
}

// leftReplayEarly is the defect: out of replay while recorded history is still unread.
func leftReplayEarly(s *execSession) bool { return !s.isReplay && s.stepCount < len(s.history) }

// A call that always records an event must answer a foreign event with a divergence and leave the session replaying at the same step.
var mustDivergeOnAForeignEvent = []string{
	"ContinueAsNew", "ContinueAsNewWithVersion", "RegisterUpdateHandler", "CreatePromise", "DurableDefer",
	"ResolvePromise", "RejectPromise", "SignalWorkflow", "DurableCompleteUpdate", "DurableAwaitSignals",
}

func TestNoHostCallLeavesReplayWithHistoryUnread(t *testing.T) {
	ht := reflect.TypeOf((*HostHandler)(nil)).Elem()
	var names []string
	for i := 0; i < ht.NumMethod(); i++ {
		names = append(names, ht.Method(i).Name)
	}
	sort.Strings(names)
	types := allEventTypes(t)

	probed := map[string]int{}
	for _, name := range names {
		for _, et := range types {
			if own, ok := resumesItsOwnPendingEvent[name]; ok && own == et {
				continue
			}
			s, c := foreignReplay(et)
			m := reflect.ValueOf(s).MethodByName(name)
			if !m.IsValid() {
				t.Fatalf("HostHandler declares %s and *execSession has no such method; the probe is reading the wrong thing", name)
			}
			args := probeArgs(m.Type(), hostCtx())
			if name == "DurableAwaitSignals" {
				args[3] = reflect.ValueOf(int64(1000)) // a wait; the poll with no wait is probed below
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s with a %s event at step 0 PANICKED the host: %v", name, et, r)
					}
				}()
				m.Call(args)
			}()
			probed[name]++
			if leftReplayEarly(s) {
				t.Errorf("%s replayed against a %q event with a recorded call still unread LEFT REPLAY (stepCount=%d of %d): the rest of the history "+
					"would run fresh and the recorded call would execute again. Report a replay divergence (replayDivergence) when the call always "+
					"records, or name the call in resumesItsOwnPendingEvent with the reason.", name, et, s.stepCount, len(s.history))
			}
			if len(s.history) != 2 {
				t.Errorf("%s replayed against a %q event appended %d event(s) to a history it was replaying", name, et, len(s.history)-2)
			}
			if len(c.calls) != 0 {
				t.Errorf("%s replayed against a %q event ran a service call: %+v", name, et, c.calls)
			}
		}
	}

	// Vacuity, per call and not as a total: a probe that returns before it reads history passes whatever the tree says.
	if len(names) < 40 {
		t.Fatalf("probed only %d host calls; HostHandler is being read wrong", len(names))
	}
	for _, name := range mustDivergeOnAForeignEvent {
		if probed[name] == 0 {
			t.Fatalf("%s was not probed", name)
		}
		s, _ := foreignReplay(EventTypeCall)
		m := reflect.ValueOf(s).MethodByName(name)
		args := probeArgs(m.Type(), hostCtx())
		if name == "DurableAwaitSignals" {
			args[3] = reflect.ValueOf(int64(1000))
		}
		got := m.Call(args)[0].Int()
		if got&0xFF != 1 {
			t.Errorf("%s against a foreign event returned %#x, want a replay divergence (error code 1). A call that does not look at the history "+
				"here is not being probed, and a pass from it is empty.", name, got)
		}
		if !s.isReplay || s.stepCount != 0 {
			t.Errorf("%s diverged but left isReplay=%v stepCount=%d, want a session still replaying at step 0 so the next call reads its own record", name, s.isReplay, s.stepCount)
		}
	}
}

// The detector itself. A probe whose own test was never shown to fail is a claim: this is the shape of the defect, run through leftReplayEarly.
func TestTheEarlyExitDetectorSeesTheDefect(t *testing.T) {
	s, _ := foreignReplay(EventTypeCall)
	if leftReplayEarly(s) {
		t.Fatal("a session that has not left replay was reported as having left it")
	}
	s.exitReplay() // what every fixed call used to do at a foreign event
	if !leftReplayEarly(s) {
		t.Fatal("a session that called exitReplay() with history unread was not reported: the detector would pass the defect")
	}
	s.stepCount = len(s.history) // at the frontier, leaving replay is correct
	if leftReplayEarly(s) {
		t.Fatal("a session that left replay at the frontier was reported as having left it early")
	}
}

// TestAReplayedSignalSendDoesNotRunTheCallRecordedAfterItAgain is the measurement cleat#4157 asks for, on a history the ORIGINAL run produced.
//
// In the original run the signal send is refused for authorization (the target is not one this workflow may signal) and the guest, told so,
// goes on to make a durable call. On v0.5.0 a refused send records NOTHING, so the history is [call]: when the next execution replays, the
// send finds the CALL at its step, used to leave replay, and sent fresh with history unread -- after which the recorded call EXECUTED AGAIN
// (a second charge). The signal's authorization has since been granted, which is what makes the replay's send succeed where the original's was
// refused: the replay must not be a different run.
//
// Once cleat#4102 gives every send a recorded outcome the original run's history has the send's event too, and the same assertions hold from
// the record alone; the test is written to be right in both worlds.
func TestAReplayedSignalSendDoesNotRunTheCallRecordedAfterItAgain(t *testing.T) {
	refuse := func(context.Context, string, string) error { return os.ErrPermission }
	allow := func(context.Context, string, string) error { return nil }
	send := func(s *execSession) int64 { return s.SignalWorkflow(hostCtx(), nil, "run-target", "go", `{}`) }
	call := func(s *execSession) int64 { return s.DurableCall(hostCtx(), nil, "work", "charge", `{}`, 0, 256) }

	orig, origCaller := originalRunWith(WithRequireSignalAuth(true), WithSignalAuthCheck(refuse))
	if got := send(orig); got == 0 {
		t.Fatal("PRECONDITION FAILED: the original send was not refused, so this is not the run the test describes")
	}
	call(orig)
	if len(origCaller.calls) != 1 {
		t.Fatalf("PRECONDITION FAILED: the original run made %d service calls, want 1", len(origCaller.calls))
	}
	hist := append([]EventRecord(nil), orig.history...)

	p := replayOfWith(hist, WithRequireSignalAuth(true), WithSignalAuthCheck(allow))
	send(p.s)
	if !p.s.isReplay || p.s.stepCount > len(hist)-1 {
		// After the send the session must still be replaying with the recorded call unread (or, once the send records its own event, having
		// consumed exactly that).
		t.Errorf("after the replayed send isReplay=%v stepCount=%d of %d: the rest of the history would run fresh", p.s.isReplay, p.s.stepCount, len(hist))
	}
	call(p.s)
	if len(p.caller.calls) != 0 {
		t.Errorf("the call recorded after the send EXECUTED AGAIN on replay: %+v", p.caller.calls)
	}
	if len(p.s.history) != len(hist) {
		t.Fatalf("the replay appended %d event(s) to a history it was replaying", len(p.s.history)-len(hist))
	}
}

func originalRunWith(opts ...EngineOption) (*execSession, *mockCaller) {
	c := &mockCaller{}
	opts = append([]EngineOption{WithWorkflowID("wf-4157")}, opts...)
	return &execSession{
		engine: NewEngine(nil, c, opts...), workflowID: "wf-4157", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
	}, c
}

type replayWith struct {
	s      *execSession
	caller *mockCaller
}

func replayOfWith(hist []EventRecord, opts ...EngineOption) replayWith {
	c := &mockCaller{}
	opts = append([]EngineOption{WithWorkflowID("wf-4157")}, opts...)
	return replayWith{s: &execSession{
		engine: NewEngine(nil, c, opts...), workflowID: "wf-4157", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{}, isReplay: true, history: hist,
	}, caller: c}
}

// wantReplayDivergence is the contract for a call that always records, replayed against a foreign event: error code 1, still replaying at the
// same step, nothing appended. cleat#4157; these assertions were the opposite (isReplay=false and a new event appended at a colliding step).
func wantReplayDivergence(t *testing.T, s *execSession, result int64, histLen int) {
	t.Helper()
	if result&0xFF != 1 {
		t.Errorf("a foreign event at the step returned %#x, want a replay divergence (error code 1)", result)
	}
	if !s.isReplay || s.stepCount != 0 {
		t.Errorf("isReplay=%v stepCount=%d after a divergence, want a session still replaying at step 0", s.isReplay, s.stepCount)
	}
	if len(s.history) != histLen {
		t.Errorf("history has %d event(s), want %d: a divergence appends nothing", len(s.history), histLen)
	}
}

// A history written BEFORE cleat#4151 recorded nothing for a no-wait await_signals that timed out, so at such a call the event at the step is the
// NEXT call's. That is not a divergence: the outcome was deterministic, so the replay answers "timed out", consumes nothing, stays a replay,
// and the recorded call after it is served from history. A poll that WAITS at a foreign event is a real divergence (the probe above).
func TestALegacyNoWaitSignalPollReplaysAsTheTimeoutWithoutLeavingReplay(t *testing.T) {
	hist := []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "work", Op: "charge", Request: `{}`, Response: `{"ok":true}`}}
	p := replayOfWith(hist)
	got := p.s.DurableAwaitSignals(hostCtx(), nil, `["go"]`, 0, 0, 200, 256, 200)
	if (got>>16)&0xFFFF == 0 || got&0xFFFF != 0 {
		t.Fatalf("the replayed no-wait poll returned %#x, want the timed-out word with no error", got)
	}
	if !p.s.isReplay || p.s.stepCount != 0 || len(p.s.history) != 1 {
		t.Fatalf("after the poll isReplay=%v stepCount=%d history=%d, want a session still replaying at step 0 with nothing appended",
			p.s.isReplay, p.s.stepCount, len(p.s.history))
	}
	p.s.DurableCall(hostCtx(), nil, "work", "charge", `{}`, 0, 256)
	if len(p.caller.calls) != 0 {
		t.Fatalf("the call recorded after the poll EXECUTED AGAIN on replay: %+v", p.caller.calls)
	}
}
