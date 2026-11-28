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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const g3hBufLen = 1024

// g3hCaller answers svc.op according to its mode and records the idempotency key each call carried, raw (stepsOf turns them into steps). It
// implements IdempotentCaller. It is self-contained on purpose: the retry table (a sibling PR) has a caller of the same shape, and the two files
// must not depend on each other, so each carries its own.
type g3hCaller struct {
	mode      string         // "ok", "error" or "block" (until the call's context is cancelled)
	attempts  int            // calls to svc.op
	others    int            // every other call
	stepOfKey map[string]int // the key of each of steps 0..9, learned from the engine's own call path before the call (learnKeys)
	steps     []int          // the step each svc.op call's key was derived from; -1 for a key that matches none; the key itself is not kept
}

// learnKeys fills stepOfKey: the idempotency key for each of steps 0..9, learned from the engine's own call path (callService), so this file never calls
// DurableCallIdempotencyKey itself. The caller keeps the STEP each key came from and not the key.
//
// THAT SHAPE IS NOT A LINT FIX AND IS NOT CLAIMED TO BE ONE. An earlier version of this comment said it stopped gosec's G701 from flagging an unrelated
// production line (engine/mysql_lifecycle.go:114); that rested on single runs of each variant and did not hold: lint-go failed on a head built this way,
// and the finding fires on some runs of UNMODIFIED develop (1 of 5 fresh-cache runs, cleat#4252, the cleat#3247 mechanism). It is kept because it is a
// sufficient way to read the keys and the golden table passes with it, not because it avoids anything.
func (c *g3hCaller) learnKeys(workflowID string) {
	probe := newRecordingCaller()
	ps := &execSession{engine: NewEngine(nil, probe), workflowID: workflowID}
	c.stepOfKey = map[string]int{}
	for n := 0; n < 10; n++ {
		_, _ = ps.callService(context.Background(), "probe", "probe", `{}`, n)
		c.stepOfKey[probe.keys[n]] = n
	}
}

func (c *g3hCaller) stepsOf(string) []int { return c.steps }

func (c *g3hCaller) Call(ctx context.Context, service, operation, req string) (string, error) {
	return c.CallWithIdempotencyKey(ctx, service, operation, req, "")
}

func (c *g3hCaller) CallWithIdempotencyKey(ctx context.Context, service, operation, _ string, key string) (string, error) {
	if service != "svc" || operation != "op" {
		c.others++
		return `{"ok":true}`, nil
	}
	c.attempts++
	step, ok := c.stepOfKey[key]
	if !ok {
		step = -1
	}
	c.steps = append(c.steps, step)
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
	caller.learnKeys("wf-g3h")
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
	out := fmt.Sprintf("res=%#x wrote=%q isReplay=%v step=%s recorded=[%s] calls=%d others=%d keysFromStep=%v",
		res, string(buf[:n]), s.isReplay, stepNow, strings.Join(events, " "), caller.attempts, caller.others, caller.stepsOf("wf-g3h"))
	// A spec that did not do what it declared is part of what the call did, and is appended ONLY when there is one, so a row that has none reads
	// exactly as it did before the call moved. The interval refusals record nothing by design and must say so (RecordsNothing): without
	// this a spec that forgot to would pass every row (measured: dropping that call left the table green).
	if len(s.stepViolations) > 0 {
		out += fmt.Sprintf(" SPEC-VIOLATIONS=%q", s.stepViolations)
	}
	return out
}

const g3hHour = int64(3600 * 1000)

func g3hCases() map[string]g3hCase {
	hb := func(step int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeHeartbeat, Service: "svc", Op: "op"}
	}
	plain := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "before", Request: `{}`, Response: `{"ok":true}`}
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
	// A call record with no outcome: the call was dispatched and the worker was lost before the outcome was recorded. Reported as ambiguous. See the
	// header for what this row does NOT pin (the absence of a resolution attempt).
	pending := call(0)
	pending.Response, pending.Pending = "", true
	cases["replay_pending_intent_is_reported_ambiguous"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{pending}, intervalMs: g3hHour}
	cases["replay_quit"] = g3hCase{mode: "ok", replaying: true, quit: true, history: []EventRecord{call(0)}, intervalMs: g3hHour}
	// ROWS ADDED FOR THE MIGRATION (cleat#4152, G3): shapes the dispatcher's Role and Identity read differently from the old replay loop, measured on the
	// old code first. Heartbeats of ANOTHER operation, a foreign record after the heartbeats, and a stop, a refused write or a zero interval when the
	// history ends inside the run or at a recorded terminal call.
	hbOther := hb(0)
	hbOther.Op = "another"
	cases["replay_heartbeats_of_another_operation_then_the_call"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hbOther, call(1)}, intervalMs: g3hHour}
	cases["replay_heartbeats_then_a_foreign_record"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hb(0), hb(1), {Step: 2, EventType: EventTypeDurableSend, Service: "svc", Op: "op"}}, intervalMs: g3hHour}
	callOther := call(1)
	callOther.Op = "another"
	cases["replay_heartbeats_then_a_call_of_another_operation"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hb(0), callOther}, intervalMs: g3hHour}
	cases["replay_mid_heartbeat_of_another_operation"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hbOther}, intervalMs: g3hHour}
	errAfterBeat := errRec
	errAfterBeat.Step = 1
	cases["replay_heartbeats_then_an_error_event"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hb(0), errAfterBeat}, intervalMs: g3hHour}
	pendingAfterBeat := pending
	pendingAfterBeat.Step = 1
	cases["replay_heartbeats_then_a_pending_intent"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hb(0), pendingAfterBeat}, intervalMs: g3hHour}
	cases["segment_replay_terminal_call"] = g3hCase{mode: "ok", deferPhase: true, replaying: true, history: []EventRecord{call(0)}, intervalMs: g3hHour}
	cases["segment_mid_heartbeat"] = g3hCase{mode: "ok", deferPhase: true, replaying: true, history: []EventRecord{hb(0), hb(1)}, intervalMs: g3hHour}
	cases["write_failed_mid_heartbeat"] = g3hCase{mode: "ok", writeFail: true, replaying: true, history: []EventRecord{hb(0), hb(1)}, intervalMs: g3hHour}
	cases["shutdown_mid_heartbeat"] = g3hCase{mode: "ok", shutdown: true, replaying: true, history: []EventRecord{hb(0), hb(1)}, intervalMs: g3hHour}
	cases["replay_mid_heartbeat_interval_zero"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{hb(0)}, intervalMs: 0}
	cases["replay_terminal_call_interval_zero"] = g3hCase{mode: "ok", replaying: true, history: []EventRecord{call(0)}, intervalMs: 0}
	return cases
}

// g3hGolden is the table recorded against the code as it was (develop 5a4d59db; re-verified unchanged on 0678b1cb, which adds the side-effect, defer and
// lock migrations and touches neither heartbeats.go nor DurableCall, the paths this table runs), one string per situation: the reference the migration is checked
// against. A row that changes is a behaviour change, and the PR that changes it says so.
//
// KNOWN-BAD ROWS, recorded so the migration moves them on purpose and not by accident:
//
//   - (fixed in cleat#4247, and now pinned as correct) replay_mid_heartbeat_resumes_the_call and replay_mid_heartbeat_after_a_plain_call: a history
//     that ends inside the call resumes it with the idempotency key of the step the FIRST incarnation started at (keysFromStep=[0] and [1]). They
//     recorded [2] and [3] before the fix, a key derived from a LATER step because the replay consumed the heartbeats before the fresh call took its
//     step; the retry call keeps the first incarnation's step for the same reason (cleat#1145).
//   - (MOVED by the heartbeat call's migration onto the dispatcher, cleat#4152 G3, and now pinned as the dispatcher's) the divergence rows. The old
//     replay loop CONSUMED the heartbeats and then the record at the end of them before reporting a divergence, so the call that owns that record
//     loses it (the #4152 defect); the dispatcher reads the run first and consumes NOTHING for a divergence (cleat#4157), in the shared message plus the request lines the old message printed (cleat#4152's divergence-detail change put the request lines back: the first version of this move dropped them, and the rows below carry them).
//     Six rows moved, measured on develop 5c489fb7 against the migrated call, the other 27 byte-identical. In four the answer is the same (error
//     code 1, call-error code callErrorUnknown, in the durable-call layout) and the message text and the step differ:
//     replay_divergence_wrong_event_type                         step 1 -> 0
//     replay_divergence_other_operation                          step 1 -> 0
//     replay_heartbeats_then_a_foreign_record (added)            step 3 -> 0: the record after the heartbeats was swallowed with them
//     replay_heartbeats_then_a_call_of_another_operation (added) step 2 -> 0
//     and TWO the old table could not see, because the old loop never compared a heartbeat's service or operation:
//     replay_heartbeats_of_another_operation_then_the_call (added)  OLD: the heartbeats of ANOTHER operation were accepted and the recorded
//     result answered. NEW: a divergence naming the heartbeat.
//     replay_mid_heartbeat_of_another_operation (added)              OLD: the heartbeats of another operation were consumed and the call was
//     RESUMED, performing a real service call (calls=1). NEW: a divergence,
//     no call. This is cleat#4211's shape: a record of the right type accepted
//     without asking whether it names THIS call.
//
// THE HEX IN THESE ROWS DOES NOT PIN THE LAYOUT, and the layout was wrong once on the way here. The dispatcher's reporter answers in the SIMPLE layout
// (the message length at bit 32); this call's guest decodes the DURABLE-CALL layout (length at bits 40-63, call-error code at bits 8-39), so the first
// version of the move answered every divergence in a layout the guest reads as a garbage call-error code and a one-byte message. I regenerated this table
// from that output and labelled the changed rows "message shape". This table compares TEXT: it records the word the code produced, and a table
// regenerated from wrong output records the wrong word, so it cannot say the word is right. What caught the mistake was heartbeats_test.go, which
// asserts the call-error code. What says it independently now is TestG3hGoldenRowsDecodeInTheDurableCallLayout below, which decodes every row's word with the
// guest adapter's own decoder and requires the length it gets to be the number of bytes the row says were written, and the registry property of cleat#4297,
// which checks the spec's declared Result against ABI.md. The spec declares Result: layoutDurableCall, and a mutant that declares it simple turns every
// divergence row here red. (That mutant does NOT fail the decode test, which reads these rows and not the live code: it catches a table recorded in the
// wrong layout, and the rows catch a code that changed.)
//
// replay_pending_intent_is_reported_ambiguous: a call record with no outcome (dispatched, the worker lost before the outcome was recorded) is reported
// to the guest as ambiguous, with no service call and replay left on (cleat-review2, on #4248). WHAT THIS ROW DOES NOT PIN, and an earlier name for
// it claimed to: that the heartbeat call makes no attempt to RESOLVE the record. replayCall tries idempotency-key replay and the configured resolver
// first; the heartbeat call does not. This table configures no replayer, so a resolution attempt would decline either way and the row cannot see the
// difference (a mutant that makes the heartbeat call try key-replay leaves it green). Pinning that needs a configured replayer, a declared operation
// and a store that reports its own clock, which is the migration PR's real-store round trip; until then the difference is established by reading
// heartbeats.go (no call to resolveAmbiguityViaKeyReplay) and not by this table.
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
	"replay_divergence_other_operation":                      "res=0x1940000000001 wrote=\"replay divergence at step 0: call_with_heartbeat asked for something other than the call recorded there: call svc.op, recorded svc.another.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_divergence_wrong_event_type":                     "res=0x1460000000001 wrote=\"replay divergence at step 0: expected call, got durable_send.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 0 bytes, sha256=e3b0c44298fc1c14]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_empty_history":                                   "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=1 recorded=[call/step=0/svc.op/err=\"\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[0]",
	"replay_error_event":                                     "res=0x1a0000000201 wrote=\"the service is unavailable\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_error_event_non_retryable":                       "res=0x120000000001 wrote=\"workflow cancelled\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_heartbeats_of_another_operation_then_the_call":   "res=0x1990000000001 wrote=\"replay divergence at step 0: call_with_heartbeat asked for something other than the heartbeat recorded there: call svc.op, recorded svc.another.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 0 bytes, sha256=e3b0c44298fc1c14]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_heartbeats_then_a_call_of_another_operation":     "res=0x1940000000001 wrote=\"replay divergence at step 1: call_with_heartbeat asked for something other than the call recorded there: call svc.op, recorded svc.another.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_heartbeats_then_a_foreign_record":                "res=0x1460000000001 wrote=\"replay divergence at step 2: expected call, got durable_send.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 0 bytes, sha256=e3b0c44298fc1c14]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_heartbeats_then_a_pending_intent":                "res=0xb40000000001 wrote=\"[AMBIGUOUS] call outcome unknown at step 1: the external call to svc.op was dispatched but the response was not recorded before a crash. Check the external service before retrying.\" isReplay=true step=2 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_heartbeats_then_an_error_event":                  "res=0x1a0000000201 wrote=\"the service is unavailable\" isReplay=true step=2 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_heartbeats_then_call":                            "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=true step=3 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_mid_heartbeat_after_a_plain_call":                "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=4 recorded=[call/step=3/svc.op/err=\"\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[1]",
	"replay_mid_heartbeat_interval_zero":                     "res=0x680000000401 wrote=\"cleat: heartbeat interval must be at least 1 ms, got 0 (the host cannot tick at a non-positive interval)\" isReplay=false step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_mid_heartbeat_of_another_operation":              "res=0x1990000000001 wrote=\"replay divergence at step 0: call_with_heartbeat asked for something other than the heartbeat recorded there: call svc.op, recorded svc.another.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 0 bytes, sha256=e3b0c44298fc1c14]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_mid_heartbeat_resumes_the_call":                  "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call/step=2/svc.op/err=\"\"/nonretry=false/code=\"\"] calls=1 others=0 keysFromStep=[0]",
	"replay_pending_intent_is_reported_ambiguous":            "res=0xb40000000001 wrote=\"[AMBIGUOUS] call outcome unknown at step 0: the external call to svc.op was dispatched but the response was not recorded before a crash. Check the external service before retrying.\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_quit":                        "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_terminal_call":               "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"replay_terminal_call_interval_zero": "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"segment_fresh":                      "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"segment_frontier":                   "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"segment_mid_heartbeat":              "res=0x80000000 wrote=\"\" isReplay=false step=2 recorded=[] calls=0 others=0 keysFromStep=[]",
	"segment_replay_terminal_call":       "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=true step=1 recorded=[] calls=0 others=0 keysFromStep=[]",
	"shutdown_fresh":                     "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"shutdown_mid_heartbeat":             "res=0x80000000 wrote=\"\" isReplay=false step=2 recorded=[] calls=0 others=0 keysFromStep=[]",
	"write_failed_fresh":                 "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] calls=0 others=0 keysFromStep=[]",
	"write_failed_mid_heartbeat":         "res=0x80000000 wrote=\"\" isReplay=false step=2 recorded=[] calls=0 others=0 keysFromStep=[]",
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
	// 21 rows as recorded against the code as it was, plus TWELVE added for the migration (cleat#4152, G3): eight that did not move and four that did
	// (named in the header above).
	if len(g3hGolden) != 33 {
		t.Errorf("the table has %d rows; this was recorded with 33, so a row was added or removed without saying so", len(g3hGolden))
	}
}

// The rows above are text, recorded from the code, so they cannot say the layout is the one the guest decodes (see the header). This reads each row's
// result word the way the GUEST does (decodeDurableCallResult, copied from the generated adapter) and requires the decoded length to be the number of
// bytes the row says were written: true of every row only if the word is in the durable-call layout. It is derived from the row's own two fields and from
// nothing the spec says, which is what makes it independent of how the rows were produced. A stop word (res=0x80000000) writes nothing and decodes to 0.
//
// g3hRowProblem is the check on one row, separate so it can be shown to FAIL on a row it should refuse. (A control inside the loop, "the simple reading of
// a good row must not also agree", was removed: if the durable length is n > 0 the simple length is n*256 plus more bits, so it can never agree, and a
// check that cannot fire cannot be seen to be wrong.)
func g3hRowProblem(golden string) string {
	row := regexp.MustCompile(`^res=0x([0-9a-f]+) wrote=("(?:[^"\\]|\\.)*") isReplay=`)
	m := row.FindStringSubmatch(golden)
	if m == nil {
		return fmt.Sprintf("UNMEASURED: not readable as res=0x... wrote=%%q ...: %.80q", golden)
	}
	word, err := strconv.ParseUint(m[1], 16, 64)
	if err != nil {
		return fmt.Sprintf("UNMEASURED: the result word %q does not parse: %v", m[1], err)
	}
	wrote, err := strconv.Unquote(m[2])
	if err != nil {
		return fmt.Sprintf("UNMEASURED: the written text does not unquote: %v", err)
	}
	if length, _, _ := decodeDurableCallResult(int64(word)); int(length) != len(wrote) {
		return fmt.Sprintf("the guest's durable-call decoder reads a length of %d from res=%#x, but the row says %d bytes were written", length, word, len(wrote))
	}
	return ""
}

func TestG3hGoldenRowsDecodeInTheDurableCallLayout(t *testing.T) {
	withMessage := 0
	for name, golden := range g3hGolden {
		if p := g3hRowProblem(golden); p != "" {
			t.Errorf("%s: %s", name, p)
		}
		if !strings.Contains(golden, `wrote=""`) {
			withMessage++
		}
	}
	if withMessage < 15 {
		t.Fatalf("PRECONDITION FAILED: only %d of %d rows carry a message; the check reads too little to mean anything", withMessage, len(g3hGolden))
	}
}

// CONTROL: g3hRowProblem refuses a row recorded in the SIMPLE layout (the original mistake), refuses a row it cannot read, and accepts the same message in
// the durable-call layout. Without this a g3hRowProblem that returned "" for everything would pass the test above.
func TestG3hRowCheckRefusesARowInTheWrongLayoutAndAcceptsTheRightOne(t *testing.T) {
	const msg = "replay divergence at step 0: a message of some length"
	row := func(word uint64) string {
		return fmt.Sprintf("res=%#x wrote=%q isReplay=true step=0 recorded=[] calls=0 others=0 keysFromStep=[]", word, msg)
	}
	n := uint64(len(msg))
	if p := g3hRowProblem(row(n<<40 | 1)); p != "" {
		t.Errorf("CONTROL: the durable-call layout was refused: %s", p)
	}
	if p := g3hRowProblem(row(n<<32 | 1)); !strings.Contains(p, "the guest's durable-call decoder reads a length of") {
		t.Errorf("CONTROL: a row recorded in the simple layout was not refused by name, got %q", p)
	}
	if p := g3hRowProblem("not a row"); !strings.HasPrefix(p, "UNMEASURED") {
		t.Errorf("CONTROL: an unreadable row was not UNMEASURED, got %q", p)
	}
}
