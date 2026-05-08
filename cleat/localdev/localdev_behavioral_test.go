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
	if r.H() == nil {
		t.Error("H() returned nil")
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
