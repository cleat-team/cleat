package localdev

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rcownie/cleat/cleat"
)

// =========================================================================
// Helpers
// =========================================================================

type stubCaller struct {
	fn func(ctx context.Context, svc, op, req string) (string, error)
}

func (s *stubCaller) Call(ctx context.Context, svc, op, req string) (string, error) {
	return s.fn(ctx, svc, op, req)
}

var _ ServiceCaller = (*stubCaller)(nil)

// =========================================================================
// Construction and options
// =========================================================================

func TestLR_New_CreatesWithDefaults(t *testing.T) {
	r := NewLocalRunner()
	if r == nil {
		t.Fatal("NewLocalRunner returned nil")
	}
	if r.H().HostCallsImpl == nil {
		t.Error("H() returned nil HostCallsImpl")
	}
	if r.Events() == nil {
		t.Error("Events() returned nil")
	}
}

func TestLR_New_WithWorkflowID(t *testing.T) {
	r := NewLocalRunner(WithWorkflowID("my-wf-id"))
	if r.workflowID != "my-wf-id" {
		t.Errorf("workflowID: want my-wf-id, got %q", r.workflowID)
	}
}

func TestLR_New_WithServiceCaller(t *testing.T) {
	called := false
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		called = true
		return "ok", nil
	}}
	r := NewLocalRunner(WithServiceCaller(caller))
	resp, err := r.durableCall("test-svc", "test-op", "{}")
	if err != nil {
		t.Fatalf("durableCall: %v", err)
	}
	if resp != "ok" {
		t.Errorf("want ok, got %q", resp)
	}
	if !called {
		t.Error("expected caller to be invoked")
	}
}

func TestLR_New_WithLogWriter(t *testing.T) {
	var buf strings.Builder
	r := NewLocalRunner(WithLogWriter(&buf))
	r.durableLog("test message")
	if !strings.Contains(buf.String(), "test message") {
		t.Errorf("expected log message, got %q", buf.String())
	}
}

// =========================================================================
// Events recording
// =========================================================================

func TestLR_Events_RecordsDurableCall(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return "resp", nil
	}}
	r := NewLocalRunner(WithServiceCaller(caller))
	_, err := r.durableCall("mysvc", "myop", `{"a":1}`)
	if err != nil {
		t.Fatalf("durableCall: %v", err)
	}
	events := r.Events()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Type != "call" || events[0].Service != "mysvc" || events[0].Operation != "myop" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

func TestLR_Events_NoEventOnCallerNil(t *testing.T) {
	r := NewLocalRunner()
	_, err := r.durableCall("svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error without ServiceCaller")
	}
	// When caller is nil, no event is recorded (call fails before recording).
	events := r.Events()
	if len(events) != 0 {
		t.Errorf("expected no events when caller is nil, got %d", len(events))
	}
}

// =========================================================================
// DurableLog, elapsed
// =========================================================================

func TestLR_DurableLog(t *testing.T) {
	var buf strings.Builder
	r := NewLocalRunner(WithLogWriter(&buf))
	r.durableLog("hello world")
	if !strings.Contains(buf.String(), "hello world") {
		t.Errorf("log missing message: %q", buf.String())
	}
}

func TestLR_Elapsed(t *testing.T) {
	r := NewLocalRunner()
	d := r.elapsed()
	if d < 0 {
		t.Errorf("elapsed should be non-negative, got %v", d)
	}
}

// =========================================================================
// Cancellation
// =========================================================================

func TestLR_Cancellation_InitiallyNotCancelled(t *testing.T) {
	r := NewLocalRunner()
	cancelled, _ := r.pollCancellation()
	if cancelled {
		t.Error("expected not cancelled initially")
	}
}

func TestLR_Cancellation_AfterCancel(t *testing.T) {
	r := NewLocalRunner()
	r.Cancel("test reason")
	cancelled, reason := r.pollCancellation()
	if !cancelled {
		t.Error("expected cancelled after Cancel")
	}
	if reason != "test reason" {
		t.Errorf("want 'test reason', got %q", reason)
	}
}

// =========================================================================
// Signals
// =========================================================================

func TestLR_SendAndPollSignal(t *testing.T) {
	r := NewLocalRunner()
	r.SendSignal("my-signal", `{"key":"value"}`)
	payload, ok, err := r.pollSignal("my-signal")
	if err != nil {
		t.Fatalf("pollSignal: %v", err)
	}
	if !ok {
		t.Fatal("expected signal to be present")
	}
	if payload != `{"key":"value"}` {
		t.Errorf("want payload, got %q", payload)
	}
}

func TestLR_PollSignal_NotPresent(t *testing.T) {
	r := NewLocalRunner()
	_, ok, err := r.pollSignal("nonexistent")
	if err != nil {
		t.Fatalf("pollSignal: %v", err)
	}
	if ok {
		t.Error("expected signal not present")
	}
}

func TestLR_PollSignal_Consumed(t *testing.T) {
	r := NewLocalRunner()
	r.SendSignal("sig1", "first")
	_, ok, _ := r.pollSignal("sig1")
	if !ok {
		t.Fatal("signal should be present")
	}
	_, ok, _ = r.pollSignal("sig1")
	if ok {
		t.Error("signal should be consumed after poll")
	}
}

// =========================================================================
// ContinueAsNew
// =========================================================================

func TestLR_ContinueAsNew_InitiallyEmpty(t *testing.T) {
	r := NewLocalRunner()
	input, ok := r.ContinueAsNewInput()
	if ok || input != "" {
		t.Errorf("expected no continue-as-new input, got ok=%v input=%q", ok, input)
	}
}

func TestLR_ContinueAsNew_AfterSet(t *testing.T) {
	r := NewLocalRunner()
	r.continueAsNew(`{"new":"input"}`)
	input, ok := r.ContinueAsNewInput()
	if !ok {
		t.Error("expected continue-as-new input present")
	}
	if input != `{"new":"input"}` {
		t.Errorf("want new input, got %q", input)
	}
}

// =========================================================================
// version / minVersion / nowMs / random
// =========================================================================

func TestLR_Version(t *testing.T) {
	r := NewLocalRunner()
	if r.version() <= 0 {
		t.Errorf("version should be > 0, got %d", r.version())
	}
}

func TestLR_MinVersion(t *testing.T) {
	r := NewLocalRunner()
	if r.minVersion() <= 0 {
		t.Errorf("minVersion should be > 0, got %d", r.minVersion())
	}
}

func TestLR_NowMs(t *testing.T) {
	r := NewLocalRunner()
	if r.nowMs() <= 0 {
		t.Errorf("nowMs should be > 0")
	}
}

func TestLR_Random(t *testing.T) {
	r := NewLocalRunner()
	r1 := r.random()
	r2 := r.random()
	if r1 == r2 && r1 == 0 {
		t.Logf("random returned same value twice (%d), may be flaky", r1)
	}
}

// =========================================================================
// Concurrency keys
// =========================================================================

func TestLR_AcquireConcurrencyKey_Success(t *testing.T) {
	r := NewLocalRunner()
	acquired, err := r.AcquireConcurrencyKey("my-lock", "test-wf", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Error("expected first acquisition to succeed")
	}
}

func TestLR_AcquireConcurrencyKey_DuplicateFails(t *testing.T) {
	r := NewLocalRunner()
	r.AcquireConcurrencyKey("my-lock", "wf-1", 0)
	// Same workflow re-acquiring same key is OK (idempotent).
	acquiredSame, _ := r.AcquireConcurrencyKey("my-lock", "wf-1", 0)
	if !acquiredSame {
		t.Error("re-acquire by same workflow should succeed (idempotent)")
	}
	// Different workflow trying same key should fail.
	acquiredDiff, err := r.AcquireConcurrencyKey("my-lock", "wf-2", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquiredDiff {
		t.Error("different workflow should not acquire already-held key")
	}
}

func TestLR_ReleaseConcurrencyKeys(t *testing.T) {
	r := NewLocalRunner()
	_, _ = r.AcquireConcurrencyKey("lock-a", "test-wf", 0)
	_, _ = r.AcquireConcurrencyKey("lock-b", "test-wf", 0)
	r.ReleaseConcurrencyKeys("test-wf")

	acquired, err := r.AcquireConcurrencyKey("lock-a", "test-wf2", 0)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if !acquired {
		t.Error("expected re-acquisition to succeed after release")
	}
}

// =========================================================================
// DurableSleepMs
// =========================================================================

func TestLR_DurableSleepMs_RecordsEvent(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	r.durableSleepMs(1)
	events := r.Events()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Type != "sleep" {
		t.Errorf("event type: want sleep, got %q", events[0].Type)
	}
	if !strings.Contains(events[0].Message, "1ms") {
		t.Errorf("message should mention duration: %q", events[0].Message)
	}
}

// =========================================================================
// DurableAwaitSignals
// =========================================================================

func TestLR_DurableAwaitSignals_Timeout(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	name, payload, timedOut, err := r.durableAwaitSignals([]string{"s1"}, 1)
	if err != nil {
		t.Fatalf("durableAwaitSignals: %v", err)
	}
	if !timedOut {
		t.Error("expected timeout")
	}
	if name != "" || payload != "" {
		t.Errorf("expected empty name/payload on timeout, got %q / %q", name, payload)
	}
}

func TestLR_DurableAwaitSignals_SignalArrives(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	done := make(chan struct{})
	go func() {
		time.Sleep(5 * time.Millisecond)
		r.SendSignal("s1", "p1")
		close(done)
	}()
	name, payload, timedOut, err := r.durableAwaitSignals([]string{"s1"}, 5000)
	if err != nil {
		t.Fatalf("durableAwaitSignals: %v", err)
	}
	if timedOut {
		t.Fatal("expected signal, got timeout")
	}
	if name != "s1" {
		t.Errorf("want s1, got %q", name)
	}
	if payload != "p1" {
		t.Errorf("want p1, got %q", payload)
	}
	<-done
}

// =========================================================================
// Child workflow stubs
// =========================================================================

type stubChildRunner struct {
	fn func(ctx context.Context, name, input string) (string, error)
}

func (s *stubChildRunner) RunChild(ctx context.Context, name, input string) (string, error) {
	return s.fn(ctx, name, input)
}

var _ ChildWorkflowRunner = (*stubChildRunner)(nil)

func TestLR_ChildWorkflow_WithRunner(t *testing.T) {
	called := false
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		called = true
		return `{"result":"ok"}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	runID, err := r.childWorkflow("my-child", `{"in":1}`)
	if err != nil {
		t.Fatalf("childWorkflow: %v", err)
	}
	if !called {
		t.Error("child runner was not invoked")
	}
	if runID == "" {
		t.Error("expected non-empty runID")
	}
	r.mu.RLock()
	cr, ok := r.childResults[runID]
	r.mu.RUnlock()
	if !ok {
		t.Fatal("child result not stored")
	}
	if cr.err != nil {
		t.Errorf("unexpected error: %v", cr.err)
	}
	if cr.result != `{"result":"ok"}` {
		t.Errorf("want result, got %q", cr.result)
	}
}

func TestLR_ChildWorkflow_WithoutRunner(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	runID, err := r.childWorkflow("orphan", `{}`)
	if err != nil {
		t.Fatalf("childWorkflow: %v", err)
	}
	r.mu.RLock()
	_, ok := r.childResults[runID]
	r.mu.RUnlock()
	if ok {
		t.Error("expected no child result without runner")
	}
	events := r.Events()
	if len(events) == 0 {
		t.Error("expected events to be recorded")
	}
}

func TestLR_ChildWorkflowTyped_Marshals(t *testing.T) {
	var capturedInput string
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		capturedInput = input
		return `{"result":"ok"}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	runID, err := r.childWorkflowTyped("typed-child", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("childWorkflowTyped: %v", err)
	}
	if runID == "" {
		t.Error("expected non-empty runID")
	}
	if !strings.Contains(capturedInput, `"key"`) || !strings.Contains(capturedInput, `"val"`) {
		t.Errorf("marshaled input should contain key/val, got %q", capturedInput)
	}
}

// =========================================================================
// DurableDefer
// =========================================================================

func TestLR_DurableDefer_ReturnsID(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	id, err := r.durableDefer("task1")
	if err != nil {
		t.Fatalf("durableDefer: %v", err)
	}
	if id != "defer-1" {
		t.Errorf("want defer-1, got %q", id)
	}
	id2, _ := r.durableDefer("task2")
	if id2 != "defer-2" {
		t.Errorf("want defer-2, got %q", id2)
	}
	events := r.Events()
	found := false
	for _, e := range events {
		if e.Type == "defer" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected defer event")
	}
}

// =========================================================================
// DurableCallWithOptions
// =========================================================================

func TestLR_DurableCallWithOptions_NoRetry(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return "ok", nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	resp, err := r.durableCallWithOptions(cleat.CallOptions{}, "svc", "op", `{}`)
	if err != nil {
		t.Fatalf("durableCallWithOptions: %v", err)
	}
	if resp != "ok" {
		t.Errorf("want ok, got %q", resp)
	}
}

func TestLR_DurableCallWithOptions_WithRetry(t *testing.T) {
	attempt := 0
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		attempt++
		if attempt == 1 {
			return "", fmt.Errorf("transient")
		}
		return "ok", nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	opts := cleat.CallOptions{
		Retry: &cleat.RetryPolicy{
			MaxAttempts:        2,
			InitialInterval:    1,
			BackoffCoefficient: 1.0,
			MaxInterval:        1,
		},
	}
	resp, err := r.durableCallWithOptions(opts, "svc", "op", `{}`)
	if err != nil {
		t.Fatalf("durableCallWithOptions: %v", err)
	}
	if resp != "ok" {
		t.Errorf("want ok, got %q", resp)
	}
	if attempt != 2 {
		t.Errorf("expected 2 attempts, got %d", attempt)
	}
}

// =========================================================================
// DurableCallWithHeartbeat
// =========================================================================

func TestLR_DurableCallWithHeartbeat(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return "heartbeat-resp", nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	resp, err := r.durableCallWithHeartbeat("svc", "op", `{}`, 0, nil)
	if err != nil {
		t.Fatalf("durableCallWithHeartbeat: %v", err)
	}
	if resp != "heartbeat-resp" {
		t.Errorf("want heartbeat-resp, got %q", resp)
	}
}

// =========================================================================
// AwaitChild / AwaitAllChildren
// =========================================================================

func TestLR_AwaitChild_Found(t *testing.T) {
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		return `{"status":"ok"}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	runID, _ := r.childWorkflow("good", `{}`)
	result, err := r.awaitChild(runID)
	if err != nil {
		t.Fatalf("awaitChild: %v", err)
	}
	if result != `{"status":"ok"}` {
		t.Errorf("want result, got %q", result)
	}
}

func TestLR_AwaitChild_Error(t *testing.T) {
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		return "", fmt.Errorf("child error")
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	runID, _ := r.childWorkflow("fail", `{}`)
	result, err := r.awaitChild(runID)
	if err == nil {
		t.Fatal("expected error from awaitChild")
	}
	if !strings.Contains(err.Error(), "child error") {
		t.Errorf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestLR_AwaitChild_NotFound(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	result, err := r.awaitChild("nonexistent-run-id")
	if err != nil {
		t.Fatalf("awaitChild: %v", err)
	}
	if result != `{"status":"completed"}` {
		t.Errorf("want default result, got %q", result)
	}
}

func TestLR_AwaitAllChildren(t *testing.T) {
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		return `{"out":1}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	id1, _ := r.childWorkflow("c1", `{}`)
	id2, _ := r.childWorkflow("c2", `{}`)

	results, err := r.awaitAllChildren([]string{id1, id2})
	if err != nil {
		t.Fatalf("awaitAllChildren: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Result != `{"out":1}` {
		t.Errorf("want result for child 0, got %q", results[0].Result)
	}
	if results[0].Error != "" {
		t.Errorf("unexpected error for child 0: %s", results[0].Error)
	}
}

// =========================================================================
// CreatePromise / RegisterUpdateHandler
// =========================================================================

func TestLR_CreatePromiseImpl(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	id, err := r.createPromiseImpl("my-promise")
	if err != nil {
		t.Fatalf("createPromiseImpl: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty promise ID")
	}
	r.mu.RLock()
	lp, ok := r.promises[id]
	r.mu.RUnlock()
	if !ok {
		t.Fatal("promise not found")
	}
	if lp.name != "my-promise" {
		t.Errorf("want my-promise, got %q", lp.name)
	}
	if lp.status != "pending" {
		t.Errorf("want pending, got %q", lp.status)
	}
	events := r.Events()
	found := false
	for _, e := range events {
		if e.Type == "create_promise" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected create_promise event")
	}
}

func TestLR_RegisterUpdateHandler(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	r.registerUpdateHandler("my-update")
	events := r.Events()
	found := false
	for _, e := range events {
		if e.Type == "register_update_handler" && e.Message == "my-update" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected register_update_handler event")
	}
}

// =========================================================================
// Options
// =========================================================================

func TestLR_WithSignalChannel(t *testing.T) {
	ch := make(chan Signal, 10)
	r := NewLocalRunner(WithLogWriter(io.Discard), WithSignalChannel(ch))
	if r.signalCh != ch {
		t.Error("signalCh was not configured")
	}
}

func TestLR_WithVersion(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard), WithVersion(7, 3))
	if r.versionVal != 7 {
		t.Errorf("versionVal: want 7, got %d", r.versionVal)
	}
	if r.minVersionVal != 3 {
		t.Errorf("minVersionVal: want 3, got %d", r.minVersionVal)
	}
}

func TestLR_WithConcurrencyKey(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard), WithConcurrencyKey("ck-1"))
	if r.concurrencyKey != "ck-1" {
		t.Errorf("concurrencyKey: want ck-1, got %q", r.concurrencyKey)
	}
}

func TestLR_WithChildWorkflowRunner(t *testing.T) {
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		return "", nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	if r.childRunner != runner {
		t.Error("childRunner was not configured")
	}
}

// =========================================================================
// Marshal error path for childWorkflowTyped
// =========================================================================

func TestLR_ChildWorkflowTyped_MarshalError(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	// A channel cannot be marshaled to JSON.
	_, err := r.childWorkflowTyped("bad", make(chan int))
	if err == nil {
		t.Error("expected marshaling error")
	}
	if !strings.Contains(err.Error(), "marshaling") {
		t.Errorf("expected marshaling error, got: %v", err)
	}
}

// =========================================================================
// durableCallTypedWithOptions
// =========================================================================

func TestLR_DurableCallTypedWithOptions_Success(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return `{"name":"hello"}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	type resp struct{ Name string }
	var result resp
	err := r.durableCallTypedWithOptions(cleat.CallOptions{}, "svc", "op", map[string]string{"k": "v"}, &result)
	if err != nil {
		t.Fatalf("durableCallTypedWithOptions: %v", err)
	}
	if result.Name != "hello" {
		t.Errorf("want hello, got %q", result.Name)
	}
}

func TestLR_DurableCallTypedWithOptions_NilResult(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return `{"name":"hello"}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	err := r.durableCallTypedWithOptions(cleat.CallOptions{}, "svc", "op", map[string]string{"k": "v"}, nil)
	if err != nil {
		t.Fatalf("durableCallTypedWithOptions with nil result: %v", err)
	}
}

func TestLR_DurableCallTypedWithOptions_MarshalError(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	err := r.durableCallTypedWithOptions(cleat.CallOptions{}, "svc", "op", make(chan int), nil)
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !strings.Contains(err.Error(), "marshaling") {
		t.Errorf("expected marshaling error, got: %v", err)
	}
}

func TestLR_DurableCallTypedWithOptions_UnmarshalError(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return `[1,2,3]`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	type resp struct{ Name string }
	var result resp
	err := r.durableCallTypedWithOptions(cleat.CallOptions{}, "svc", "op", map[string]string{"k": "v"}, &result)
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if !strings.Contains(err.Error(), "unmarshaling") {
		t.Errorf("expected unmarshaling error, got: %v", err)
	}
}

func TestLR_DurableCallTypedWithOptions_CallError(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return "", fmt.Errorf("upstream failure")
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	err := r.durableCallTypedWithOptions(cleat.CallOptions{}, "svc", "op", map[string]string{"k": "v"}, nil)
	if err == nil {
		t.Fatal("expected call error")
	}
	if !strings.Contains(err.Error(), "upstream failure") {
		t.Errorf("expected upstream failure, got: %v", err)
	}
}

// =========================================================================
// awaitChildTyped
// =========================================================================

func TestLR_AwaitChildTyped_Success(t *testing.T) {
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		return `{"status":"done"}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	runID, _ := r.childWorkflow("child", `{}`)
	type result struct{ Status string }
	var res result
	err := r.awaitChildTyped(runID, &res)
	if err != nil {
		t.Fatalf("awaitChildTyped: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("want done, got %q", res.Status)
	}
}

func TestLR_AwaitChildTyped_NilResult(t *testing.T) {
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		return `{"status":"done"}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	runID, _ := r.childWorkflow("child", `{}`)
	err := r.awaitChildTyped(runID, nil)
	if err == nil {
		t.Error("expected error for nil result (json.Unmarshal(nil))")
	}
}

func TestLR_AwaitChildTyped_ChildError(t *testing.T) {
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		return "", fmt.Errorf("child boom")
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	runID, _ := r.childWorkflow("fail", `{}`)
	var res string
	err := r.awaitChildTyped(runID, &res)
	if err == nil {
		t.Fatal("expected child error")
	}
	if !strings.Contains(err.Error(), "child boom") {
		t.Errorf("expected child boom, got: %v", err)
	}
}

func TestLR_AwaitChildTyped_UnmarshalError(t *testing.T) {
	runner := &stubChildRunner{fn: func(ctx context.Context, name, input string) (string, error) {
		return `not-valid-json-object`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithChildWorkflowRunner(runner))
	runID, _ := r.childWorkflow("bad", `{}`)
	type result struct{ Status string }
	var res result
	err := r.awaitChildTyped(runID, &res)
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

// =========================================================================
// durableCallTypedWithHeartbeat
// =========================================================================

func TestLR_DurableCallTypedWithHeartbeat_Success(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return `{"value":42}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	type resp struct{ Value int }
	var result resp
	err := r.durableCallTypedWithHeartbeat("svc", "op", map[string]string{"k": "v"}, &result, 0, nil)
	if err != nil {
		t.Fatalf("durableCallTypedWithHeartbeat: %v", err)
	}
	if result.Value != 42 {
		t.Errorf("want 42, got %d", result.Value)
	}
}

func TestLR_DurableCallTypedWithHeartbeat_NilResult(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return `{"value":42}`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	err := r.durableCallTypedWithHeartbeat("svc", "op", map[string]string{"k": "v"}, nil, 0, nil)
	if err != nil {
		t.Fatalf("durableCallTypedWithHeartbeat with nil result: %v", err)
	}
}

func TestLR_DurableCallTypedWithHeartbeat_MarshalError(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	err := r.durableCallTypedWithHeartbeat("svc", "op", make(chan int), nil, 0, nil)
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !strings.Contains(err.Error(), "marshaling") {
		t.Errorf("expected marshaling error, got: %v", err)
	}
}

func TestLR_DurableCallTypedWithHeartbeat_CallError(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return "", fmt.Errorf("heartbeat fail")
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	err := r.durableCallTypedWithHeartbeat("svc", "op", map[string]string{"k": "v"}, nil, 0, nil)
	if err == nil {
		t.Fatal("expected call error")
	}
	if !strings.Contains(err.Error(), "heartbeat fail") {
		t.Errorf("expected heartbeat fail, got: %v", err)
	}
}

func TestLR_DurableCallTypedWithHeartbeat_UnmarshalError(t *testing.T) {
	caller := &stubCaller{fn: func(ctx context.Context, svc, op, req string) (string, error) {
		return `not-json`, nil
	}}
	r := NewLocalRunner(WithLogWriter(io.Discard), WithServiceCaller(caller))
	type resp struct{ Value int }
	var result resp
	err := r.durableCallTypedWithHeartbeat("svc", "op", map[string]string{"k": "v"}, &result, 0, nil)
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

// =========================================================================
// setQueryState
// =========================================================================

func TestLR_SetQueryState_SetsValue(t *testing.T) {
	r := NewLocalRunner()
	r.setQueryState("mykey", "myvalue")
	r.mu.RLock()
	val, ok := r.queryState["mykey"]
	r.mu.RUnlock()
	if !ok {
		t.Error("queryState key not set")
	}
	if val != "myvalue" {
		t.Errorf("want myvalue, got %q", val)
	}
}

func TestLR_SetQueryState_Overwrites(t *testing.T) {
	r := NewLocalRunner()
	r.setQueryState("k", "v1")
	r.setQueryState("k", "v2")
	r.mu.RLock()
	val := r.queryState["k"]
	r.mu.RUnlock()
	if val != "v2" {
		t.Errorf("want v2, got %q", val)
	}
}

// =========================================================================
// runDetached
// =========================================================================

func TestLR_RunDetached_Success(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	called := false
	err := r.runDetached(func(h cleat.HostCalls) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("runDetached: %v", err)
	}
	if !called {
		t.Error("detached function was not called")
	}
	events := r.Events()
	found := false
	for _, e := range events {
		if e.Type == "run_detached" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected run_detached event")
	}
}

func TestLR_RunDetached_Error(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	err := r.runDetached(func(h cleat.HostCalls) error {
		return fmt.Errorf("detached failure")
	})
	if err == nil {
		t.Fatal("expected error from detached function")
	}
	if !strings.Contains(err.Error(), "detached failure") {
		t.Errorf("unexpected error: %v", err)
	}
}

// =========================================================================
// awaitPromiseImpl
// =========================================================================

func TestLR_AwaitPromiseImpl_NotFound(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	_, timedOut, err := r.awaitPromiseImpl("nonexistent", time.Millisecond)
	if err == nil {
		t.Fatal("expected error for nonexistent promise")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not found error, got: %v", err)
	}
	if timedOut {
		t.Error("timedOut should be false for non-existent promise")
	}
}

func TestLR_AwaitPromiseImpl_AlreadyResolved(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	id, err := r.createPromiseImpl("test-promise")
	if err != nil {
		t.Fatalf("createPromiseImpl: %v", err)
	}
	// Directly resolve the promise in internal state.
	r.mu.Lock()
	lp := r.promises[id]
	lp.status = "resolved"
	lp.result = "result-value"
	close(lp.ch)
	r.mu.Unlock()

	result, timedOut, err := r.awaitPromiseImpl(id, time.Second)
	if err != nil {
		t.Fatalf("awaitPromiseImpl: %v", err)
	}
	if timedOut {
		t.Error("should not time out on resolved promise")
	}
	if result != "result-value" {
		t.Errorf("want result-value, got %q", result)
	}
}

func TestLR_AwaitPromiseImpl_AlreadyRejected(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	id, err := r.createPromiseImpl("rejected-promise")
	if err != nil {
		t.Fatalf("createPromiseImpl: %v", err)
	}
	// Directly reject the promise in internal state.
	r.mu.Lock()
	lp := r.promises[id]
	lp.status = "rejected"
	lp.errorMsg = "rejection reason"
	close(lp.ch)
	r.mu.Unlock()

	_, timedOut, err := r.awaitPromiseImpl(id, time.Second)
	if err == nil {
		t.Fatal("expected rejection error")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("expected rejection error, got: %v", err)
	}
	if timedOut {
		t.Error("timedOut should be false for rejected promise")
	}
}

func TestLR_AwaitPromiseImpl_PendingTimeout(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	id, err := r.createPromiseImpl("pending-promise")
	if err != nil {
		t.Fatalf("createPromiseImpl: %v", err)
	}
	result, timedOut, err := r.awaitPromiseImpl(id, time.Millisecond)
	if err != nil {
		t.Fatalf("awaitPromiseImpl: %v", err)
	}
	if !timedOut {
		t.Error("expected timeout on pending promise")
	}
	if result != "" {
		t.Errorf("expected empty result on timeout, got %q", result)
	}
}

func TestLR_AwaitPromiseImpl_PendingResolvedViaChannel(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	id, err := r.createPromiseImpl("will-resolve")
	if err != nil {
		t.Fatalf("createPromiseImpl: %v", err)
	}

	done := make(chan struct{})
	var result string
	var timedOut bool
	var awaitErr error

	go func() {
		result, timedOut, awaitErr = r.awaitPromiseImpl(id, time.Second)
		close(done)
	}()

	time.Sleep(5 * time.Millisecond)

	r.mu.Lock()
	lp := r.promises[id]
	lp.status = "resolved"
	lp.result = "channel-resolved"
	close(lp.ch)
	r.mu.Unlock()

	<-done

	if awaitErr != nil {
		t.Fatalf("awaitPromiseImpl: %v", awaitErr)
	}
	if timedOut {
		t.Error("should not time out when resolved via channel")
	}
	if result != "channel-resolved" {
		t.Errorf("want channel-resolved, got %q", result)
	}
}

func TestLR_AwaitPromiseImpl_PendingRejectedViaChannel(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	id, err := r.createPromiseImpl("will-reject")
	if err != nil {
		t.Fatalf("createPromiseImpl: %v", err)
	}

	done := make(chan struct{})
	var timedOut bool
	var awaitErr error

	go func() {
		_, timedOut, awaitErr = r.awaitPromiseImpl(id, time.Second)
		close(done)
	}()

	time.Sleep(5 * time.Millisecond)

	r.mu.Lock()
	lp := r.promises[id]
	lp.status = "rejected"
	lp.errorMsg = "rejected-via-channel"
	close(lp.ch)
	r.mu.Unlock()

	<-done

	if awaitErr == nil {
		t.Fatal("expected rejection error")
	}
	if !strings.Contains(awaitErr.Error(), "rejected-via-channel") {
		t.Errorf("expected rejected-via-channel, got: %v", awaitErr)
	}
	if timedOut {
		t.Error("should not time out when rejected via channel")
	}
}

// =========================================================================
// pluginCallImpl
// =========================================================================

func TestLR_PluginCallImpl_ReturnsError(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	resp, err := r.pluginCallImpl("myplugin", "myfunc", `{}`)
	if err == nil {
		t.Fatal("expected error from pluginCallImpl")
	}
	if resp != "" {
		t.Errorf("expected empty response, got %q", resp)
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("expected 'not available' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "myplugin") || !strings.Contains(err.Error(), "myfunc") {
		t.Errorf("error should mention plugin and function names, got: %v", err)
	}
}

// =========================================================================
// acquireLockImpl / releaseLockImpl
// =========================================================================

func TestLR_AcquireLockImpl_FirstAcquisitionSucceeds(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard), WithWorkflowID("wf-1"))
	ok, err := r.acquireLockImpl("lock-key", 30000)
	if err != nil {
		t.Fatalf("acquireLockImpl: %v", err)
	}
	if !ok {
		t.Error("first acquisition should succeed")
	}
}

func TestLR_AcquireLockImpl_SameWorkflowReacquires(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard), WithWorkflowID("wf-1"))
	ok, _ := r.acquireLockImpl("lock-key", 30000)
	if !ok {
		t.Fatal("first acquisition should succeed")
	}
	ok, err := r.acquireLockImpl("lock-key", 30000)
	if err != nil {
		t.Fatalf("acquireLockImpl: %v", err)
	}
	if !ok {
		t.Error("re-acquire by same workflow should succeed (idempotent)")
	}
}

func TestLR_AcquireLockImpl_DifferentWorkflowFails(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard), WithWorkflowID("wf-1"))
	ok, _ := r.acquireLockImpl("lock-key", 30000)
	if !ok {
		t.Fatal("first acquisition should succeed")
	}
	// Directly change workflow ID to simulate different workflow.
	r.workflowID = "wf-2"
	ok, err := r.acquireLockImpl("lock-key", 30000)
	if err != nil {
		t.Fatalf("acquireLockImpl: %v", err)
	}
	if ok {
		t.Error("different workflow should not acquire held key")
	}
}

func TestLR_ReleaseLockImpl_ReleasesKey(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard), WithWorkflowID("wf-1"))
	ok, _ := r.acquireLockImpl("lock-key", 30000)
	if !ok {
		t.Fatal("first acquisition should succeed")
	}
	err := r.releaseLockImpl("lock-key")
	if err != nil {
		t.Fatalf("releaseLockImpl: %v", err)
	}
	// After release, a different workflow should be able to acquire.
	r.workflowID = "wf-2"
	ok, err = r.acquireLockImpl("lock-key", 30000)
	if err != nil {
		t.Fatalf("acquireLockImpl after release: %v", err)
	}
	if !ok {
		t.Error("should acquire after release")
	}
}

func TestLR_ReleaseLockImpl_NonexistentKey(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	err := r.releaseLockImpl("key-that-does-not-exist")
	if err != nil {
		t.Fatalf("releaseLockImpl on nonexistent key: %v", err)
	}
}

// =========================================================================
// awaitConditionImpl
// =========================================================================

func TestLR_AwaitConditionImpl_TrueImmediately(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	met, err := r.awaitConditionImpl(func() bool { return true }, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("awaitConditionImpl: %v", err)
	}
	if !met {
		t.Error("expected condition met")
	}
}

func TestLR_AwaitConditionImpl_Timeout(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	met, err := r.awaitConditionImpl(func() bool { return false }, time.Millisecond, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("awaitConditionImpl: %v", err)
	}
	if met {
		t.Error("expected timeout, not condition met")
	}
}
