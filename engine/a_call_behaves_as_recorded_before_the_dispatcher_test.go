package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cleat#4152, G3: what DurableCall (cleat_call) does in each situation, pinned BEFORE the call moves onto the dispatcher, so the move reads as a diff.
//
// The call has the most paths of any host call, and five of them answer without recording the call's event (a cancelled workflow, a worker presumed
// lost, the event cap, a hard stop observed mid-call, and a failed write-ahead intent). Each row is a situation and the text is what the call did:
// the result word, the bytes it wrote, whether the session is still replaying, the step, the events it recorded, the service calls it made and the step
// each key was derived from, the suspension it set, whether the event cap's continue-as-new was triggered, and the intent store's writes.
//
// WHAT THIS TABLE IS, AND IS NOT. It is text recorded from the code, so it cannot say a result word is RIGHT, only that it did not move
// (TestG3dGoldenRowsDecodeInTheDurableCallLayout reads each word the way the guest does, which is what the heartbeat's first version of its table
// lacked). The counts DurableCall emits are pinned separately, by a_call_emits_the_same_counters_on_replay_and_fresh_before_the_dispatcher_test.go,
// which the move must pass byte-identically and does not edit.
//
// NOT COVERED, and not claimed: the resolver paths of a pending intent (this table configures no replayer, so a resolution attempt would decline either
// way and the row cannot see whether one was tried; answerRecordedCall, which the retried call shares, is exercised by the resolver tests), a real
// store's intent rows (the stub below records its writes and nothing else), and a real worker.

type g3dStub struct {
	WorkflowStore // nil: only the intent methods below are ever called on it
	writeErr      error
	completeErr   error
	wrote         int
	completed     int
}

func (s *g3dStub) WriteCallIntent(_ context.Context, _ string, _ EventRecord, _ string, _ int64) error {
	s.wrote++
	return s.writeErr
}

func (s *g3dStub) CompleteCallIntent(_ context.Context, _ string, _ EventRecord, _ []byte, _ string, _ string, _ int64) error {
	s.completed++
	return s.completeErr
}

// g3dPlainStore is a store that does not implement write-ahead intents.
type g3dPlainStore struct{ WorkflowStore }

type g3dSignals struct {
	cancelled bool
	err       error
}

func (g3dSignals) DeliverSignal(context.Context, string, string, string) error { return nil }
func (g3dSignals) PollSignal(context.Context, string, string) (SignalDelivery, bool, error) {
	return SignalDelivery{}, false, nil
}
func (g3dSignals) ConsumeSignal(context.Context, string, int64) error { return nil }
func (g *g3dSignals) PollCancellation(context.Context, string) (bool, string, error) {
	return g.cancelled, "", g.err
}

type g3dCase struct {
	history   []EventRecord
	replaying bool
	// suspended makes the session already suspended, the way a guest that did not stop at a suspension leaves it.
	suspended  bool
	deferPhase bool
	shutdown   bool
	writeFail  bool
	quit       bool
	// calls is how many DurableCalls the guest makes, in order, each of svc.op; 0 means one.
	calls int
	// failFirst makes the first n attempts at svc.op fail with a service error.
	failFirst int
	// cancelled and pollErr configure the cancellation poll.
	cancelled bool
	pollErr   bool
	cantStart bool
	// eventCap is the engine's --max-quota-events and eventCount what the session has already recorded; autoTriggered says the cap's continue-as-new
	// was already triggered once.
	eventCap      int
	eventCount    int
	autoTriggered bool
	// hardStopMidCall cancels the engine's hard-stop context from inside the service call; hardStopBefore cancels it before the call starts.
	hardStopMidCall bool
	hardStopBefore  bool
	// intent declares svc.op write-ahead; intentStore is "stub", "plain" or "" (no store); the errors fail the stub's two writes.
	intent      bool
	intentStore string
	writeErr    error
	completeErr error
}

func g3dCases() map[string]g3dCase {
	call := func(step int, op string) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeCall, Service: "svc", Op: op, Request: `{}`, Response: `{"ok":true}`}
	}
	failedCall := call(0, "op")
	failedCall.Response = ""
	failedCall.Err = "service said no"
	nonRetryable := failedCall
	nonRetryable.ErrNonRetryable = true
	pending := call(0, "op")
	pending.Response = ""
	pending.Pending = true
	return map[string]g3dCase{
		"fresh":                                      {},
		"fresh_service_error":                        {failFirst: 1},
		"fresh_cancelled":                            {cancelled: true},
		"fresh_cancel_poll_fails_open":               {pollErr: true},
		"fresh_cancelled_wins_over_cant_start":       {cancelled: true, cantStart: true},
		"fresh_cant_start_new_work":                  {cantStart: true},
		"fresh_cant_start_wins_over_the_cap":         {cantStart: true, eventCap: 1, eventCount: 1},
		"fresh_event_cap_reached":                    {eventCap: 1, eventCount: 1},
		"fresh_event_cap_already_triggered":          {eventCap: 1, eventCount: 1, autoTriggered: true},
		"fresh_below_the_event_cap":                  {eventCap: 5, eventCount: 1},
		"fresh_hard_stop_mid_call_that_fails":        {hardStopMidCall: true, failFirst: 1},
		"fresh_hard_stop_mid_call_that_succeeds":     {hardStopMidCall: true},
		"fresh_hard_stop_before_the_call":            {hardStopBefore: true},
		"fresh_in_a_defer_segment":                   {deferPhase: true},
		"fresh_after_a_shutdown":                     {shutdown: true},
		"fresh_after_a_refused_event_write":          {writeFail: true},
		"fresh_when_already_suspended":               {suspended: true},
		"intent_ok":                                  {intent: true, intentStore: "stub"},
		"intent_service_error":                       {intent: true, intentStore: "stub", failFirst: 1},
		"intent_write_fails":                         {intent: true, intentStore: "stub", writeErr: errors.New("intent write refused")},
		"intent_completion_fails":                    {intent: true, intentStore: "stub", completeErr: errors.New("intent completion refused")},
		"intent_completion_fails_then_the_next_call": {intent: true, intentStore: "stub", completeErr: errors.New("intent completion refused"), calls: 2},
		"intent_store_does_not_support_it":           {intent: true, intentStore: "plain"},
		"intent_declared_but_no_store":               {intent: true},
		"intent_is_still_refused_in_a_defer":         {intent: true, intentStore: "stub", deferPhase: true},
		"replay_a_recorded_call":                     {replaying: true, history: []EventRecord{call(0, "op")}},
		"replay_a_recorded_error":                    {replaying: true, history: []EventRecord{failedCall}},
		"replay_a_recorded_non_retryable_error":      {replaying: true, history: []EventRecord{nonRetryable}},
		"replay_a_pending_intent":                    {replaying: true, history: []EventRecord{pending}},
		"replay_other_event_type":                    {replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op"}}},
		"replay_other_operation":                     {replaying: true, history: []EventRecord{call(0, "another")}},
		"replay_other_service":                       {replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "elsewhere", Op: "op", Response: `{}`}}},
		"replay_quit":                                {replaying: true, quit: true, history: []EventRecord{call(0, "op")}},
		"replay_frontier_empty_history":              {replaying: true},
		"replay_one_then_the_frontier":               {replaying: true, history: []EventRecord{call(0, "op")}, calls: 2},
		"replay_divergence_then_a_call":              {replaying: true, history: []EventRecord{call(0, "another")}, calls: 2},
		"replay_frontier_in_a_defer_segment":         {replaying: true, deferPhase: true},
		"replay_frontier_after_a_refused_write":      {replaying: true, writeFail: true},
		"replay_frontier_when_suspended":             {replaying: true, suspended: true},
		"replay_a_recorded_call_when_suspended":      {replaying: true, suspended: true, history: []EventRecord{call(0, "op")}},
		"replay_a_recorded_call_in_a_defer":          {replaying: true, deferPhase: true, history: []EventRecord{call(0, "op")}},
		"replay_frontier_at_the_event_cap":           {replaying: true, eventCap: 1, eventCount: 1},
		"replay_frontier_then_the_event_cap_hits":    {replaying: true, history: []EventRecord{call(0, "op")}, calls: 2, eventCap: 1, eventCount: 1},
	}
}

const g3dBufLen = 4096

func (c g3dCase) observe() string {
	caller := &g3rCaller{failFirst: c.failFirst}
	caller.learnKeys("wf-g3d")
	opts := []EngineOption{WithWorkflowID("wf-g3d")}
	if c.deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	if c.shutdown {
		down := make(chan struct{})
		close(down)
		opts = append(opts, WithShutdownSignal(down))
	}
	if c.cantStart {
		opts = append(opts, WithCanStartNewWork(func() bool { return false }))
	}
	if c.eventCap > 0 {
		opts = append(opts, WithMaxQuotaEvents(c.eventCap))
	}
	if c.cancelled || c.pollErr {
		sig := &g3dSignals{cancelled: c.cancelled}
		if c.pollErr {
			sig.err = errors.New("database blip")
		}
		opts = append(opts, WithSignalStore(sig))
	}
	hardStop, stopNow := context.WithCancel(context.Background())
	defer stopNow()
	if c.hardStopMidCall || c.hardStopBefore {
		opts = append(opts, WithHardStopSignal(hardStop))
		if c.hardStopBefore {
			stopNow()
		}
		if c.hardStopMidCall {
			caller.onAttempt = func(int) { stopNow() }
		}
	}
	stub := &g3dStub{writeErr: c.writeErr, completeErr: c.completeErr}
	if c.intent {
		opts = append(opts, WithWriteAheadIntentOps("svc.op"))
		switch c.intentStore {
		case "stub":
			opts = append(opts, WithWorkflowStore(stub))
		case "plain":
			opts = append(opts, WithWorkflowStore(&g3dPlainStore{}))
		}
	}
	eng := NewEngine(nil, caller, opts...)
	s := &execSession{
		engine: eng, workflowID: "wf-g3d", defName: "def-g3d", nowMs: time.Now().UnixMilli(), originalInput: `{}`,
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: c.replaying, history: append([]EventRecord(nil), c.history...), eventCount: c.eventCount,
		autoContinueAsNewTriggered: c.autoTriggered,
	}
	if c.writeFail {
		s.eventWriteFailure = errors.New("event write refused")
	}
	if c.suspended {
		s.suspendErr = &SuspendError{Reason: "earlier"}
	}
	if c.quit {
		s.stepCallback = func(int, *EventRecord, map[string]string) ReplayStepAction { return ReplayQuit }
	}
	n := c.calls
	if n == 0 {
		n = 1
	}
	before := len(s.history)
	var words, wrote []string
	var panicked string
	func() {
		// A row that panics is a row (cleat#4244): the table records the defect and is not aborted by it.
		defer func() {
			if r := recover(); r != nil {
				panicked = fmt.Sprint(r)
			}
		}()
		for i := 0; i < n; i++ {
			buf := make([]byte, g3dBufLen)
			ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
			res := s.DurableCall(ctx, nil, "svc", "op", `{}`, 0, g3dBufLen)
			k := 0
			for k < len(buf) && buf[k] != 0 {
				k++
			}
			words = append(words, fmt.Sprintf("%#x", res))
			wrote = append(wrote, string(buf[:k]))
		}
	}()
	susp := "none"
	if s.suspendErr != nil {
		susp = s.suspendErr.Reason
	}
	out := fmt.Sprintf("res=%v wrote=%q isReplay=%v step=%d recorded=%s attempts=%d others=%d keysFromStep=%v suspension=%s autoCAN=%v eventCount=%d intent=%d/%d",
		words, wrote, s.isReplay, s.stepCount, g3rEvents(s.history, before), caller.attempts, caller.others, caller.stepsOf("wf-g3d"), susp,
		s.autoContinueAsNewTriggered, s.eventCount, stub.wrote, stub.completed)
	if panicked != "" {
		out = "PANIC=" + fmt.Sprintf("%q", panicked) + " " + out
	}
	// A spec that did not do what it declared is part of what the call did, and is appended ONLY when there is one, so a row that has none reads exactly
	// as it did before the call moved.
	if len(s.stepViolations) > 0 {
		out += fmt.Sprintf(" SPEC-VIOLATIONS=%q", s.stepViolations)
	}
	return out
}

func TestG3dDurableCallBehavesAsRecordedBeforeTheDispatcher(t *testing.T) {
	cases := g3dCases()
	if len(cases) != len(g3dGolden) {
		t.Fatalf("PRECONDITION: %d situations but %d recorded rows", len(cases), len(g3dGolden))
	}
	for name, c := range cases {
		want, ok := g3dGolden[name]
		if !ok {
			t.Errorf("%s: no recorded row", name)
			continue
		}
		if got := c.observe(); got != want {
			t.Errorf("%s changed:\n  was: %s\n  now: %s", name, want, got)
		}
	}
}

var g3dGolden = map[string]string{
	"fresh":                                      "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"fresh_after_a_refused_event_write":          "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"fresh_after_a_shutdown":                     "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"fresh_below_the_event_cap":                  "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=2 intent=0/0",
	"fresh_cancel_poll_fails_open":               "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"fresh_cancelled":                            "res=[0x120000000001] wrote=[\"workflow cancelled\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"fresh_cancelled_wins_over_cant_start":       "res=[0x120000000001] wrote=[\"workflow cancelled\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"fresh_cant_start_new_work":                  "res=[0x4c0000000201] wrote=[\"worker heartbeat presumed lost; refusing to start new work until it recovers\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"fresh_cant_start_wins_over_the_cap":         "res=[0x4c0000000201] wrote=[\"worker heartbeat presumed lost; refusing to start new work until it recovers\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"fresh_event_cap_already_triggered":          "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=true eventCount=2 intent=0/0",
	"fresh_event_cap_reached":                    "res=[0x2d0000000001] wrote=[\"event cap reached; workflow continuing as new\"] isReplay=false step=1 recorded=[continue_as_new/step=0/attempt=0/./err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=0 others=0 keysFromStep=[] suspension=continue_as_new autoCAN=true eventCount=1 intent=0/0",
	"fresh_hard_stop_before_the_call":            "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"fresh_hard_stop_mid_call_that_fails":        "res=[0x2f0000000201] wrote=[\"worker shutting down; this call was interrupted\"] isReplay=false step=0 recorded=[] attempts=1 others=0 keysFromStep=[0] suspension=worker shutdown autoCAN=false eventCount=1 intent=0/0",
	"fresh_hard_stop_mid_call_that_succeeds":     "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"fresh_in_a_defer_segment":                   "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"fresh_service_error":                        "res=[0x180000000201] wrote=[\"TRY_AGAIN: 503 try again\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"fresh_when_already_suspended":               "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=earlier autoCAN=false eventCount=0 intent=0/0",
	"intent_completion_fails":                    "res=[0x4d0000000201] wrote=[\"completing write-ahead intent for svc.op at step 0: intent completion refused\"] isReplay=false step=0 recorded=[] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=1/1",
	"intent_completion_fails_then_the_next_call": "res=[0x4d0000000201 0x4d0000000201] wrote=[\"completing write-ahead intent for svc.op at step 0: intent completion refused\" \"completing write-ahead intent for svc.op at step 0: intent completion refused\"] isReplay=false step=0 recorded=[] attempts=2 others=0 keysFromStep=[0 0] suspension=none autoCAN=false eventCount=2 intent=2/2",
	"intent_declared_but_no_store":               "res=[0x600000000201] wrote=[\"call svc.op is declared write-ahead-intent but this engine cannot honour it: no store configured\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"intent_is_still_refused_in_a_defer":         "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"intent_ok":                                  "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=1/1",
	"intent_service_error":                       "res=[0x180000000201] wrote=[\"TRY_AGAIN: 503 try again\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=1/1",
	"intent_store_does_not_support_it":           "res=[0x930000000201] wrote=[\"call svc.op is declared write-ahead-intent but this engine cannot honour it: store *engine.g3dPlainStore does not implement write-ahead call intent\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"intent_write_fails":                         "res=[0x3d0000000201] wrote=[\"write-ahead intent for svc.op at step 0: intent write refused\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=1 intent=1/0",
	"replay_a_pending_intent":                    "res=[0xb40000000001] wrote=[\"[AMBIGUOUS] call outcome unknown at step 0: the external call to svc.op was dispatched but the response was not recorded before a crash. Check the external service before retrying.\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_a_recorded_call":                     "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_a_recorded_call_in_a_defer":          "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_a_recorded_call_when_suspended":      "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=earlier autoCAN=false eventCount=0 intent=0/0",
	"replay_a_recorded_error":                    "res=[0xf0000000201] wrote=[\"service said no\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_a_recorded_non_retryable_error":      "res=[0xf0000000001] wrote=[\"service said no\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_divergence_then_a_call":              "res=[0x1050000000001 0xb0000000000] wrote=[\"replay divergence at step 0: workflow called svc.op but history has svc.another.\\n  actual request: {}\\n  expected request: {}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" \"{\\\"ok\\\":true}\"] isReplay=false step=2 recorded=[call/step=1/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[1] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"replay_frontier_after_a_refused_write":      "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_frontier_at_the_event_cap":           "res=[0x2d0000000001] wrote=[\"event cap reached; workflow continuing as new\"] isReplay=false step=1 recorded=[continue_as_new/step=0/attempt=0/./err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=0 others=0 keysFromStep=[] suspension=continue_as_new autoCAN=true eventCount=1 intent=0/0",
	"replay_frontier_empty_history":              "res=[0xb0000000000] wrote=[\"{\\\"ok\\\":true}\"] isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"replay_frontier_in_a_defer_segment":         "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_frontier_then_the_event_cap_hits":    "res=[0xb0000000000 0x2d0000000001] wrote=[\"{\\\"ok\\\":true}\" \"event cap reached; workflow continuing as new\"] isReplay=false step=2 recorded=[continue_as_new/step=1/attempt=0/./err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=0 others=0 keysFromStep=[] suspension=continue_as_new autoCAN=true eventCount=1 intent=0/0",
	"replay_frontier_when_suspended":             "res=[0x80000000] wrote=[\"\"] isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=earlier autoCAN=false eventCount=0 intent=0/0",
	"replay_one_then_the_frontier":               "res=[0xb0000000000 0xb0000000000] wrote=[\"{\\\"ok\\\":true}\" \"{\\\"ok\\\":true}\"] isReplay=false step=2 recorded=[call/step=1/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[1] suspension=none autoCAN=false eventCount=1 intent=0/0",
	"replay_other_event_type":                    "res=[0xfd0000000001] wrote=[\"replay divergence at step 0: expected call event, got call_attempt_failed.\\n  actual request: {}\\n  expected request: \\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_other_operation":                     "res=[0x1050000000001] wrote=[\"replay divergence at step 0: workflow called svc.op but history has svc.another.\\n  actual request: {}\\n  expected request: {}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_other_service":                       "res=[0x1040000000001] wrote=[\"replay divergence at step 0: workflow called svc.op but history has elsewhere.op.\\n  actual request: {}\\n  expected request: \\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
	"replay_quit":                                "res=[0x0] wrote=[\"\"] isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[] suspension=none autoCAN=false eventCount=0 intent=0/0",
}

// g3dRowProblem reads a row's result words the way the GUEST does (decodeDurableCallResult, copied from the generated adapter) and requires each decoded
// length to be the number of bytes the row says that call wrote: true of every word only if it is in the durable-call layout. It returns "" for a row
// that agrees, "UNMEASURED: ..." for one it cannot read (never a pass), or what disagrees. A stop word (0x80000000) writes nothing and decodes to 0.
var g3dRowShape = regexp.MustCompile(`res=\[([^\]]*)\] wrote=\[((?:"(?:[^"\\]|\\.)*"|\s)*)\] isReplay=`)
var g3dQuoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func g3dRowProblem(row string) string {
	m := g3dRowShape.FindStringSubmatch(row)
	if m == nil {
		return "UNMEASURED: the row is not readable as res=[...] wrote=[...]: " + row[:min(len(row), 80)]
	}
	words := strings.Fields(m[1])
	wrote := g3dQuoted.FindAllString(m[2], -1)
	if len(words) != len(wrote) {
		return fmt.Sprintf("UNMEASURED: %d result words but %d written texts", len(words), len(wrote))
	}
	for i, w := range words {
		word, err := strconv.ParseUint(strings.TrimPrefix(w, "0x"), 16, 64)
		if err != nil {
			return "UNMEASURED: the result word " + w + " does not parse"
		}
		text, err := strconv.Unquote(wrote[i])
		if err != nil {
			return "UNMEASURED: the written text does not unquote: " + wrote[i]
		}
		if length, _, _ := decodeDurableCallResult(int64(word)); int(length) != len(text) {
			return fmt.Sprintf("call %d: the guest's durable-call decoder reads a length of %d from %s, but the row says %d bytes were written", i+1, length, w, len(text))
		}
	}
	return ""
}

func TestG3dGoldenRowsDecodeInTheDurableCallLayout(t *testing.T) {
	withMessage := 0
	for name, golden := range g3dGolden {
		if p := g3dRowProblem(golden); p != "" {
			t.Errorf("%s: %s", name, p)
		}
		if !strings.Contains(golden, `wrote=[""]`) {
			withMessage++
		}
	}
	if withMessage < 20 {
		t.Fatalf("PRECONDITION FAILED: only %d of %d rows carry a message; the check reads too little to mean anything", withMessage, len(g3dGolden))
	}
}

// CONTROL: the row check refuses a row whose word is in the SIMPLE layout (the length at bit 32, which is what the dispatcher's own reporter writes
// before a spec re-packs it), refuses an unreadable row as UNMEASURED, and accepts the same message in the durable-call layout. Without it the test above
// could pass for any layout, or for rows it cannot read.
func TestG3dRowCheckRefusesARowInTheWrongLayoutAndAcceptsTheRightOne(t *testing.T) {
	msg := "workflow cancelled"
	durable := fmt.Sprintf("res=[%#x] wrote=[%q] isReplay=false", packDurableCallResult(len(msg), callErrorUnknown, 1), msg)
	simple := fmt.Sprintf("res=[%#x] wrote=[%q] isReplay=false", packSimpleResult(1, uint32(len(msg))), msg)
	if p := g3dRowProblem(durable); p != "" {
		t.Errorf("a row in the durable-call layout was refused: %s", p)
	}
	if p := g3dRowProblem(simple); p == "" || strings.HasPrefix(p, "UNMEASURED") {
		t.Errorf("a row in the simple layout was %q, want a disagreement", p)
	}
	if p := g3dRowProblem("not a row"); !strings.HasPrefix(p, "UNMEASURED") {
		t.Errorf("an unreadable row was %q, want UNMEASURED", p)
	}
}
