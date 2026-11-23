package engine

// cleat#4152, group G3 (Multi): a characterization of DurableCallWithRetry, RECORDED FROM THE CODE AS IT WAS BEFORE IT WENT THROUGH step() (no
// production change in this file's PR), and kept so that moving it is checkable as "changes where the code lives and nothing else" and every row it
// does change is visible in the migration's diff.
//
// A retried call is the heaviest single call in the engine, and the situations here are the reasons: it records a refusal as an event
// (call_retry_refused, cleat#4032), judges a budget that is tenant state, resumes a policy a lost worker interrupted from the attempts already
// recorded and keeps the FIRST incarnation's step so every attempt carries one idempotency key (cleat#1145), fails closed on a non-retryable
// list it cannot parse, and checks the stop BETWEEN attempts as well as at its entry (cleat#4210).
//
// Each row is one string: the packed result, the bytes written to the guest, whether the session is still replaying, the step count, the events the
// call RECORDED, and what reached the OUTSIDE WORLD (attempts at svc.op, calls to anything else, and the step whose idempotency key each attempt
// carried). The backoff is 1 ms, so the timing is not what is being measured, and nothing here waits.
//
// What a row cannot say: the timestamp of a recorded event, the metrics the call emits, and anything the wasm backends add around it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const g3rBufLen = 1024

// g3rCaller is a caller that fails its first failFirst attempts at svc.op with a retryable error, answers everything else, and reports the step whose
// idempotency key each attempt carried. It implements IdempotentCaller, so callService hands it a key.
type g3rCaller struct {
	failFirst int
	attempts  int            // attempts at svc.op
	others    int            // every other call
	stepOfKey map[string]int // the key of each of steps 0..9, learned from the engine's own call path before the call (learnKeys)
	steps     []int          // the step each svc.op attempt's key was derived from; -1 for a key that matches none; the key itself is not kept
	onAttempt func(n int)
}

// learnKeys fills stepOfKey: the idempotency key for each of steps 0..9, learned from the engine's own call path (callService), so this file never calls
// DurableCallIdempotencyKey itself. The caller keeps the STEP each key came from and not the key.
//
// THAT SHAPE IS NOT A LINT FIX AND IS NOT CLAIMED TO BE ONE. An earlier version of this comment said it stopped gosec's G701 from flagging an unrelated
// production line (engine/mysql_lifecycle.go:114); that rested on single runs of each variant and did not hold: lint-go failed on a head built this way,
// and the finding fires on some runs of UNMODIFIED develop (1 of 5 fresh-cache runs, cleat#4252, the cleat#3247 mechanism). It is kept because it is a
// sufficient way to read the keys and the golden table passes with it, not because it avoids anything.
func (c *g3rCaller) learnKeys(workflowID string) {
	probe := newRecordingCaller()
	ps := &execSession{engine: NewEngine(nil, probe), workflowID: workflowID}
	c.stepOfKey = map[string]int{}
	for n := 0; n < 10; n++ {
		_, _ = ps.callService(context.Background(), "probe", "probe", `{}`, n)
		c.stepOfKey[probe.keys[n]] = n
	}
}

func (c *g3rCaller) stepsOf(string) []int { return c.steps }

func (c *g3rCaller) Call(ctx context.Context, service, operation, req string) (string, error) {
	return c.CallWithIdempotencyKey(ctx, service, operation, req, "")
}

func (c *g3rCaller) CallWithIdempotencyKey(_ context.Context, service, operation, _ string, key string) (string, error) {
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
	if c.onAttempt != nil {
		c.onAttempt(c.attempts)
	}
	if c.attempts <= c.failFirst {
		return "", &ServiceError{Code: "TRY_AGAIN", Message: "503 try again"}
	}
	return `{"ok":true}`, nil
}

// g3rCase is one situation: the history the session starts with, how it is built, the caller's behaviour and the policy the guest passes.
type g3rCase struct {
	history    []EventRecord
	replaying  bool
	deferPhase bool
	draining   bool
	shutdown   bool
	writeFail  bool
	suspended  bool // the session has already suspended (cleat#4285), so it starts no fresh work
	budget     time.Duration
	maxRetries int
	quit       bool
	prefix     bool // the guest makes a plain svc.before call first, so the retried call arrives at step 1 or at the replay frontier
	failFirst  int
	failAfter1 bool // the caller marks the session's event write as refused after its first attempt (the cleat#4210 shape)
	policy     g3rPolicy
}

type g3rPolicy struct {
	maxAttempts, initialMs, coef100, maxMs int64
	nonRetryable                           string
}

func g3rDefaultPolicy() g3rPolicy {
	return g3rPolicy{maxAttempts: 3, initialMs: 1, coef100: 100, maxMs: 1, nonRetryable: `[]`}
}

func g3rEvents(history []EventRecord, from int) string {
	var parts []string
	for _, r := range history[from:] {
		parts = append(parts, fmt.Sprintf("%s/step=%d/attempt=%d/%s.%s/err=%q/nonretry=%v/exhausted=%v/code=%q", r.EventType, r.Step, r.Attempt, r.Service, r.Op, r.Err, r.ErrNonRetryable, r.RetriesExhausted, r.ErrCode))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func (c g3rCase) observe() string {
	caller := &g3rCaller{failFirst: c.failFirst}
	caller.learnKeys("wf-g3r")
	opts := []EngineOption{WithWorkflowID("wf-g3r")}
	if c.deferPhase {
		opts = append(opts, WithDeferPhase())
	}
	if c.budget > 0 {
		opts = append(opts, WithHostRetryBudget(c.budget))
	}
	if c.shutdown {
		down := make(chan struct{})
		close(down)
		opts = append(opts, WithShutdownSignal(down))
	}
	eng := NewEngine(nil, caller, opts...)
	if c.maxRetries > 0 {
		eng.maxRetries = c.maxRetries
	}
	s := &execSession{
		engine: eng, workflowID: "wf-g3r", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
		isReplay: c.replaying, history: append([]EventRecord(nil), c.history...), inDeferDrain: c.draining,
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
	if c.failAfter1 {
		caller.onAttempt = func(n int) {
			if n == 1 {
				s.eventWriteFailure = errors.New("event write refused")
			}
		}
	}
	buf := make([]byte, g3rBufLen)
	ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
	if c.prefix {
		s.DurableCall(ctx, nil, "svc", "before", `{}`, 0, g3rBufLen)
		for i := range buf {
			buf[i] = 0
		}
	}
	before := len(s.history)
	p := c.policy
	var res int64
	var panicked string
	func() {
		// A row that panics is a row (cleat#4244): the table records the defect and is not aborted by it.
		defer func() {
			if r := recover(); r != nil {
				panicked = fmt.Sprint(r)
			}
		}()
		res = s.DurableCallWithRetry(ctx, nil, "svc", "op", `{}`, p.maxAttempts, p.initialMs, p.coef100, p.maxMs, p.nonRetryable, 0, g3rBufLen)
	}()
	if panicked != "" {
		return fmt.Sprintf("PANIC=%q isReplay=%v step=%d recorded=%s attempts=%d others=%d keysFromStep=%v", panicked, s.isReplay, s.stepCount, g3rEvents(s.history, before), caller.attempts, caller.others, caller.stepsOf("wf-g3r"))
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	out := fmt.Sprintf("res=%#x wrote=%q isReplay=%v step=%d recorded=%s attempts=%d others=%d keysFromStep=%v",
		res, string(buf[:n]), s.isReplay, s.stepCount, g3rEvents(s.history, before), caller.attempts, caller.others, caller.stepsOf("wf-g3r"))
	// A spec that did not do what it declared is part of what the call did, and is appended ONLY when there is one, so a row that has none reads exactly
	// as it did before the call moved. The refusals that record nothing by design must say so (RecordsNothing): without this a spec that forgot to would
	// pass every row.
	if len(s.stepViolations) > 0 {
		out += fmt.Sprintf(" SPEC-VIOLATIONS=%q", s.stepViolations)
	}
	return out
}

func g3rCases() map[string]g3rCase {
	pol := g3rDefaultPolicy
	failed := func(step, attempt int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op", Attempt: attempt, Err: "503 try again"}
	}
	plain := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "before", Request: `{}`, Response: `{"ok":true}`}
	terminal := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{"ok":true}`}
	cases := map[string]g3rCase{}
	cases["fresh_first_attempt_succeeds"] = g3rCase{policy: pol()}
	cases["fresh_fails_twice_then_succeeds"] = g3rCase{failFirst: 2, policy: pol()}
	cases["fresh_always_fails"] = g3rCase{failFirst: 1 << 20, policy: pol()}
	nr := pol()
	nr.nonRetryable = `["TRY_AGAIN"]`
	cases["fresh_non_retryable_stops_at_one"] = g3rCase{failFirst: 1 << 20, policy: nr}
	big := pol()
	big.maxAttempts = 1000
	cases["fresh_attempt_ceiling"] = g3rCase{failFirst: 1 << 20, maxRetries: 4, policy: big}
	bad := pol()
	bad.nonRetryable = `not json`
	cases["fresh_unparseable_non_retryable"] = g3rCase{policy: bad}
	zero := pol()
	zero.maxAttempts = 0
	negative := pol()
	negative.maxAttempts = -1
	cases["fresh_max_attempts_zero_is_refused"] = g3rCase{policy: zero}
	cases["fresh_max_attempts_negative_is_refused"] = g3rCase{policy: negative}
	cases["segment_max_attempts_zero_stop_wins"] = g3rCase{deferPhase: true, policy: zero}
	cases["shutdown_max_attempts_zero_stop_wins"] = g3rCase{shutdown: true, policy: zero}
	cases["write_failed_max_attempts_zero_stop_wins"] = g3rCase{writeFail: true, policy: zero}
	long := g3rPolicy{maxAttempts: 5, initialMs: 1000, coef100: 200, maxMs: 0, nonRetryable: `[]`}
	cases["fresh_policy_too_long_for_the_budget"] = g3rCase{budget: time.Nanosecond, policy: long}
	cases["segment_fresh"] = g3rCase{deferPhase: true, policy: pol()}
	cases["segment_frontier"] = g3rCase{deferPhase: true, replaying: true, prefix: true, history: []EventRecord{plain}, policy: pol()}
	cases["segment_defer_body"] = g3rCase{deferPhase: true, draining: true, replaying: true, prefix: true, history: []EventRecord{plain}, failFirst: 1, policy: pol()}
	cases["frontier_gets_its_whole_policy"] = g3rCase{replaying: true, prefix: true, history: []EventRecord{plain}, failFirst: 1, policy: pol()}
	cases["replay_terminal_call"] = g3rCase{replaying: true, history: []EventRecord{terminal}, policy: pol()}
	errTerminal := terminal
	errTerminal.Response, errTerminal.Err, errTerminal.RetriesExhausted, errTerminal.ErrCode = "", "retries exhausted: 503 try again", true, "TRY_AGAIN"
	cases["replay_terminal_error_record"] = g3rCase{replaying: true, history: []EventRecord{errTerminal}, policy: pol()}
	cases["replay_mid_retry_one_attempt_spent"] = g3rCase{replaying: true, history: []EventRecord{failed(0, 1)}, failFirst: 1, policy: pol()}
	cases["replay_mid_retry_keeps_the_first_step"] = g3rCase{replaying: true, prefix: true, history: []EventRecord{plain, failed(1, 1)}, failFirst: 1, policy: pol()}
	cases["replay_mid_retry_all_attempts_spent"] = g3rCase{replaying: true, history: []EventRecord{failed(0, 1), failed(1, 2), failed(2, 3)}, policy: pol()}
	cases["replay_mid_retry_max_attempts_zero_is_refused"] = g3rCase{replaying: true, history: []EventRecord{failed(0, 1)}, policy: zero}
	cases["replay_terminal_call_max_attempts_zero_is_refused"] = g3rCase{replaying: true, history: []EventRecord{terminal}, policy: zero}
	other := failed(0, 1)
	other.Op = "another"
	cases["replay_mid_retry_other_operation"] = g3rCase{replaying: true, history: []EventRecord{other}, policy: pol()}
	cases["replay_other_event_type"] = g3rCase{replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeDurableSend, Service: "svc", Op: "op"}}, policy: pol()}
	cases["replay_a_recorded_refusal"] = g3rCase{replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeCallRetryRefused, Service: "svc", Op: "op"}}, policy: pol()}
	cases["write_failed_fresh"] = g3rCase{writeFail: true, policy: pol()}
	cases["write_failed_between_attempts"] = g3rCase{failAfter1: true, failFirst: 1 << 20, policy: pol()}
	cases["shutdown_fresh"] = g3rCase{shutdown: true, policy: pol()}
	cases["replay_quit"] = g3rCase{replaying: true, quit: true, history: []EventRecord{terminal}, policy: pol()}

	// ROWS ADDED FOR THE MIGRATION (cleat#4152, G3): shapes the dispatcher's Role and Identity read differently from the old replay loop, measured on the
	// old code first. Attempts of ANOTHER operation, a foreign record after the attempts, a terminal call of another operation, a pending intent and an
	// error terminal after attempts, a stop or a refused write when the history ends inside the policy, and a zero policy at the replay frontier.
	otherAttempt := failed(0, 1)
	otherAttempt.Op = "another"
	terminalAt1 := terminal
	terminalAt1.Step = 1
	cases["replay_attempts_of_another_operation_then_the_call"] = g3rCase{replaying: true, history: []EventRecord{otherAttempt, terminalAt1}, policy: pol()}
	cases["replay_attempts_then_a_foreign_record"] = g3rCase{replaying: true, history: []EventRecord{failed(0, 1), failed(1, 2), {Step: 2, EventType: EventTypeDurableSend, Service: "svc", Op: "op"}}, policy: pol()}
	terminalOther := terminalAt1
	terminalOther.Op = "another"
	cases["replay_attempts_then_a_call_of_another_operation"] = g3rCase{replaying: true, history: []EventRecord{failed(0, 1), terminalOther}, policy: pol()}
	pendingAt1 := terminalAt1
	pendingAt1.Response, pendingAt1.Pending = "", true // a real pending intent: Pending is what isPendingIntent reads, and an empty Response alone is an empty success
	cases["replay_attempts_then_a_pending_intent"] = g3rCase{replaying: true, history: []EventRecord{failed(0, 1), pendingAt1}, policy: pol()}
	errAt1 := errTerminal
	errAt1.Step = 1
	cases["replay_attempts_then_an_error_terminal"] = g3rCase{replaying: true, history: []EventRecord{failed(0, 1), errAt1}, policy: pol()}
	cases["segment_mid_retry"] = g3rCase{deferPhase: true, replaying: true, history: []EventRecord{failed(0, 1)}, policy: pol()}
	cases["write_failed_mid_retry"] = g3rCase{writeFail: true, replaying: true, history: []EventRecord{failed(0, 1)}, policy: pol()}
	cases["shutdown_mid_retry"] = g3rCase{shutdown: true, replaying: true, history: []EventRecord{failed(0, 1)}, policy: pol()}
	cases["frontier_max_attempts_zero_is_refused"] = g3rCase{replaying: true, prefix: true, history: []EventRecord{plain}, policy: zero}
	// After a suspension (cleat#4285) a session starts no fresh work. The old code judged the BUDGET first and only then reached the suspension check, so
	// a suspended session with a too-long policy was refused for the policy and RECORDED call_retry_refused.
	// A RESUME IS NOT JUDGED AGAINST THE BUDGET AGAIN: the old code skipped the check on any session still replaying, and a resume is one. A policy that is
	// too long for the budget now but fitted when its attempts were recorded (the budget is tenant state, writable while a run is in flight) is finished,
	// not refused.
	cases["replay_mid_retry_is_not_judged_against_the_budget_again"] = g3rCase{replaying: true, budget: time.Nanosecond, history: []EventRecord{failed(0, 1)}, policy: long}
	cases["suspended_fresh"] = g3rCase{suspended: true, policy: pol()}
	cases["suspended_policy_too_long_for_the_budget"] = g3rCase{suspended: true, budget: time.Nanosecond, policy: long}
	cases["suspended_max_attempts_zero_is_refused"] = g3rCase{suspended: true, policy: zero}
	cases["replay_a_recorded_refusal_max_attempts_zero_is_refused"] = g3rCase{replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeCallRetryRefused, Service: "svc", Op: "op"}}, policy: zero}
	return cases
}

// g3rGolden is the table recorded against the code as it was (develop 5a4d59db), one string per situation: the reference the migration is checked
// against. A row that changes is a behaviour change, and the PR that changes it says so.
//
// ONE KIND OF ROW IS STILL KNOWN-BAD (and one was fixed), and are recorded so the migration moves them on purpose and not by accident:
//
//   - (fixed in cleat#4244, now pinned as correct) replay_mid_retry_all_attempts_spent recorded a PANIC: a resume whose recorded attempts already
//     equal the policy's maxAttempts never entered the loop, and freshCallWithRetry dereferenced a nil lastErr. It now makes NO service call and
//     records the retries-exhausted failure carrying the last recorded attempt's error, retryable, as a policy that ran out normally does.
//   - (MOVED by the retried call's migration onto the dispatcher, cleat#4152 G3, and now pinned as the dispatcher's) the divergence rows. The old
//     replay loop CONSUMED the attempts and then the record after them before reporting a divergence (the #4152 defect), so the call that owns that
//     record loses it; the dispatcher reads the run first and consumes NOTHING (cleat#4157), in the shared message plus the request lines the old message printed (cleat#4152's divergence-detail change put the request lines back: the first version of this move dropped them, and the rows below carry them). SEVEN rows moved, measured
//     on develop 8da56c0b against the migrated call, the other 37 byte-identical:
//     replay_other_event_type                                 step 1 -> 0
//     replay_attempts_then_a_foreign_record (added)           step 3 -> 0: the attempts and the record after them were swallowed together
//     replay_attempts_then_a_call_of_another_operation (added) step 2 -> 0
//     and TWO the old table could not see, because the old loop stopped at the first attempt it did not recognise and then reported the wrong thing:
//     replay_mid_retry_other_operation, replay_attempts_of_another_operation_then_the_call: OLD "expected call event, got call_attempt_failed", which
//     names the record and not the mistake. NEW a divergence naming the attempt and the operation it was recorded for.
//     Two rows moved for reasons of their own:
//     frontier_max_attempts_zero_is_refused: the same refusal, and isReplay stays TRUE. The old code left replay at the frontier before it refused;
//     the check now sits before the dispatcher (it must precede replay, see DurableCallWithRetry), which leaves replay only to run fresh work.
//     suspended_policy_too_long_for_the_budget: OLD the budget was judged before the suspension check, so a session that had already suspended was
//     refused for its policy AND RECORDED call_retry_refused. NEW it answers the stop word and records nothing: step() refuses a suspended session
//     before Fresh (cleat#4285), which is what that guard is for.
//
// Worth reading in the rest: replay_mid_retry_keeps_the_first_step shows both attempts carrying step 1's key while the session is at step 2 (cleat#1145);
// write_failed_between_attempts stops after ONE attempt with its failure event recorded (cleat#4210); fresh_policy_too_long_for_the_budget records
// call_retry_refused and makes no attempt (cleat#4032); and the three segment rows show the refusal at the entry, at the frontier and the whole policy
// given to a defer BODY.
var g3rGolden = map[string]string{
	"fresh_always_fails":                                      "res=0x2b0000000201 wrote=\"retries exhausted: TRY_AGAIN: 503 try again\" isReplay=false step=3 recorded=[call_attempt_failed/step=0/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call_attempt_failed/step=1/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"retries exhausted: TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=true/code=\"\"] attempts=3 others=0 keysFromStep=[0 0 0]",
	"fresh_attempt_ceiling":                                   "res=0x2b0000000201 wrote=\"retries exhausted: TRY_AGAIN: 503 try again\" isReplay=false step=4 recorded=[call_attempt_failed/step=0/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call_attempt_failed/step=1/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call_attempt_failed/step=2/attempt=3/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=3/attempt=0/svc.op/err=\"retries exhausted: TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=true/code=\"\"] attempts=4 others=0 keysFromStep=[0 0 0 0]",
	"fresh_fails_twice_then_succeeds":                         "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call_attempt_failed/step=0/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call_attempt_failed/step=1/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=3 others=0 keysFromStep=[0 0 0]",
	"fresh_first_attempt_succeeds":                            "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0]",
	"fresh_non_retryable_stops_at_one":                        "res=0x180000000001 wrote=\"TRY_AGAIN: 503 try again\" isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=true/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0]",
	"fresh_policy_too_long_for_the_budget":                    "res=0x850000000601 wrote=\"retry policy rejected: worst-case backoff exceeds the 1ns host-retry budget; run the policy in the guest, suspending between attempts\" isReplay=false step=1 recorded=[call_retry_refused/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=0 others=0 keysFromStep=[]",
	"fresh_unparseable_non_retryable":                         "res=0x401 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"fresh_max_attempts_zero_is_refused":                      "res=0x401 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"fresh_max_attempts_negative_is_refused":                  "res=0x401 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"segment_max_attempts_zero_stop_wins":                     "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"shutdown_max_attempts_zero_stop_wins":                    "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"write_failed_max_attempts_zero_stop_wins":                "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_mid_retry_max_attempts_zero_is_refused":           "res=0x401 wrote=\"\" isReplay=true step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_terminal_call_max_attempts_zero_is_refused":       "res=0x401 wrote=\"\" isReplay=true step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"frontier_gets_its_whole_policy":                          "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call_attempt_failed/step=1/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=2 others=0 keysFromStep=[1 1]",
	"replay_a_recorded_refusal":                               "res=0xbd0000000601 wrote=\"retry policy rejected: this policy was refused when this call was recorded, as its worst-case backoff exceeds the host-retry budget; run the policy in the guest, suspending between attempts\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_mid_retry_all_attempts_spent":                     "res=0x200000000201 wrote=\"retries exhausted: 503 try again\" isReplay=false step=4 recorded=[call/step=3/attempt=0/svc.op/err=\"retries exhausted: 503 try again\"/nonretry=false/exhausted=true/code=\"\"] attempts=0 others=0 keysFromStep=[]",
	"replay_mid_retry_keeps_the_first_step":                   "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=4 recorded=[call_attempt_failed/step=2/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=3/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=2 others=0 keysFromStep=[1 1]",
	"replay_mid_retry_one_attempt_spent":                      "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call_attempt_failed/step=1/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=2 others=0 keysFromStep=[0 0]",
	"replay_mid_retry_other_operation":                        "res=0x19f0000000001 wrote=\"replay divergence at step 0: call_with_retry asked for something other than the call_attempt_failed recorded there: call svc.op, recorded svc.another.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 0 bytes, sha256=e3b0c44298fc1c14]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_other_event_type":                                 "res=0x1460000000001 wrote=\"replay divergence at step 0: expected call, got durable_send.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 0 bytes, sha256=e3b0c44298fc1c14]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_quit":                                             "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_terminal_call":                                    "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_terminal_error_record":                            "res=0x200000000201 wrote=\"retries exhausted: 503 try again\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"segment_defer_body":                                      "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call_attempt_failed/step=1/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=2 others=0 keysFromStep=[1 1]",
	"segment_fresh":                                           "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"segment_frontier":                                        "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"shutdown_fresh":                                          "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"write_failed_between_attempts":                           "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[call_attempt_failed/step=0/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0]",
	"write_failed_fresh":                                      "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"frontier_max_attempts_zero_is_refused":                   "res=0x401 wrote=\"\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_a_recorded_refusal_max_attempts_zero_is_refused":  "res=0x401 wrote=\"\" isReplay=true step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_attempts_of_another_operation_then_the_call":      "res=0x19f0000000001 wrote=\"replay divergence at step 0: call_with_retry asked for something other than the call_attempt_failed recorded there: call svc.op, recorded svc.another.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 0 bytes, sha256=e3b0c44298fc1c14]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_attempts_then_a_call_of_another_operation":        "res=0x1900000000001 wrote=\"replay divergence at step 1: call_with_retry asked for something other than the call recorded there: call svc.op, recorded svc.another.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_attempts_then_a_foreign_record":                   "res=0x1460000000001 wrote=\"replay divergence at step 2: expected call, got durable_send.\\n  actual request: [redacted: 2 bytes, sha256=44136fa355b3678a]\\n  expected request: [redacted: 0 bytes, sha256=e3b0c44298fc1c14]\\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_attempts_then_a_pending_intent":                   "res=0xb40000000001 wrote=\"[AMBIGUOUS] call outcome unknown at step 1: the external call to svc.op was dispatched but the response was not recorded before a crash. Check the external service before retrying.\" isReplay=true step=2 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_attempts_then_an_error_terminal":                  "res=0x200000000201 wrote=\"retries exhausted: 503 try again\" isReplay=true step=2 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"segment_mid_retry":                                       "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"shutdown_mid_retry":                                      "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"write_failed_mid_retry":                                  "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"suspended_fresh":                                         "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"suspended_max_attempts_zero_is_refused":                  "res=0x401 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"suspended_policy_too_long_for_the_budget":                "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_mid_retry_is_not_judged_against_the_budget_again": "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=2 recorded=[call/step=1/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0]",
}

func TestG3rRetriedCallBehavesAsRecordedBeforeTheDispatcher(t *testing.T) {
	cases := g3rCases()
	for name, want := range g3rGolden {
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
		if _, ok := g3rGolden[name]; !ok {
			t.Errorf("%s: a situation with no golden row", name)
		}
	}
	// 23 rows as recorded against the code as it was, plus SEVEN ADDED by cleat#4244's refusal of a policy that allows no attempt: zero and negative
	// maxAttempts fresh, zero on a resume and on a replayed terminal call, and zero under each of the three stops (a defer segment, a shutdown and a
	// refused event write), which answer the suspend sentinel because bit 31 wins over a refusal. None of the 23 moved for that reason.
	// 30 rows as recorded against the code as it was, plus FOURTEEN added for the migration (cleat#4152, G3), measured on the old code before it moved.
	if len(g3rGolden) != 44 {
		t.Errorf("the table has %d rows; this was recorded with 44, so a row was added or removed without saying so", len(g3rGolden))
	}
}

// TestAResumeWithEveryAttemptAlreadySpentRecordsTheExhaustionAndMakesNoCall is cleat#4244 stated directly, in both directions: a resume whose recorded
// attempts equal maxAttempts must not call the service and must not panic, and a resume with one attempt LEFT must still call it (so the guard is not
// "never call on resume").
func TestAResumeWithEveryAttemptAlreadySpentRecordsTheExhaustionAndMakesNoCall(t *testing.T) {
	failed := func(step, attempt int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op", Attempt: attempt, Err: "503 try again"}
	}
	pol := func() g3rPolicy { return g3rDefaultPolicy() }
	spent := g3rCase{replaying: true, history: []EventRecord{failed(0, 1), failed(1, 2), failed(2, 3)}, policy: pol()}
	got := spent.observe()
	if strings.HasPrefix(got, "PANIC") {
		t.Fatalf("a spent resume panicked: %s", got)
	}
	for _, want := range []string{"attempts=0", `exhausted=true`, `retries exhausted: 503 try again`, "nonretry=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("a spent resume: want %q in %s", want, got)
		}
	}
	oneLeft := g3rCase{replaying: true, history: []EventRecord{failed(0, 1), failed(1, 2)}, failFirst: 0, policy: pol()}
	if got := oneLeft.observe(); !strings.Contains(got, "attempts=1") {
		t.Errorf("a resume with one attempt left must make exactly one call, got %s", got)
	}
}

// The rows above are text recorded from the code, so they cannot say the layout is the one the guest decodes (the heartbeat table's header says how that
// went wrong once). This reads every row's result word the way the GUEST does and requires the decoded length to be the number of bytes the row says were
// written, through the same check the heartbeat table uses and with its control (TestG3hRowCheckRefusesARowInTheWrongLayoutAndAcceptsTheRightOne).
func TestG3rGoldenRowsDecodeInTheDurableCallLayout(t *testing.T) {
	withMessage := 0
	for name, golden := range g3rGolden {
		if p := g3hRowProblem(golden); p != "" {
			t.Errorf("%s: %s", name, p)
		}
		if !strings.Contains(golden, `wrote=""`) {
			withMessage++
		}
	}
	if withMessage < 15 {
		t.Fatalf("PRECONDITION FAILED: only %d of %d rows carry a message; the check reads too little to mean anything", withMessage, len(g3rGolden))
	}
}
