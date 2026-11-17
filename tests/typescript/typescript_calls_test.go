package typescript

// The calls-group bindings (cleat#3827, group H): callRetry, callHeartbeat, send, scheduleInvoke.
//
// THE CALLER IS SCRIPTED PER ATTEMPT, not just per operation: `payments/charge` fails its FIRST attempt and succeeds on the
// second, which is the only shape that can tell a retry from a single call. A caller that always succeeds makes a binding that
// never retries pass, and a caller that always fails makes one that retries once look identical to one that retries properly.
//
// The retry policy has TWO paths, and both are exercised: the host runs the whole policy inside one segment when its worst-case
// backoff fits the tenant's host-retry budget, and otherwise refuses before making any call so the SDK's loop does it, sleeping
// (and therefore suspending) between attempts. `engine.WithHostRetryBudget` is the switch between them -- measured here rather
// than assumed, because "which path ran" is what decides whether the test is about the host or about the SDK.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// scriptedCaller answers durable calls from a map, and `payments/charge` fails on its first attempt before answering.
type scriptedCaller struct {
	mu       sync.Mutex
	attempts map[string]int
	asked    []string
}

func newScriptedCaller() *scriptedCaller { return &scriptedCaller{attempts: map[string]int{}} }

func (c *scriptedCaller) Call(_ context.Context, service, operation, _ string) (string, error) {
	key := service + "/" + operation
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[key]++
	c.asked = append(c.asked, key)
	switch key {
	case "payments/charge":
		if c.attempts[key] == 1 {
			return "", fmt.Errorf("the service was briefly unavailable")
		}
		return `{"id":"ch_1"}`, nil
	case "payments/always-fails":
		return "", fmt.Errorf("the service refused")
	case "payments/slow":
		return `{"alive":true}`, nil
	case "email/notify", "email/digest":
		return `{}`, nil
	}
	return "", fmt.Errorf("the test scripted no answer for %q", key)
}

func (c *scriptedCaller) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[key]
}

func buildCalls(t *testing.T) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/calls_group.ts"
	res := build(t, root, src, "calls_retry,calls_retry_exhausted,calls_heartbeat,calls_send,calls_schedule")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

// newCallsEngine is newEngineWith with a service caller: the caller is a POSITIONAL argument of engine.NewEngine, not an option,
// which is why the shared helper cannot be used here.
func newCallsEngine(t *testing.T, caller engine.ServiceCaller, extra ...engine.EngineOption) (*engine.Engine, func()) {
	t.Helper()
	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("engine.NewRuntime: %v", err)
	}
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		_ = rt.Close(ctx)
		t.Fatalf("engine.NewWasmtimeBackend: %v", err)
	}
	opts := append([]engine.EngineOption{
		engine.WithWorkflowID(benchWF),
		engine.WithDefName("bench"),
		engine.WithDefVersion(1),
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	}, extra...)
	e := engine.NewEngine(rt, caller, opts...)
	return e, func() { _ = rt.Close(ctx) }
}

// runCalls executes one entry and requires it not to suspend. The caller is fresh per test so its per-attempt script cannot leak
// between them.
func runCalls(t *testing.T, wasm []byte, entry string, caller *scriptedCaller, extra ...engine.EngineOption) (string, []engine.EventRecord) {
	t.Helper()
	e, closeRT := newCallsEngine(t, caller, extra...)
	defer closeRT()
	raw, history, susp, _, _, err := e.Execute(context.Background(), wasm, entry, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("%s: running the workflow: %v", entry, err)
	}
	if susp != nil {
		t.Fatalf("%s: the workflow suspended (%s) although the policy fits one segment", entry, susp.Reason)
	}
	return string(raw), history
}

// TestTypeScriptCallRetryRetriesOnTheHost: the host path. The first attempt fails, the second answers, and the workflow sees the
// value -- while the caller records TWO attempts, which is the part a single-answer fixture could not measure.
func TestTypeScriptCallRetryRetriesOnTheHost(t *testing.T) {
	wasm := buildCalls(t)
	caller := newScriptedCaller()
	raw, history := runCalls(t, wasm, "calls_retry", caller)

	var got struct {
		Out struct {
			ID string `json:"id"`
		} `json:"out"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if got.Out.ID != "ch_1" {
		t.Errorf("callRetry resolved with %q, want the second attempt's ch_1", got.Out.ID)
	}
	if n := caller.count("payments/charge"); n != 2 {
		t.Errorf("the service was called %d time(s), want 2: the first attempt fails and the retry answers", n)
	}
	// ONE history event for the whole logical call is what the host path buys; a per-attempt loop would record two.
	if n := countCallEvents(history); n != 1 {
		t.Errorf("the history holds %d call events, want 1: the host runs the whole policy in one segment", n)
	}
}

// TestTypeScriptCallRetryReportsARefusedPolicy: a policy too long for the tenant's host-retry budget is REFUSED, nothing is
// called, and the rejection says the policy is the problem rather than the service.
//
// The fallback Go and Rust take from here is deliberately NOT taken: it is broken on replay (cleat#4032), and the measurement
// that found it is this test's neighbour. `cleat.WithHostRetryBudget` is the only way to reach the refusal.
func TestTypeScriptCallRetryReportsARefusedPolicy(t *testing.T) {
	wasm := buildCalls(t)
	caller := newScriptedCaller()
	// The fixture's policy backs off once, by 1ms, so any budget below 1ms refuses it. `retryPolicyFitsBudget` sums the backoffs
	// and answers false as soon as the total exceeds the budget, so a budget of 1ms would FIT this policy and the refusal would
	// never be reached -- measured, then corrected here.
	raw, _ := runCalls(t, wasm, "calls_retry_exhausted", caller, engine.WithHostRetryBudget(time.Microsecond))

	var got struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if !strings.Contains(got.Message, "exceeds the host-retry budget") {
		t.Errorf("the rejection read %q, want it to name the policy as the reason", got.Message)
	}
	if !strings.Contains(got.Message, "cleat#4032") {
		t.Errorf("the rejection read %q, want it to name the missing fallback so a caller knows why this differs from Go", got.Message)
	}
	if n := caller.count("payments/always-fails"); n != 0 {
		t.Errorf("the service was called %d time(s) for a policy the host refused; a refusal consumes no attempt", n)
	}
}

// TestTypeScriptCallRetryReportsExhaustion: the terminal message is Go's and Rust's, which is what the worker dead-letters on.
func TestTypeScriptCallRetryReportsExhaustion(t *testing.T) {
	wasm := buildCalls(t)
	caller := newScriptedCaller()
	raw, _ := runCalls(t, wasm, "calls_retry_exhausted", caller)

	var got struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if !strings.Contains(got.Message, "retries exhausted: the service refused") {
		t.Errorf("the rejection read %q, want the host loop's \"retries exhausted: \" prefix and the service's last error", got.Message)
	}
	if n := caller.count("payments/always-fails"); n != 2 {
		t.Errorf("the service was called %d time(s), want exactly maxAttempts = 2", n)
	}
}

// TestTypeScriptCallHeartbeatAnswers: the heartbeat path returns the service's value with the interval crossing intact.
func TestTypeScriptCallHeartbeatAnswers(t *testing.T) {
	wasm := buildCalls(t)
	caller := newScriptedCaller()
	raw, _ := runCalls(t, wasm, "calls_heartbeat", caller)

	var got struct {
		Out struct {
			Alive bool `json:"alive"`
		} `json:"out"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if !got.Out.Alive {
		t.Errorf("callHeartbeat resolved with %s, want the service's {alive:true}", raw)
	}
	if n := caller.count("payments/slow"); n != 1 {
		t.Errorf("the service was called %d time(s), want 1 -- a heartbeat is not a retry", n)
	}
}

// TestTypeScriptSendIsRecordedAndNotWaitedFor: the send is a recorded event, and the workflow finishes without the service's
// answer. The caller runs on a goroutine, so the ASSERTION IS ON THE HISTORY: waiting for the goroutine would make the test
// depend on scheduling rather than on the binding.
func TestTypeScriptSendIsRecordedAndNotWaitedFor(t *testing.T) {
	wasm := buildCalls(t)
	caller := newScriptedCaller()
	raw, history := runCalls(t, wasm, "calls_send", caller)

	var got struct {
		Reached bool `json:"reached"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if !got.Reached {
		t.Errorf("the workflow did not reach its end: send must not wait for the service")
	}
	events := eventsOfType(history, engine.EventTypeDurableSend)
	if len(events) != 1 {
		t.Fatalf("the history holds %d durable_send events, want 1", len(events))
	}
	if events[0].Service != "email" || events[0].Op != "notify" {
		t.Errorf("the recorded send names %q.%q, want email.notify", events[0].Service, events[0].Op)
	}
	if !strings.Contains(events[0].Request, "someone@example.test") {
		t.Errorf("the recorded request is %q, want the payload the workflow sent", events[0].Request)
	}
}

// TestTypeScriptScheduleInvokeIsRecorded: the delayed invocation is a recorded event, with the delay the workflow asked for.
func TestTypeScriptScheduleInvokeIsRecorded(t *testing.T) {
	wasm := buildCalls(t)
	caller := newScriptedCaller()
	raw, history := runCalls(t, wasm, "calls_schedule", caller)

	var got struct {
		Reached bool `json:"reached"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the result is not the object this test expects: %v\n%.400s", err, raw)
	}
	if !got.Reached {
		t.Errorf("the workflow did not reach its end: scheduleInvoke must not wait")
	}
	events := eventsOfType(history, engine.EventTypeDurableScheduleInvoke)
	if len(events) != 1 {
		t.Fatalf("the history holds %d durable_schedule_invoke events, want 1", len(events))
	}
	if events[0].Service != "email" || events[0].Op != "digest" {
		t.Errorf("the recorded invocation names %q.%q, want email.digest", events[0].Service, events[0].Op)
	}
	if events[0].DurationMs != 250 {
		t.Errorf("the recorded delay is %dms, want 250: the delay is the one argument a binding can transpose", events[0].DurationMs)
	}
}

// eventsOfType filters the history by event type.
func eventsOfType(history []engine.EventRecord, want engine.EventType) []engine.EventRecord {
	var out []engine.EventRecord
	for _, ev := range history {
		if ev.EventType == want {
			out = append(out, ev)
		}
	}
	return out
}

// countCallEvents counts the recorded service calls, which is one per attempt on the SDK path and one for the whole policy on
// the host path.
func countCallEvents(history []engine.EventRecord) int {
	n := 0
	for _, ev := range history {
		if ev.EventType == engine.EventTypeCall {
			n++
		}
	}
	return n
}

// replayUntilDone replays until the workflow stops suspending, sleeping briefly between passes so a durable sleep's wake time
// is actually reached. Bounded, so a workflow that never finishes fails the test rather than hanging it.
func replayUntilDone(t *testing.T, e *engine.Engine, wasm []byte, entry string, history []engine.EventRecord) string {
	t.Helper()
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		result, next, susp, _, _, err := e.Replay(context.Background(), wasm, entry, json.RawMessage(`{}`), history)
		if err != nil {
			t.Fatalf("Replay %d: %v", i+1, err)
		}
		if susp == nil {
			return string(result)
		}
		history = next
	}
	t.Fatalf("the workflow was still suspending after 50 replays; the SDK-level retry loop is not making progress")
	return ""
}

// TestTypeScriptACallVariantAfterASuspensionRecordsNothing is the group's late-call guard (groups/README.md convention 3),
// and it pins ALL FOUR bindings: unguarding any one of them records an event after the suspension and turns this red.
//
// Measured one binding at a time, with REBUILD_PLUGIN=1 (the first three here, `send` by review):
//
//	__cleat_call_retry        -> call event, payments/charge
//	__cleat_call_heartbeat    -> call event, payments/slow
//	__cleat_send              -> durable_send event
//	__cleat_schedule_invoke   -> durable_schedule_invoke event, email/digest
//
// THE FIRST VERSION OF THIS COMMENT SAID THE OPPOSITE -- that the engine refused `send`, `callHeartbeat` and `scheduleInvoke`
// after a suspension and their guards were unobservable -- and it was wrong twice over. `stopBeforeNewWork` is a DEFER-PHASE
// and shutdown condition (`engine/durablecalls.go`), not "this segment suspended", so those three record normally after a
// suspension. And the "measured" empty history behind the claim came from a mutation that never applied: it was a string
// replacement against text `cargo fmt` had since reflowed, so the binding stayed guarded, the probe printed nothing, and the
// green run was the baseline wearing the mutation's clothes. A mutation that does not apply is not a measurement.
func TestTypeScriptACallVariantAfterASuspensionRecordsNothing(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/late_calls.ts"
	res := build(t, root, src, "late_calls")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	caller := newScriptedCaller()
	e, closeRT := newCallsEngine(t, caller)
	defer closeRT()

	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "late_calls", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the workflow: %v", err)
	}
	// The precondition: the segment really did suspend, at the sleep.
	if susp == nil {
		t.Fatalf("the workflow did not suspend; ctx.sleep(60000) is supposed to suspend the segment")
	}
	if !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("suspended for %q, not at the sleep; the ordering below is then unmeasured", susp.Reason)
	}
	// Every event type this group can record, so a binding that is guarded everywhere except one call site is caught: each of
	// the four has its own `guarded`, and three were invisible to the first version of this test.
	for _, want := range []engine.EventType{
		engine.EventTypeCall,
		engine.EventTypeDurableSend,
		engine.EventTypeDurableScheduleInvoke,
	} {
		if events := eventsOfType(history, want); len(events) != 0 {
			t.Fatalf("the segment recorded a %s event (%+v) after the call that suspended it", want, events[0])
		}
	}
	for _, key := range []string{"payments/charge", "payments/slow", "email/notify", "email/digest"} {
		if n := caller.count(key); n != 0 {
			t.Errorf("the service %q was called %d time(s) after the segment suspended; that binding must be guarded", key, n)
		}
	}
}
