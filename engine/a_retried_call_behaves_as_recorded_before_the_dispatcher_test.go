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

// learnKeys fills stepOfKey: the idempotency key for each of steps 0..9, learned from the engine's own call path (callService) and NOT by calling
// DurableCallIdempotencyKey from this file. THE CALLER KEEPS THE STEP AND NOT THE KEY, and that is deliberate: golangci-lint's gosec G701 (SQL taint
// analysis) reports an UNRELATED line of production code (engine/mysql_lifecycle.go:114) when a test type that implements IdempotentCaller stores
// the key argument it is handed. Measured on the merge of this file into develop 0678b1cb: 1 finding with the key stored, 0 with the interface
// implemented and the key never stored, 0 with the key used for a lookup and only the step stored (this version). An earlier version of this file
// learned the keys correctly through callService but still stored them, linted clean on the base it was written on (5a4d59db) and failed CI's
// lint-go on the merge with the next develop: the finding depends on the base as well as on the file.
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
	return fmt.Sprintf("res=%#x wrote=%q isReplay=%v step=%d recorded=%s attempts=%d others=%d keysFromStep=%v",
		res, string(buf[:n]), s.isReplay, s.stepCount, g3rEvents(s.history, before), caller.attempts, caller.others, caller.stepsOf("wf-g3r"))
}

func g3rCases() map[string]g3rCase {
	pol := g3rDefaultPolicy
	failed := func(step, attempt int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeCallAttemptFailed, Service: "svc", Op: "op", Attempt: attempt, Err: "503 try again"}
	}
	plain := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "before", Response: `{"ok":true}`}
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
	other := failed(0, 1)
	other.Op = "another"
	cases["replay_mid_retry_other_operation"] = g3rCase{replaying: true, history: []EventRecord{other}, policy: pol()}
	cases["replay_other_event_type"] = g3rCase{replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeDurableSend, Service: "svc", Op: "op"}}, policy: pol()}
	cases["replay_a_recorded_refusal"] = g3rCase{replaying: true, history: []EventRecord{{Step: 0, EventType: EventTypeCallRetryRefused, Service: "svc", Op: "op"}}, policy: pol()}
	cases["write_failed_fresh"] = g3rCase{writeFail: true, policy: pol()}
	cases["write_failed_between_attempts"] = g3rCase{failAfter1: true, failFirst: 1 << 20, policy: pol()}
	cases["shutdown_fresh"] = g3rCase{shutdown: true, policy: pol()}
	cases["replay_quit"] = g3rCase{replaying: true, quit: true, history: []EventRecord{terminal}, policy: pol()}
	return cases
}

// g3rGolden is the table recorded against the code as it was (develop 5a4d59db), one string per situation: the reference the migration is checked
// against. A row that changes is a behaviour change, and the PR that changes it says so.
//
// TWO KINDS OF ROW ARE KNOWN-BAD, and are recorded so the migration moves them on purpose and not by accident:
//
//   - replay_mid_retry_all_attempts_spent is a PANIC (cleat#4244): a resume whose recorded attempts already equal the policy's maxAttempts never
//     enters the loop, and freshCallWithRetry then dereferences a nil lastErr. The row records the panic, not an answer.
//   - replay_mid_retry_other_operation and replay_other_event_type end at step 1: a divergence CONSUMED the record before reporting it (the #4152
//     defect), so the call that owns it loses it. The migration consumes nothing.
//
// Worth reading in the rest: replay_mid_retry_keeps_the_first_step shows both attempts carrying step 1's key while the session is at step 2 (cleat#1145);
// write_failed_between_attempts stops after ONE attempt with its failure event recorded (cleat#4210); fresh_policy_too_long_for_the_budget records
// call_retry_refused and makes no attempt (cleat#4032); and the three segment rows show the refusal at the entry, at the frontier and the whole policy
// given to a defer BODY.
var g3rGolden = map[string]string{
	"fresh_always_fails":                    "res=0x2b0000000201 wrote=\"retries exhausted: TRY_AGAIN: 503 try again\" isReplay=false step=3 recorded=[call_attempt_failed/step=0/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call_attempt_failed/step=1/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"retries exhausted: TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=true/code=\"\"] attempts=3 others=0 keysFromStep=[0 0 0]",
	"fresh_attempt_ceiling":                 "res=0x2b0000000201 wrote=\"retries exhausted: TRY_AGAIN: 503 try again\" isReplay=false step=4 recorded=[call_attempt_failed/step=0/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call_attempt_failed/step=1/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call_attempt_failed/step=2/attempt=3/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=3/attempt=0/svc.op/err=\"retries exhausted: TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=true/code=\"\"] attempts=4 others=0 keysFromStep=[0 0 0 0]",
	"fresh_fails_twice_then_succeeds":       "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call_attempt_failed/step=0/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call_attempt_failed/step=1/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=3 others=0 keysFromStep=[0 0 0]",
	"fresh_first_attempt_succeeds":          "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0]",
	"fresh_non_retryable_stops_at_one":      "res=0x180000000001 wrote=\"TRY_AGAIN: 503 try again\" isReplay=false step=1 recorded=[call/step=0/attempt=0/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=true/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0]",
	"fresh_policy_too_long_for_the_budget":  "res=0x850000000601 wrote=\"retry policy rejected: worst-case backoff exceeds the 1ns host-retry budget; run the policy in the guest, suspending between attempts\" isReplay=false step=1 recorded=[call_retry_refused/step=0/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=0 others=0 keysFromStep=[]",
	"fresh_unparseable_non_retryable":       "res=0x401 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"frontier_gets_its_whole_policy":        "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call_attempt_failed/step=1/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=2 others=0 keysFromStep=[1 1]",
	"replay_a_recorded_refusal":             "res=0xbd0000000601 wrote=\"retry policy rejected: this policy was refused when this call was recorded, as its worst-case backoff exceeds the host-retry budget; run the policy in the guest, suspending between attempts\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_mid_retry_all_attempts_spent":   "PANIC=\"runtime error: invalid memory address or nil pointer dereference\" isReplay=false step=3 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_mid_retry_keeps_the_first_step": "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=4 recorded=[call_attempt_failed/step=2/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=3/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=2 others=0 keysFromStep=[1 1]",
	"replay_mid_retry_one_attempt_spent":    "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call_attempt_failed/step=1/attempt=2/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=2 others=0 keysFromStep=[0 0]",
	"replay_mid_retry_other_operation":      "res=0xfd0000000001 wrote=\"replay divergence at step 0: expected call event, got call_attempt_failed.\\n  actual request: {}\\n  expected request: \\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_other_event_type":               "res=0xf60000000001 wrote=\"replay divergence at step 0: expected call event, got durable_send.\\n  actual request: {}\\n  expected request: \\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_quit":                           "res=0x0 wrote=\"\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_terminal_call":                  "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"replay_terminal_error_record":          "res=0x200000000201 wrote=\"retries exhausted: 503 try again\" isReplay=true step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"segment_defer_body":                    "res=0xb0000000000 wrote=\"{\\\"ok\\\":true}\" isReplay=false step=3 recorded=[call_attempt_failed/step=1/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\" call/step=2/attempt=0/svc.op/err=\"\"/nonretry=false/exhausted=false/code=\"\"] attempts=2 others=0 keysFromStep=[1 1]",
	"segment_fresh":                         "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"segment_frontier":                      "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"shutdown_fresh":                        "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
	"write_failed_between_attempts":         "res=0x80000000 wrote=\"\" isReplay=false step=1 recorded=[call_attempt_failed/step=0/attempt=1/svc.op/err=\"TRY_AGAIN: 503 try again\"/nonretry=false/exhausted=false/code=\"\"] attempts=1 others=0 keysFromStep=[0]",
	"write_failed_fresh":                    "res=0x80000000 wrote=\"\" isReplay=false step=0 recorded=[] attempts=0 others=0 keysFromStep=[]",
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
	if len(g3rGolden) != 23 {
		t.Errorf("the table has %d rows; this was recorded with 23, so a row was added or removed without saying so", len(g3rGolden))
	}
}
