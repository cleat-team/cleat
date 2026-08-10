package cleattest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// ---------------------------------------------------------------------------
// 2a: Child Workflow Tests
// ---------------------------------------------------------------------------

func TestOnChildWorkflowWithReturn(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("my-child").Return(`{"status":"ok"}`, nil)

	runID, err := env.H().ChildWorkflow("my-child", `{"input":"data"}`)
	if err != nil {
		t.Fatalf("ChildWorkflow failed: %v", err)
	}
	if runID == "" {
		t.Fatal("expected non-empty runID")
	}

	result, err := env.H().AwaitChild(runID)
	if err != nil {
		t.Fatalf("AwaitChild failed: %v", err)
	}
	if result != `{"status":"ok"}` {
		t.Fatalf("expected %q, got %q", `{"status":"ok"}`, result)
	}
}

func TestRegisterChildStub(t *testing.T) {
	env := NewTestEnv()
	env.RegisterChildStub("my-child", "stub-result")

	runID, err := env.H().ChildWorkflow("my-child", "input")
	if err != nil {
		t.Fatalf("ChildWorkflow failed: %v", err)
	}

	result, err := env.H().AwaitChild(runID)
	if err != nil {
		t.Fatalf("AwaitChild failed: %v", err)
	}
	if result != "stub-result" {
		t.Fatalf("expected %q, got %q", "stub-result", result)
	}
}

func TestChildWorkflowCallHistory(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("child-a").Return("result-a", nil)
	env.OnChildWorkflow("child-b").Return("result-b", nil)

	env.H().ChildWorkflow("child-a", `{"req":"a"}`)
	env.H().ChildWorkflow("child-b", `{"req":"b"}`)

	history := env.ChildWorkflowCallHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2 records, got %d", len(history))
	}
	if history[0].Name != "child-a" || history[0].InputJSON != `{"req":"a"}` {
		t.Fatalf("unexpected first record: %+v", history[0])
	}
	if history[1].Name != "child-b" || history[1].InputJSON != `{"req":"b"}` {
		t.Fatalf("unexpected second record: %+v", history[1])
	}
}

func TestChildWorkflowWithHandler(t *testing.T) {
	env := NewTestEnv()

	// Register a handler (takes priority over stubs).
	env.RegisterChildWorkflow("my-child", func(inputJSON string) (string, error) {
		return `{"handler":"processed"}`, nil
	})

	// Register a stub (should be overridden by handler above).
	env.OnChildWorkflow("my-child").Return(`{"stub":"value"}`, nil)

	runID, err := env.H().ChildWorkflow("my-child", "input")
	if err != nil {
		t.Fatalf("ChildWorkflow failed: %v", err)
	}

	result, err := env.H().AwaitChild(runID)
	if err != nil {
		t.Fatalf("AwaitChild failed: %v", err)
	}
	if result != `{"handler":"processed"}` {
		t.Fatalf("expected handler result %q, got %q", `{"handler":"processed"}`, result)
	}
}

func TestChildWorkflowHandlerReturnsError(t *testing.T) {
	env := NewTestEnv()

	env.RegisterChildWorkflow("my-child", func(inputJSON string) (string, error) {
		return "", fmt.Errorf("child failed")
	})

	runID, err := env.H().ChildWorkflow("my-child", "input")
	if err != nil {
		t.Fatalf("ChildWorkflow failed: %v", err)
	}

	_, err = env.H().AwaitChild(runID)
	if err == nil {
		t.Fatal("expected error from AwaitChild")
	}
	if err.Error() != "child failed" {
		t.Fatalf("expected 'child failed', got %q", err.Error())
	}
}

func TestChildWorkflowStubReturnsError(t *testing.T) {
	env := NewTestEnv()

	env.OnChildWorkflow("failing-child").Return("", fmt.Errorf("stub-error"))

	runID, err := env.H().ChildWorkflow("failing-child", "input")
	if err != nil {
		t.Fatalf("ChildWorkflow failed: %v", err)
	}

	_, err = env.H().AwaitChild(runID)
	if err == nil {
		t.Fatal("expected error from AwaitChild")
	}
	if err.Error() != "stub-error" {
		t.Fatalf("expected 'stub-error', got %q", err.Error())
	}
}

func TestAwaitAllChildren(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("child-a").Return("result-a", nil)
	env.OnChildWorkflow("child-b").Return("result-b", nil)

	runID1, _ := env.H().ChildWorkflow("child-a", "in1")
	runID2, _ := env.H().ChildWorkflow("child-b", "in2")

	results, err := env.H().AwaitAllChildren([]string{runID1, runID2})
	if err != nil {
		t.Fatalf("AwaitAllChildren failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Result != "result-a" {
		t.Fatalf("expected result-a, got %q", results[0].Result)
	}
	if results[1].Result != "result-b" {
		t.Fatalf("expected result-b, got %q", results[1].Result)
	}
}

func TestChildWorkflowTyped(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("my-child").Return(`{"status":"ok"}`, nil)

	type inputType struct {
		Value string `json:"value"`
	}

	runID, err := env.H().ChildWorkflowTyped("my-child", inputType{Value: "test"})
	if err != nil {
		t.Fatalf("ChildWorkflowTyped failed: %v", err)
	}
	if runID == "" {
		t.Fatal("expected non-empty runID")
	}
}

func TestAwaitChildTyped(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("my-child").Return(`{"status":"ok","count":42}`, nil)

	runID, _ := env.H().ChildWorkflow("my-child", "input")

	type resultType struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	}
	var result resultType
	err := env.H().AwaitChildTyped(runID, &result)
	if err != nil {
		t.Fatalf("AwaitChildTyped failed: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("expected status=ok, got %q", result.Status)
	}
	if result.Count != 42 {
		t.Fatalf("expected count=42, got %d", result.Count)
	}
}

func TestAwaitChildNeverRegistered(t *testing.T) {
	env := NewTestEnv()

	runID, err := env.H().ChildWorkflow("unregistered", "input")
	if err != nil {
		t.Fatalf("ChildWorkflow failed: %v", err)
	}

	// awaitChildImpl returns default result for unregistered children.
	result, err := env.H().AwaitChild(runID)
	if err != nil {
		t.Fatalf("AwaitChild failed: %v", err)
	}
	if result != `{"status":"completed"}` {
		t.Fatalf("expected default result %q, got %q", `{"status":"completed"}`, result)
	}
}

func TestChildWorkflowWithOptions(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("my-child").Return("opts-result", nil)

	runID, err := env.H().ChildWorkflowWithOptions("my-child", "input", cleat.ChildWorkflowOptions{
		Version:           2,
		ParentClosePolicy: cleat.ParentClosePolicyTerminate,
	})
	if err != nil {
		t.Fatalf("ChildWorkflowWithOptions failed: %v", err)
	}

	result, err := env.H().AwaitChild(runID)
	if err != nil {
		t.Fatalf("AwaitChild failed: %v", err)
	}
	if result != "opts-result" {
		t.Fatalf("expected %q, got %q", "opts-result", result)
	}
}

// ---------------------------------------------------------------------------
// 2b: Promise Tests
// ---------------------------------------------------------------------------

func TestCreatePromiseResolveAwait(t *testing.T) {
	env := NewTestEnv()

	promiseID, err := env.H().CreatePromise("test-promise")
	if err != nil {
		t.Fatalf("CreatePromise failed: %v", err)
	}
	if promiseID == "" {
		t.Fatal("expected non-empty promise ID")
	}

	env.ResolvePromise(promiseID, "resolved-value")

	result, timedOut, err := env.H().AwaitPromise(promiseID, 0)
	if err != nil {
		t.Fatalf("AwaitPromise failed: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout for resolved promise")
	}
	if result != "resolved-value" {
		t.Fatalf("expected %q, got %q", "resolved-value", result)
	}
}

func TestCreatePromiseRejectAwait(t *testing.T) {
	env := NewTestEnv()

	promiseID, err := env.H().CreatePromise("test-promise")
	if err != nil {
		t.Fatalf("CreatePromise failed: %v", err)
	}

	env.RejectPromise(promiseID, "something went wrong")

	_, timedOut, err := env.H().AwaitPromise(promiseID, 0)
	if err == nil {
		t.Fatal("expected error for rejected promise")
	}
	if timedOut {
		t.Fatal("unexpected timeout for rejected promise")
	}
	if err.Error() != "promise rejected: something went wrong" {
		t.Fatalf("expected 'promise rejected: something went wrong', got %v", err)
	}
}

func TestAwaitPromiseTimeout(t *testing.T) {
	env := NewTestEnv()

	promiseID, err := env.H().CreatePromise("pending-promise")
	if err != nil {
		t.Fatalf("CreatePromise failed: %v", err)
	}

	// A pending promise with timeout should advance the clock and return timedOut.
	result, timedOut, err := env.H().AwaitPromise(promiseID, 5*time.Second)
	if err != nil {
		t.Fatalf("AwaitPromise failed: %v", err)
	}
	if !timedOut {
		t.Fatal("expected timeout for pending promise")
	}
	if result != "" {
		t.Fatalf("expected empty result, got %q", result)
	}
}

func TestAwaitPromiseNotFound(t *testing.T) {
	env := NewTestEnv()

	_, _, err := env.H().AwaitPromise("non-existent", 0)
	if err == nil {
		t.Fatal("expected error for non-existent promise")
	}
	if err.Error() != "cleattest: promise non-existent not found" {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestResolvePromiseNoOpForMissing(t *testing.T) {
	env := NewTestEnv()

	// ResolvePromise on a non-existent promise should be a no-op (no panic).
	env.ResolvePromise("non-existent", "value")
	env.RejectPromise("non-existent", "err")
}

// ---------------------------------------------------------------------------
// 2c: Update Tests
// ---------------------------------------------------------------------------

func TestHandleUpdate(t *testing.T) {
	env := NewTestEnv()

	env.H().RegisterUpdateHandler("my_update", func(payload string) (string, error) {
		return `{"result":"updated"}`, nil
	}, func(payload string) error {
		return nil
	})

	result, err := env.HandleUpdate("my_update", `{"input":"data"}`)
	if err != nil {
		t.Fatalf("HandleUpdate failed: %v", err)
	}
	if result != `{"result":"updated"}` {
		t.Fatalf("expected %q, got %q", `{"result":"updated"}`, result)
	}
}

func TestHandleUpdateWithoutHandler(t *testing.T) {
	env := NewTestEnv()

	_, err := env.HandleUpdate("nonexistent", "payload")
	if err == nil {
		t.Fatal("expected error for unregistered update handler")
	}
}

// ---------------------------------------------------------------------------
// 2d: Signal Tests
// ---------------------------------------------------------------------------

func TestSendSignalAndWaitWithReply(t *testing.T) {
	env := NewTestEnv()

	type signalResult struct {
		resp string
		err  error
	}
	resultCh := make(chan signalResult)

	go func() {
		resp, err := env.H().SendSignalAndWait("target", "sig", `{"key":"val"}`, 5*time.Second)
		resultCh <- signalResult{resp, err}
	}()

	// Spin until the signal is available in the pending queue.
	var payload string
	for i := 0; i < 100; i++ {
		var found bool
		payload, found, _ = env.H().PollSignal("sig")
		if found {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if payload == "" {
		t.Fatal("signal not delivered within timeout")
	}

	// Parse the correlation ID embedded by sendSignalAndWaitImpl.
	var sp struct {
		CorrelationID string `json:"_correlation_id"`
	}
	if err := json.Unmarshal([]byte(payload), &sp); err != nil {
		t.Fatalf("failed to parse signal payload: %v", err)
	}
	if sp.CorrelationID == "" {
		t.Fatal("expected correlation ID in signal payload")
	}

	// Reply through the HostCalls interface.
	err := env.H().ReplyToSignal(sp.CorrelationID, "reply-response")
	if err != nil {
		t.Fatalf("ReplyToSignal failed: %v", err)
	}

	result := <-resultCh
	if result.err != nil {
		t.Fatalf("SendSignalAndWait failed: %v", result.err)
	}
	if result.resp != "reply-response" {
		t.Fatalf("expected %q, got %q", "reply-response", result.resp)
	}
}

func TestSendSignalAndWaitTimeout(t *testing.T) {
	env := NewTestEnv()

	// SendSignalAndWait blocks until timeout or response.
	// Use AdvanceTime to trigger the simulated timeout.
	errCh := make(chan error, 1)
	go func() {
		_, err := env.H().SendSignalAndWait("target", "sig", "{}", 10*time.Millisecond)
		errCh <- err
	}()

	// Give the goroutine time to reach the select and create the sleep record
	// before advancing time.
	time.Sleep(50 * time.Millisecond)
	env.AdvanceTime(20 * time.Millisecond)

	err := <-errCh
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSignalWorkflow(t *testing.T) {
	env := NewTestEnv()

	err := env.H().SignalWorkflow("target", "test-signal", "test-payload")
	if err != nil {
		t.Fatalf("SignalWorkflow failed: %v", err)
	}

	payload, found, err := env.H().PollSignal("test-signal")
	if err != nil {
		t.Fatalf("PollSignal failed: %v", err)
	}
	if !found {
		t.Fatal("expected signal to be delivered")
	}
	if payload != "test-payload" {
		t.Fatalf("expected %q, got %q", "test-payload", payload)
	}
}

func TestReplyToSignalUnknownCorrelationID(t *testing.T) {
	env := NewTestEnv()

	err := env.H().ReplyToSignal("nonexistent-corr", "response")
	if err == nil {
		t.Fatal("expected error for unknown correlation ID")
	}
}

// ---------------------------------------------------------------------------
// 2e: Lock / Condition Tests
// ---------------------------------------------------------------------------

func TestAcquireLockRelease(t *testing.T) {
	env := NewTestEnv()

	acquired, err := env.H().AcquireLock("my-key", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLock failed: %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired")
	}

	err = env.H().ReleaseLock("my-key")
	if err != nil {
		t.Fatalf("ReleaseLock failed: %v", err)
	}
}

func TestAcquireLockIdempotent(t *testing.T) {
	env := NewTestEnv()

	acquired, err := env.H().AcquireLock("my-key", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquire: acquired=%v err=%v", acquired, err)
	}

	// Same workflow acquiring the same key again should succeed (idempotent
	// because acquireLockImpl passes "" as workflowID to AcquireConcurrencyKey).
	acquired, err = env.H().AcquireLock("my-key", time.Minute)
	if err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}
	if !acquired {
		t.Fatal("second acquire should succeed (idempotent)")
	}
}

func TestAwaitConditionMetImmediately(t *testing.T) {
	env := NewTestEnv()

	met := env.H().AwaitCondition(func() bool { return true }, time.Second, time.Minute)
	if !met {
		t.Fatal("expected condition to be met immediately")
	}
}

func TestAwaitConditionTimeout(t *testing.T) {
	env := NewTestEnv()

	resultCh := make(chan bool)
	go func() {
		met := env.H().AwaitCondition(func() bool { return false }, 10*time.Millisecond, 100*time.Millisecond)
		resultCh <- met
	}()

	// Give the goroutine time to enter durableSleepImpl.
	time.Sleep(20 * time.Millisecond)

	// Advance time past the deadline.
	env.AdvanceTime(200 * time.Millisecond)

	select {
	case met := <-resultCh:
		if met {
			t.Fatal("expected condition to time out")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for AwaitCondition goroutine")
	}
}

// ---------------------------------------------------------------------------
// 2f: Other Primitives
// ---------------------------------------------------------------------------

func TestContinueAsNew(t *testing.T) {
	env := NewTestEnv()

	err := env.H().ContinueAsNew(`{"input":"data"}`)
	if err != nil {
		t.Fatalf("ContinueAsNew failed: %v", err)
	}
}

func TestSideEffect(t *testing.T) {
	env := NewTestEnv()

	result, err := env.H().SideEffect(func() (string, error) {
		return "computed-value", nil
	})
	if err != nil {
		t.Fatalf("SideEffect failed: %v", err)
	}
	if result != "computed-value" {
		t.Fatalf("expected %q, got %q", "computed-value", result)
	}
}

func TestSideEffectFnError(t *testing.T) {
	env := NewTestEnv()

	_, err := env.H().SideEffect(func() (string, error) {
		return "", fmt.Errorf("fn-error")
	})
	if err == nil {
		t.Fatal("expected error from SideEffect when fn returns error")
	}
	if err.Error() != "fn-error" {
		t.Fatalf("expected 'fn-error', got %v", err)
	}
}

func TestRunDetached(t *testing.T) {
	env := NewTestEnv()

	called := false
	err := env.H().RunDetached(func(h cleat.HostCalls) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("RunDetached failed: %v", err)
	}
	if !called {
		t.Fatal("expected RunDetached to execute the function")
	}
}

func TestOnPluginCallReturn(t *testing.T) {
	env := NewTestEnv()
	env.OnPluginCall("test-plugin", "test-func").Return(`{"result":"plugin-ok"}`, nil)

	result, err := env.H().PluginCall("test-plugin", "test-func", `{"input":"data"}`)
	if err != nil {
		t.Fatalf("PluginCall failed: %v", err)
	}
	if result != `{"result":"plugin-ok"}` {
		t.Fatalf("expected %q, got %q", `{"result":"plugin-ok"}`, result)
	}
}

func TestOnPluginCallReturnWithError(t *testing.T) {
	env := NewTestEnv()
	env.OnPluginCall("test-plugin", "test-func").Return("", fmt.Errorf("plugin-error"))

	_, err := env.H().PluginCall("test-plugin", "test-func", "input")
	if err == nil {
		t.Fatal("expected error from PluginCall")
	}
	if err.Error() != "plugin-error" {
		t.Fatalf("expected 'plugin-error', got %v", err)
	}
}

func TestPluginCallWithoutStub(t *testing.T) {
	env := NewTestEnv()

	_, err := env.H().PluginCall("unknown", "unknown", "input")
	if err == nil {
		t.Fatal("expected error for unregistered plugin call")
	}
}

func TestDurableLog(t *testing.T) {
	env := NewTestEnv()

	// durableLogImpl is a no-op; verify it doesn't panic.
	env.H().DurableLog("test log message")
}

func TestPollCancellation(t *testing.T) {
	env := NewTestEnv()

	cancelled, reason := env.H().PollCancellation()
	if cancelled {
		t.Fatal("expected no cancellation")
	}
	if reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}
}

func TestSetRetryBehavior(t *testing.T) {
	env := NewTestEnv()
	env.SetRetryBehavior("svc", "op", 2, "final")
	env.OnCall("svc", "op", nil).Return("success-after-retries", nil)

	// First call should fail (per-service/operation retry behavior).
	_, err := env.H().DurableCall("svc", "op", "req")
	if err == nil {
		t.Fatal("expected retry failure on first call")
	}

	// Second call should also fail.
	_, err = env.H().DurableCall("svc", "op", "req")
	if err == nil {
		t.Fatal("expected retry failure on second call")
	}

	// Third call should succeed (falls through to stub matching).
	resp, err := env.H().DurableCall("svc", "op", "req")
	if err != nil {
		t.Fatalf("expected success on third call: %v", err)
	}
	if resp != "success-after-retries" {
		t.Fatalf("expected %q, got %q", "success-after-retries", resp)
	}
}

func TestDurableCallTypedWithHeartbeat(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return(`{"result":"heartbeat-ok"}`, nil)

	type reqType struct {
		Input string `json:"input"`
	}
	type respType struct {
		Result string `json:"result"`
	}

	var resp respType
	err := env.H().DurableCallTypedWithHeartbeat("svc", "op", reqType{Input: "data"}, &resp, time.Second, nil)
	if err != nil {
		t.Fatalf("DurableCallTypedWithHeartbeat failed: %v", err)
	}
	if resp.Result != "heartbeat-ok" {
		t.Fatalf("expected result=heartbeat-ok, got %q", resp.Result)
	}
}

func TestDurableCallTypedWithHeartbeatNoResultPtr(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return(`{"result":"ok"}`, nil)

	// When result is nil, the call should succeed without unmarshaling.
	err := env.H().DurableCallTypedWithHeartbeat("svc", "op", struct{}{}, nil, time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Additional: TestEnv options and edge coverage
// ---------------------------------------------------------------------------

func TestNewTestEnvWithOptions(t *testing.T) {
	env := NewTestEnv(WithRetrySimulation(3))
	// Verify WithRetrySimulation was applied by checking retry behavior.
	env.OnCall("svc", "op", nil).Return("ok", nil)

	_, err := env.H().DurableCall("svc", "op", "req")
	if err == nil {
		t.Fatal("expected retry from WithRetrySimulation(3)")
	}
}

func TestChildWorkflowFromOnChildWorkflowBuilder(t *testing.T) {
	// This test exercises the OnChildWorkflow builder path (line 696-702).
	env := NewTestEnv()
	builder := env.OnChildWorkflow("builder-child")
	if builder == nil {
		t.Fatal("OnChildWorkflow returned nil")
	}
	builder.Return("builder-result", nil)

	runID, _ := env.H().ChildWorkflow("builder-child", "input")
	result, _ := env.H().AwaitChild(runID)
	if result != "builder-result" {
		t.Fatalf("expected %q, got %q", "builder-result", result)
	}
}

func TestChildWorkflowCallHistoryErrorRecord(t *testing.T) {
	env := NewTestEnv()

	env.RegisterChildWorkflow("err-child", func(inputJSON string) (string, error) {
		return "", fmt.Errorf("wf-error")
	})

	env.H().ChildWorkflow("err-child", "input")
	history := env.ChildWorkflowCallHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 record, got %d", len(history))
	}
	if history[0].Err == nil {
		t.Fatal("expected non-nil error in call history")
	}
	if history[0].Err.Error() != "wf-error" {
		t.Fatalf("expected error 'wf-error', got %v", history[0].Err)
	}
}

func TestAwaitConditionViaHostCalls(t *testing.T) {
	// Test the hostCallsImpl.AwaitCondition path calling through to
	// awaitConditionImpl.
	env := NewTestEnv()

	// Condition that is immediately true.
	var mu sync.Mutex
	state := false

	ch := make(chan bool)
	go func() {
		met := env.H().AwaitCondition(func() bool {
			mu.Lock()
			defer mu.Unlock()
			return state
		}, 10*time.Millisecond, 5*time.Second)
		ch <- met
	}()

	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	state = true
	mu.Unlock()

	env.AdvanceTime(50 * time.Millisecond)

	select {
	case met := <-ch:
		if !met {
			t.Fatal("expected condition to be met")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for AwaitCondition")
	}
}

func TestAcquireLockAcquireConcurrencyKey(t *testing.T) {
	// Test acquireLockImpl via H().AcquireLockMs (alternate entry point).
	env := NewTestEnv()

	acquired, err := env.H().AcquireLockMs("lock-key", 30000)
	if err != nil {
		t.Fatalf("AcquireLockMs failed: %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired")
	}
}

func TestRegisterChildWorkflowThenChildWorkflow(t *testing.T) {
	env := NewTestEnv()
	env.RegisterChildWorkflow("handler-child", func(inputJSON string) (string, error) {
		return "handler-output", nil
	})

	runID, err := env.H().ChildWorkflow("handler-child", "input")
	if err != nil {
		t.Fatalf("ChildWorkflow failed: %v", err)
	}

	result, err := env.H().AwaitChild(runID)
	if err != nil {
		t.Fatalf("AwaitChild failed: %v", err)
	}
	if result != "handler-output" {
		t.Fatalf("expected 'handler-output', got %q", result)
	}
}

func TestPluginCallMultipleStubs(t *testing.T) {
	env := NewTestEnv()
	env.OnPluginCall("plugin-a", "func-a").Return("result-a", nil)
	env.OnPluginCall("plugin-b", "func-b").Return("result-b", nil)

	r1, _ := env.H().PluginCall("plugin-a", "func-a", "in")
	r2, _ := env.H().PluginCall("plugin-b", "func-b", "in")

	if r1 != "result-a" {
		t.Fatalf("expected result-a, got %q", r1)
	}
	if r2 != "result-b" {
		t.Fatalf("expected result-b, got %q", r2)
	}
}

// durableCallTypedWithHeartbeatImpl is never reached through hostCallsImpl
// (which always takes a fallback path), so we test it directly.
func TestDurableCallTypedWithHeartbeatImplDirect(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return(`{"result":"direct-ok"}`, nil)

	type reqType struct {
		X int `json:"x"`
	}
	type respType struct {
		Result string `json:"result"`
	}

	var resp respType
	err := env.durableCallTypedWithHeartbeatImpl("svc", "op", reqType{X: 42}, &resp, time.Second, nil)
	if err != nil {
		t.Fatalf("durableCallTypedWithHeartbeatImpl failed: %v", err)
	}
	if resp.Result != "direct-ok" {
		t.Fatalf("expected result=direct-ok, got %q", resp.Result)
	}
}

func TestDurableCallTypedWithHeartbeatImplDirectNilResult(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return("ignored", nil)

	err := env.durableCallTypedWithHeartbeatImpl("svc", "op", struct{}{}, nil, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDurableCallTypedWithHeartbeatImplDirectMarshalError(t *testing.T) {
	env := NewTestEnv()

	// An un-marshalable request should cause an error.
	err := env.durableCallTypedWithHeartbeatImpl("svc", "op", func() {}, nil, 0, nil)
	if err == nil {
		t.Fatal("expected marshal error")
	}
}

// ---------------------------------------------------------------------------
// AwaitAnyChild / PollChild
//
// These were the two child host calls TestEnv never wired into
// HostCallsOptions, so h.awaitAnyChild and h.pollChild were nil and every call
// returned "AwaitAnyChild can only be called from within a workflow function
// (the HostCalls runtime was not initialized)" -- regardless of context. That
// is what made all six examples/dag tests red for as long as they existed, and
// it meant no external SDK user could test a workflow using either call.
// TestEveryHostCallIsWired below is the guard against the next one.
// ---------------------------------------------------------------------------

func TestAwaitAnyChild_PicksLowestSortedRunID(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("c").Return("result-c", nil)
	env.OnChildWorkflow("a").Return("result-a", nil)
	env.OnChildWorkflow("b").Return("result-b", nil)

	runIDs := make([]string, 0, 3)
	for _, name := range []string{"c", "a", "b"} {
		runID, err := env.H().ChildWorkflow(name, "{}")
		if err != nil {
			t.Fatalf("ChildWorkflow(%s): %v", name, err)
		}
		runIDs = append(runIDs, runID)
	}

	// All three have already resolved, so the winner is decided purely by the
	// polling order. engine/children.go sorts the run IDs before polling so a
	// replay picks the same winner as the original execution; this harness has
	// to agree with it or a test will disagree with production about which
	// child came back first.
	sorted := append([]string(nil), runIDs...)
	sort.Strings(sorted)

	// Hand them over in an order that is not the sorted one, so honouring
	// argument order and honouring sorted order give different answers. If
	// runIDs happen to already be sorted the assertion still holds but stops
	// discriminating, so fail loudly rather than pass vacuously.
	shuffled := []string{runIDs[2], runIDs[0], runIDs[1]}
	if shuffled[0] == sorted[0] {
		t.Fatalf("test is vacuous: shuffled order already leads with the lowest run ID (%v)", runIDs)
	}

	got, result, err := env.H().AwaitAnyChild(shuffled)
	if err != nil {
		t.Fatalf("AwaitAnyChild: %v", err)
	}
	if got != sorted[0] {
		t.Errorf("AwaitAnyChild picked %q, want the lowest sorted run ID %q (given %v)", got, sorted[0], shuffled)
	}
	if result == "" {
		t.Error("AwaitAnyChild returned an empty result")
	}
}

func TestAwaitAnyChild_ReturnsRunIDAlongsideAChildError(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("failing").Return("", fmt.Errorf("boom"))

	runID, err := env.H().ChildWorkflow("failing", "{}")
	if err != nil {
		t.Fatalf("ChildWorkflow: %v", err)
	}

	// The run ID must come back even on the error path. cleat/dagrun/dagrun.go
	// needs it to say WHICH task failed; without it the DAG could only report
	// the child's bare message.
	got, _, awaitErr := env.H().AwaitAnyChild([]string{runID})
	if awaitErr == nil {
		t.Fatal("expected the child's error to propagate")
	}
	if got != runID {
		t.Errorf("AwaitAnyChild returned run ID %q on the error path, want %q", got, runID)
	}
}

func TestAwaitAnyChild_EmptyRunIDsIsAnError(t *testing.T) {
	env := NewTestEnv()
	if _, _, err := env.H().AwaitAnyChild(nil); err == nil {
		t.Fatal("expected an error for an empty run ID set, not a silent zero value")
	}
}

func TestPollChild_ReportsCompletedRunningAndFailed(t *testing.T) {
	env := NewTestEnv()
	env.OnChildWorkflow("ok").Return("result-ok", nil)
	env.OnChildWorkflow("bad").Return("", fmt.Errorf("boom"))

	okID, err := env.H().ChildWorkflow("ok", "{}")
	if err != nil {
		t.Fatalf("ChildWorkflow(ok): %v", err)
	}
	badID, err := env.H().ChildWorkflow("bad", "{}")
	if err != nil {
		t.Fatalf("ChildWorkflow(bad): %v", err)
	}

	if status, result, err := env.H().PollChild(okID); err != nil || status != "completed" || result != "result-ok" {
		t.Errorf("PollChild(ok) = (%q, %q, %v), want (completed, result-ok, nil)", status, result, err)
	}
	if status, _, err := env.H().PollChild(badID); err == nil || status != "failed" {
		t.Errorf("PollChild(bad) = (%q, _, %v), want (failed, _, non-nil)", status, err)
	}
	// An unknown child is "running", not an error -- a poll loop has to be able
	// to terminate.
	if status, _, err := env.H().PollChild("no-such-run-id"); err != nil || status != "running" {
		t.Errorf("PollChild(unknown) = (%q, _, %v), want (running, _, nil)", status, err)
	}
}

// TestEveryHostCallIsWired is the guard against the next AwaitAnyChild.
//
// A field left out of hostCallsOptions leaves its hook nil, and most of
// cleat/runtime_*.go answers a nil hook with "can only be called from within a
// workflow function (the HostCalls runtime was not initialized)". That message
// is about workflow context and says nothing about the harness, so the failure
// reads as the caller's fault. Six examples/dag tests were red on that message
// for as long as they existed, and nobody could tell from it that cleattest
// simply had not implemented the call.
//
// Measured 2026-08-08 with AwaitAnyChild and PollChild now wired: 58 func
// fields on cleat.HostCallsOptions, 19 still nil. That is not a claim they are
// all broken -- most degrade gracefully, and the split below was checked by
// reading which hook each method's nil-check actually tests, not by assuming
// the names line up. DurableSleepMs looked unwired-and-broken until you notice
// its guard is on h.durableSleep, which IS wired.
//
// This test does not require the list to be empty. It requires it to be
// EXACTLY this, so that a host call added to HostCallsOptions tomorrow and not
// implemented here fails immediately rather than in someone's example a year
// later. Shrinking it is the improvement; growing it has to be deliberate.
//
// Re-derive the split with:
//
//	grep -rn "func (h \*HostCallsImpl) <Name>(" -A 5 cleat/runtime*.go |
//	  grep -oE "h\.[a-zA-Z]+ [=!]= nil"
func TestEveryHostCallIsWired(t *testing.T) {
	// Unwired AND hard-erroring: a nil hook here makes the call return the
	// "runtime was not initialized" error, so a workflow using any of these
	// cannot be tested with cleattest at all. This is the backlog.
	//
	// 7 on 2026-08-08 when this guard was added; 4 the same day once
	// ResolvePromise, RejectPromise and ContinueAsNewWithVersion were wired;
	// 1 now.
	//
	// ScheduleCron/DeleteCron/ListCrons came off this list because the reason
	// they were on it stopped being true. The note here used to say mocking
	// them would be "inventing the first specification of a workflow
	// scheduling its own cron anywhere in the codebase" and that the fix
	// belonged in the engine. It now exists there -- cleat_schedule_cron,
	// cleat_delete_cron and cleat_list_crons in engine/schedules.go -- so
	// cleattest validates against the engine's own ValidateCronExpr and
	// ValidateTimezone rather than against rules invented here.
	unimplemented := map[string]string{
		// Implementable, but needs one design decision first: WHEN do the
		// closures run. cleat/embedded/runner.go drains its deferFuncs LIFO
		// under recover() at a completion boundary; cleattest has no such
		// boundary -- tests run the workflow body and assert whenever they
		// like. Wiring it without answering that gives closures that are
		// registered and never called, which is the exact bug embedded/ had
		// for months before 2026-08-05.
		"DurableDeferFunc": "func-valued defer; drain trigger undecided (string-valued DurableDefer is wired)",
	}
	// Unwired but harmless: each has a fallback in cleat/runtime_*.go that does
	// the right thing without a hook, so wiring them would add code with no
	// behaviour change.
	fallsBack := map[string]string{
		"DurableCallTyped":            "marshals and calls DurableCall",
		"DurableCallTypedWithOptions": "falls back to the untyped path",
		"DurableCallJSONWithOptions":  "falls back to the untyped path",
		"DurableCallWithHeartbeat":    "falls back to the plain call",
		"DurableCallWithRetry":        "no nil-check on this hook",
		"DurableSleepMs":              "guards on durableSleep, which is wired",
		"ChildWorkflowWithOptions":    "falls back to ChildWorkflow",
		"WorkflowID":                  "returns an empty string",
		"RunID":                       "returns an empty string",
		"NewUUID":                     "built on Random, which is wired",
		"HandleUpdate":                "falls back to the registered handlers",
		"PluginCallStreaming":         "guarded with != nil, so nil is a no-op path",
		"AwaitSignalsWithQuorum":      "falls back to the plain await",
	}

	opts := NewTestEnv().hostCallsOptions()
	v := reflect.ValueOf(opts)
	typ := v.Type()

	var total int
	var unexpected []string
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Func {
			continue
		}
		total++
		if !v.Field(i).IsNil() {
			continue
		}
		seen[field.Name] = true
		if _, ok := unimplemented[field.Name]; ok {
			continue
		}
		if _, ok := fallsBack[field.Name]; ok {
			continue
		}
		unexpected = append(unexpected, field.Name)
	}

	if total == 0 {
		t.Fatal("found no func fields on HostCallsOptions -- this check would pass vacuously")
	}
	for _, name := range unexpected {
		t.Errorf("cleattest does not implement HostCallsOptions.%s. Wire it in "+
			"hostCallsOptions, or add it to one of the maps in this test with a "+
			"reason -- an unwired hook makes the call fail with a message about "+
			"workflow context that has nothing to do with the real cause.", name)
	}
	// The other direction: a name recorded here that is now wired, or gone from
	// the struct, means the record has rotted.
	for _, m := range []map[string]string{unimplemented, fallsBack} {
		for name := range m {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("HostCallsOptions has no field %q -- remove it from this test", name)
			} else if !seen[name] {
				t.Errorf("HostCallsOptions.%s is wired now -- remove it from this test", name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The three host calls wired on 2026-08-08. Before that, each returned
// "the HostCalls runtime was not initialized" from a fully-constructed
// TestEnv, so a workflow using any of them could not be tested at all.
// ---------------------------------------------------------------------------

func TestResolvePromise_FromInsideTheWorkflow(t *testing.T) {
	env := NewTestEnv()

	id, err := env.H().CreatePromise("p")
	if err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}

	// The workflow-side call, not the test-driver TestEnv.ResolvePromise.
	if err := env.H().ResolvePromise(id, "the-value"); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}

	result, timedOut, err := env.H().AwaitPromise(id, 0)
	if err != nil {
		t.Fatalf("AwaitPromise: %v", err)
	}
	if timedOut {
		t.Fatal("promise was resolved, but AwaitPromise timed out")
	}
	if result != "the-value" {
		t.Errorf("AwaitPromise = %q, want %q", result, "the-value")
	}
}

func TestRejectPromise_FromInsideTheWorkflow(t *testing.T) {
	env := NewTestEnv()

	id, err := env.H().CreatePromise("p")
	if err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
	if err := env.H().RejectPromise(id, "nope"); err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}

	if _, _, err := env.H().AwaitPromise(id, 0); err == nil {
		t.Fatal("expected the rejection to surface from AwaitPromise")
	}
}

func TestResolvePromise_UnknownIDIsNotAnError(t *testing.T) {
	env := NewTestEnv()
	// Matches the engine: engine/promises.go logs rather than returns a store
	// error, and the store's UPDATE matching no rows is not an error in SQL.
	// A harness that failed here would let a test assert a failure mode
	// production cannot produce.
	if err := env.H().ResolvePromise("no-such-promise", "v"); err != nil {
		t.Errorf("resolving an unknown promise returned %v, want nil", err)
	}
	if err := env.H().RejectPromise("no-such-promise", "e"); err != nil {
		t.Errorf("rejecting an unknown promise returned %v, want nil", err)
	}
}

func TestContinueAsNewWithVersion_RecordsInputAndVersion(t *testing.T) {
	env := NewTestEnv()

	if err := env.H().ContinueAsNewWithVersion(`{"n":2}`, 7); err != nil {
		t.Fatalf("ContinueAsNewWithVersion: %v", err)
	}
	env.AssertContinued(t, `{"n":2}`)
	if got := env.LastContinuedVersion(); got != 7 {
		t.Errorf("LastContinuedVersion = %d, want 7", got)
	}
}

func TestContinueAsNew_LeavesVersionUnpinned(t *testing.T) {
	env := NewTestEnv()

	// The non-versioned entry point must not invent a version. 0 is the
	// engine's own "keep the current version" sentinel.
	if err := env.H().ContinueAsNew(`{"n":1}`); err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}
	env.AssertContinued(t, `{"n":1}`)
	if got := env.LastContinuedVersion(); got != 0 {
		t.Errorf("LastContinuedVersion = %d after plain ContinueAsNew, want 0", got)
	}
}
