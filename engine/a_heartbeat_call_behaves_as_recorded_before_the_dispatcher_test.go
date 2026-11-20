package engine

// cleat#4152, group G3 (Multi): a characterization of DurableCallWithHeartbeat, RECORDED FROM THE CODE AS IT WAS BEFORE IT WENT THROUGH step() (no
// production change in this file's PR). It is the sibling of a_retried_call_behaves_as_recorded_before_the_dispatcher_test.go and reads the same way:
// one string per situation, holding the packed result, the bytes written to the guest, whether the session is still replaying, the step count, the
// events the call RECORDED, and what reached the outside world (calls to svc.op, and the step whose idempotency key each carried).
//
// THE TICKER IS NOT MEASURED. The call starts a goroutine and a time.Ticker and records one heartbeat event per tick, so how many heartbeats a
// call leaves behind depends on how long the call ran. The table does not assert that: most rows use an interval of an hour, so no tick fires, and
// the one row that must tick (the cancelled call) normalizes the run of newly recorded heartbeats to a single token and does not print the step
// numbers the count would have shifted. What each row can say about heartbeats is that there were some or there were none.
//
// What a row cannot say: the timestamp of a recorded event, the telemetry span, and anything the wasm backends add around the call.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const g3hBufLen = 1024

// g3hCaller answers svc.op according to its mode and records the idempotency key each call carried, raw (stepsOf turns them into steps). It
// implements IdempotentCaller. It is self-contained on purpose: the retry table (a sibling PR) has a caller of the same shape, and the two files
// must not depend on each other, so each carries its own.
type g3hCaller struct {
	mode     string // "ok", "error" or "block" (until the call's context is cancelled)
	attempts int    // calls to svc.op
	others   int    // every other call
	keys     []string
}

// stepsOf maps each recorded key to the step it was derived from (steps 0..9; -1 for a key that matches none). The key for each step is learned from
// the engine's own call path (callService) and NOT by calling DurableCallIdempotencyKey from this file: that direct call made golangci-lint's gosec
// G701 report an unrelated line of production code (engine/mysql_lifecycle.go) that it does not report without it.
func (c *g3hCaller) stepsOf(workflowID string) []int {
	probe := newRecordingCaller()
	ps := &execSession{engine: NewEngine(nil, probe), workflowID: workflowID}
	stepOfKey := map[string]int{}
	for n := 0; n < 10; n++ {
		_, _ = ps.callService(context.Background(), "probe", "probe", `{}`, n)
		stepOfKey[probe.keys[n]] = n
	}
	out := make([]int, 0, len(c.keys))
	for _, k := range c.keys {
		step, ok := stepOfKey[k]
		if !ok {
			step = -1
		}
		out = append(out, step)
	}
	return out
}

func (c *g3hCaller) Call(ctx context.Context, service, operation, req string) (string, error) {
	return c.CallWithIdempotencyKey(ctx, service, operation, req, "")
}

func (c *g3hCaller) CallWithIdempotencyKey(ctx context.Context, service, operation, _ string, key string) (string, error) {
	if service != "svc" || operation != "op" {
		c.others++
		return `{"ok":true}`, nil
	}
	c.attempts++
	c.keys = append(c.keys, key)
	switch c.mode {
	case "error":
		return "", errors.New("the service is unavailable")
	case "block":
		<-ctx.Done()
		return "", ctx.Err()
	}
	return `{"ok":true}`, nil
}

type g3hCase struct {
	history    []EventRecord
	replaying  bool
	deferPhase bool
	shutdown   bool
	writeFail  bool
	quit       bool
	prefix     bool // the guest makes a plain svc.before call first, so the heartbeat call arrives at the replay frontier
	cancel     bool // a signal store that reports the workflow cancelled, so the first tick cancels the call
	mode       string
	intervalMs int64
}

func (c g3hCase) observe() string {
	caller := &g3hCaller{mode: c.mode}
	opts := []EngineOption{WithWorkflowID("wf-g3h")}
	if c.deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	if c.shutdown {
		down := make(chan struct{})
		close(down)
		opts = append(opts, WithShutdownSignal(down))
	}
	if c.cancel {
		opts = append(opts, WithSignalStore(&mockCancellationStore{cancelled: true, reason: "operator cancelled"}))
	}
	s := &execSession{
		engine: NewEngine(nil, caller, opts...), workflowID: "wf-g3h", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: c.replaying, history: append([]EventRecord(nil), c.history...),
	}
	if c.writeFail {
		s.eventWriteFailure = errors.New("event write refused")
	}
	if c.quit {
		s.stepCallback = func(int, *EventRecord, map[string]string) ReplayStepAction { return ReplayQuit }
	}
	buf := make([]byte, g3hBufLen)
	ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
	if c.prefix {
		s.DurableCall(ctx, nil, "svc", "before", `{}`, 0, g3hBufLen)
		for i := range buf {
			buf[i] = 0
		}
	}
	before := len(s.history)
	startStep := s.stepCount
	var res int64
	var panicked string
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = fmt.Sprint(r)
			}
		}()
		res = s.DurableCallWithHeartbeat(ctx, nil, "svc", "op", `{}`, c.intervalMs, 0, g3hBufLen)
	}()
	if panicked != "" {
		return fmt.Sprintf("PANIC=%q", panicked)
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	// The newly recorded events, with a run of heartbeats collapsed to one token and the step numbers that run shifts left symbolic.
	events := make([]string, 0)
	beats := 0
	for _, r := range s.history[before:] {
		if r.EventType == EventTypeHeartbeat && c.cancel {
			if beats == 0 {
				events = append(events, "heartbeat*")
			}
			beats++
			continue
		}
		step := fmt.Sprint(r.Step)
		if c.cancel && beats > 0 {
			step = fmt.Sprintf("%d+hb", startStep)
		}
		events = append(events, fmt.Sprintf("%s/step=%s/%s.%s/err=%q/nonretry=%v/code=%q", r.EventType, step, r.Service, r.Op, r.Err, r.ErrNonRetryable, r.ErrCode))
	}
	stepNow := fmt.Sprint(s.stepCount)
	if c.cancel && beats > 0 {
		stepNow = fmt.Sprintf("%d+hb+1", startStep)
	}
	return fmt.Sprintf("res=%#x wrote=%q isReplay=%v step=%s recorded=[%s] calls=%d others=%d keysFromStep=%v",
		res, string(buf[:n]), s.isReplay, stepNow, strings.Join(events, " "), caller.attempts, caller.others, caller.stepsOf("wf-g3h"))
}

const g3hHour = int64(3600 * 1000)

func g3hCases() map[string]g3hCase {
	hb := func(step int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeHeartbeat, Service: "svc", Op: "op"}
	}
	plain := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "before", Response: `{"ok":true}`}
	call := func(step int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{"ok":true}`}
	}
	cases := map[string]g3hCase{}
	cases["fresh_success"] = g3hCase{mode: "ok", intervalMs: g3hHour}
	cases["fresh_service_error"] = g3hCase{mode: "error", intervalMs: g3hHour}
	cases["fresh_interval_zero_is_refused_and_records_nothing"] = g3hCase{mode: "ok", intervalMs: 0}
	cases["fresh_interval_negative_is_refused_and_records_nothing"] = g3hCase{mode: "ok", intervalMs: -5}
	cases["fresh_cancelled_mid_call"] = g3hCase{mode: "block", cancel: true, intervalMs: 5}
	cases["segment_fresh"] = g3hCase{mode: "ok", deferPhase: true, intervalMs: g3hHour}
	cases["segment_frontier"] = g3hCase{mode: "ok", deferPhase: true, replaying: true, prefix: true, history: []EventRecord{plain}, intervalMs: g3hHour}
	cases["frontier_is_a_fresh_call"] = g3hCase{mode: "ok", replaying: true, prefix: true, history: []EventRecord{plain}, intervalMs: g3hHour}
	cases["write_failed_fresh"] = g3hCase{mode: "ok", writeFail: true, intervalMs: g3hHour}
	cases["shutdown_fresh"] = g3hCase{mode: "ok", shutdown: true, intervalMs: g3hHour}
	cases["replay_terminal_call"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{call(0)}, intervalMs: g3hHour}
	cases["replay_heartbeats_then_call"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hb(0), hb(1), call(2)}, intervalMs: g3hHour}
	// History ends INSIDE the call: heartbeats were recorded and no terminal call follows, which happens when the worker was lost while it ran.
	cases["replay_mid_heartbeat_resumes_the_call"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hb(0), hb(1)}, intervalMs: g3hHour}
	cases["replay_mid_heartbeat_after_a_plain_call"] = g3hCase{mode: "ok", replaying: true, prefix: true, history: []EventRecord{plain, hb(1), hb(2)}, intervalMs: g3hHour}
	cases["replay_empty_history"] = g3hCase{mode: "ok", replaying: true, intervalMs: g3hHour}
	errRec := call(0)
	errRec.Response, errRec.Err, errRec.ErrCode = "", "the service is unavailable", "unavailable"
	cases["replay_error_event"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{errRec}, intervalMs: g3hHour}
	nrRec := errRec
	nrRec.ErrNonRetryable, nrRec.Err = true, cancelledCallError
	cases["replay_error_event_non_retryable"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{nrRec}, intervalMs: g3hHour}
	otherType := EventRecord{Step: 0, EventType: EventTypeDurableSend, Service: "svc", Op: "op"}
	cases["replay_divergence_wrong_event_type"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{otherType}, intervalMs: g3hHour}
	otherSvc := call(0)
	otherSvc.Op = "another"
	cases["replay_divergence_other_operation"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{otherSvc}, intervalMs: g3hHour}
	cases["replay_quit"] = g3hCase{mode: "ok", replaying: true, quit: true, history: []EventRecord{call(0)}, intervalMs: g3hHour}
	return cases
}

// g3hGolden is the table recorded against the code as it was (develop 5a4d59db; re-verified unchanged on 0678b1cb, which adds the side-effect, defer and
// lock migrations and touches neither heartbeats.go nor DurableCall, the paths this table runs), one string per situation: the reference the migration is checked
// against. A row that changes is a behaviour change, and the PR that changes it says so.
//
// KNOWN-BAD ROWS, recorded so the migration moves them on purpose and not by accident:
//
//   - replay_mid_heartbeat_resumes_the_call and replay_mid_heartbeat_after_a_plain_call (cleat#4247): a history that ends inside the call resumes it
//     with an idempotency key derived from a LATER step than the first incarnation's (keysFromStep=[2] where the call started at step 0, and [3]
//     where it started at 1), because the replay consumes the heartbeats before the fresh call takes its step. The retry call keeps the first
//     incarnation's step for exactly this reason (cleat#1145); this one does not.
//   - replay_divergence_wrong_event_type and replay_divergence_other_operation end at step 1: a divergence CONSUMED the record before reporting it
//     (the #4152 defect), so the call that owns it loses it.
//
// Worth reading in the rest: fresh_interval_zero_is_refused_and_records_nothing and the negative one show the interval is refused with nothing
// recorded and no call made (cleat#4105); fresh_cancelled_mid_call is reported and recorded as a NON-retryable cancellation, and
// replay_error_event_non_retryable reads the same classification back, so the two agree; and frontier_is_a_fresh_call shows a heartbeat call that
// follows the last replayed event simply runs, with its key from its own step.
var g3hGolden = map[string]string{
	"fresh_cancelled_mid_call":                               "res=0x120000000001 wrote=\"workflow cancelled\" isReplay=false step=0+hb+1 recorded=[heartbeat* call/step=0+hb/svc.op/err=\"workflow cancelled\"/nonretry=true/code=\"cancelled\"] calls=1 others=0 keysFromStep=[0]",
	"fresh_interval_negative_is_refused_and_records_nothing": "res=0x690000000401 wrote=\"cleat: heartbeat interval must be at least 1 ms, got -5 (the host cannot tick at a non-positive interval)\" isReplay=false step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"fresh_interval_zero_is_refused_and_records_nothing":     "res=0x680000000401 wrote=\"cleat: heartbeat interval must be at least 1 ms, got 0 (the host cannot tick at a non-positive interval)\" isReplay=false step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"fresh_service_error":                                    "res=0x1a0000000201 wrote=\"the service is unavailable\" isReplay=false step=1 recorded=[call/step=0/svc.op/err=\"the service is unavailable\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[0]",
	"fresh_success":                                          "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=1 recorded=[call/step=0/svc.op/err=\"\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[0]",
	"frontier_is_a_fresh_call":                               "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=2 recorded=[call/step=1/svc.op/err=\"\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[1]",
	"replay_divergence_other_operation":                      "res=0x1050000000001 wrote=\"replay divergence at step 0: workflow called svc.op but history has svc.another.\\n  actual request: {}\\n  expected request: {}\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_divergence_wrong_event_type":                     "res=0xf60000000001 wrote=\"replay divergence at step 0: expected call event, got durable_send.\\n  actual request: {}\\n  expected request: \\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_empty_history":                                   "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=1 recorded=[call/step=0/svc.op/err=\"\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[0]",
	"replay_error_event":                                     "res=0x1a0000000201 wrote=\"the service is unavailable\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_error_event_non_retryable":                       "res=0x120000000001 wrote=\"workflow cancelled\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_heartbeats_then_call":                            "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=true step=3 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_mid_heartbeat_after_a_plain_call":                "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=4 recorded=[call/step=3/svc.op/err=\"\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[3]",
	"replay_mid_heartbeat_resumes_the_call":                  "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call/step=2/svc.op/err=\"\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[2]",
	"replay_quit":                                            "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_terminal_call":                                   "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"segment_fresh":                                          "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"segment_frontier":                                       "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"shutdown_fresh":                                         "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"write_failed_fresh":                                     "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
}

func TestG3hHeartbeatCallBehavesAsRecordedBeforeTheDispatcher(t *testing.T) {
	cases := g3hCases()
	for name, want := range g3hGolden {
		c, ok := cases[name]
		if !ok {
			t.Errorf("%s: situation no longer exercised", name)
			continue
		}
		if got := c.observe(); got != want {
			t.Errorf("%s changed:\n  was: %s\n  now: %s", name, want, got)
		}
	}
	for name := range cases {
		if _, ok := g3hGolden[name]; !ok {
			t.Errorf("%s: a situation with no golden row", name)
		}
	}
	if len(g3hGolden) != 20 {
		t.Errorf("the table has %d rows; this was recorded with 20, so a row was added or removed without saying so", len(g3hGolden))
	}
}
