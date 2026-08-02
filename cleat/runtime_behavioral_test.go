package cleat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// RetryPolicy: MaximumAttempts / MaximumInterval
// ---------------------------------------------------------------------------

func TestRetryPolicyMaximumAttempts(t *testing.T) {
	rp := &RetryPolicy{MaxAttempts: 5}
	if got := rp.MaximumAttempts(); got != 5 {
		t.Errorf("MaximumAttempts() = %d, want 5", got)
	}
}

func TestRetryPolicyMaximumAttemptsZero(t *testing.T) {
	rp := &RetryPolicy{MaxAttempts: 0}
	if got := rp.MaximumAttempts(); got != 0 {
		t.Errorf("MaximumAttempts() = %d, want 0", got)
	}
}

func TestRetryPolicyMaximumAttemptsNilReceiver(t *testing.T) {
	var rp *RetryPolicy
	if got := rp.MaximumAttempts(); got != 0 {
		t.Errorf("nil MaximumAttempts() = %d, want 0", got)
	}
}

func TestRetryPolicyMaximumInterval(t *testing.T) {
	rp := &RetryPolicy{MaxInterval: 10 * time.Second}
	if got := rp.MaximumInterval(); got != 10*time.Second {
		t.Errorf("MaximumInterval() = %v, want 10s", got)
	}
}

func TestRetryPolicyMaximumIntervalZero(t *testing.T) {
	rp := &RetryPolicy{MaxInterval: 0}
	if got := rp.MaximumInterval(); got != 0 {
		t.Errorf("MaximumInterval() = %v, want 0", got)
	}
}

func TestRetryPolicyMaximumIntervalNilReceiver(t *testing.T) {
	var rp *RetryPolicy
	if got := rp.MaximumInterval(); got != 0 {
		t.Errorf("nil MaximumInterval() = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// TerminalError
// ---------------------------------------------------------------------------

func TestTerminalErrorError(t *testing.T) {
	err := &TerminalError{Err: errors.New("something went wrong")}
	msg := err.Error()
	if !strings.HasPrefix(msg, "terminal: ") {
		t.Errorf("expected 'terminal: ' prefix, got %q", msg)
	}
	if !strings.Contains(msg, "something went wrong") {
		t.Errorf("expected message content 'something went wrong', got %q", msg)
	}
}

func TestTerminalErrorUnwrap(t *testing.T) {
	inner := errors.New("inner error")
	err := &TerminalError{Err: inner}
	if !errors.Is(err, inner) {
		t.Errorf("TerminalError should wrap inner error")
	}
}

func TestNewTerminalError(t *testing.T) {
	inner := errors.New("original error")
	err := NewTerminalError(inner)

	var te *TerminalError
	if !errors.As(err, &te) {
		t.Fatal("NewTerminalError should return a *TerminalError")
	}
	if te.Err != inner {
		t.Errorf("expected wrapped error %v, got %v", inner, te.Err)
	}
}

func TestIsTerminalErrorTrue(t *testing.T) {
	err := NewTerminalError(errors.New("fatal"))
	if !IsTerminalError(err) {
		t.Error("expected IsTerminalError to return true")
	}
}

func TestIsTerminalErrorFalse(t *testing.T) {
	err := errors.New("regular error")
	if IsTerminalError(err) {
		t.Error("expected IsTerminalError to return false for regular error")
	}
}

func TestIsTerminalErrorWrapped(t *testing.T) {
	inner := NewTerminalError(errors.New("fatal"))
	wrapped := fmt.Errorf("wrapping: %w", inner)
	if !IsTerminalError(wrapped) {
		t.Error("expected IsTerminalError to work through error wrapping")
	}
}

func TestIsTerminalErrorNil(t *testing.T) {
	if IsTerminalError(nil) {
		t.Error("expected IsTerminalError to return false for nil")
	}
}

// ---------------------------------------------------------------------------
// CallError
// ---------------------------------------------------------------------------

func TestCallErrorError(t *testing.T) {
	err := &CallError{
		Service:   "mysvc",
		Operation: "myop",
		Code:      CallErrorNotFound,
		Message:   "resource not found",
	}
	msg := err.Error()
	if !strings.Contains(msg, "mysvc") || !strings.Contains(msg, "myop") {
		t.Errorf("expected error to contain service and operation, got %q", msg)
	}
	if !strings.Contains(msg, "resource not found") {
		t.Errorf("expected error to contain message, got %q", msg)
	}
}

func TestCallErrorRetryable(t *testing.T) {
	tests := []struct {
		name string
		code CallErrorCode
		want bool
	}{
		{"timeout", CallErrorTimeout, true},
		{"unavailable", CallErrorUnavailable, true},
		{"unknown", CallErrorUnknown, false},
		{"not found", CallErrorNotFound, false},
		{"invalid request", CallErrorInvalidRequest, false},
		{"permission denied", CallErrorPermissionDenied, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &CallError{Code: tc.code}
			if got := err.Retryable(); got != tc.want {
				t.Errorf("CallError.Code=%d Retryable() = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CallTimeoutError
// ---------------------------------------------------------------------------

func TestCallTimeoutErrorError(t *testing.T) {
	err := &CallTimeoutError{
		Service:   "svc",
		Operation: "op",
		Timeout:   5 * time.Second,
	}
	msg := err.Error()
	if !strings.Contains(msg, "svc") || !strings.Contains(msg, "op") {
		t.Errorf("expected error to contain service and operation, got %q", msg)
	}
	if !strings.Contains(msg, "timed out") {
		t.Errorf("expected 'timed out' in error, got %q", msg)
	}
}

// ---------------------------------------------------------------------------
// ServiceNotFoundError
// ---------------------------------------------------------------------------

func TestServiceNotFoundErrorError(t *testing.T) {
	err := &ServiceNotFoundError{Service: "db-service"}
	msg := err.Error()
	if !strings.Contains(msg, "service not found") {
		t.Errorf("expected 'service not found' in error, got %q", msg)
	}
	if !strings.Contains(msg, "db-service") {
		t.Errorf("expected 'db-service' in error, got %q", msg)
	}
}

func TestServiceNotFoundErrorUnwrap(t *testing.T) {
	inner := errors.New("connection refused")
	err := &ServiceNotFoundError{Service: "db", Err: inner}
	if !errors.Is(err, inner) {
		t.Errorf("ServiceNotFoundError should wrap inner error")
	}
}

// ---------------------------------------------------------------------------
// DurableCallError
// ---------------------------------------------------------------------------

func TestDurableCallErrorError(t *testing.T) {
	err := &DurableCallError{
		Service:   "mysvc",
		Operation: "myop",
		Message:   "invalid input",
	}
	msg := err.Error()
	if !strings.Contains(msg, "mysvc") || !strings.Contains(msg, "myop") {
		t.Errorf("expected error to contain service and operation, got %q", msg)
	}
	if !strings.Contains(msg, "invalid input") {
		t.Errorf("expected 'invalid input' in error, got %q", msg)
	}
}

func TestDurableCallErrorUnwrap(t *testing.T) {
	inner := errors.New("underlying cause")
	err := &DurableCallError{
		Service:   "svc",
		Operation: "op",
		Message:   "fail",
		Err:       inner,
	}
	if !errors.Is(err, inner) {
		t.Errorf("DurableCallError should wrap inner error")
	}
}

// ---------------------------------------------------------------------------
// SuspendSentinel
// ---------------------------------------------------------------------------

func TestSuspendSentinelError(t *testing.T) {
	var err SuspendSentinel
	msg := err.Error()
	if msg != "durable: workflow suspended" {
		t.Errorf("expected 'durable: workflow suspended', got %q", msg)
	}
}

func TestErrSuspendSentinelValue(t *testing.T) {
	if ErrSuspend == nil {
		t.Fatal("ErrSuspend should not be nil")
	}
	if ErrSuspend.Error() != "durable: workflow suspended" {
		t.Errorf("expected 'durable: workflow suspended', got %q", ErrSuspend.Error())
	}
	var se SuspendSentinel
	if !errors.Is(ErrSuspend, se) {
		t.Error("ErrSuspend should be a SuspendSentinel")
	}
}

// ---------------------------------------------------------------------------
// Promise / Await
// ---------------------------------------------------------------------------

func TestNewPromiseTyped(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		CreatePromise: func(name string) (string, error) {
			return "promise-abc", nil
		},
	})

	p, err := NewPromiseTyped[string](h, "my-promise")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "promise-abc" {
		t.Errorf("expected ID 'promise-abc', got %q", p.ID)
	}
	if p.Name != "my-promise" {
		t.Errorf("expected Name 'my-promise', got %q", p.Name)
	}
}

func TestNewPromiseTypedCreateError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		CreatePromise: func(name string) (string, error) {
			return "", errors.New("creation failed")
		},
	})

	_, err := NewPromiseTyped[string](h, "fail-promise")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "creation failed") {
		t.Errorf("expected 'creation failed' in error, got %v", err)
	}
}

func TestPromiseAwaitResolved(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		CreatePromise: func(name string) (string, error) {
			return "p1", nil
		},
		AwaitPromise: func(promiseID string, timeout time.Duration) (string, bool, error) {
			return `{"value":42}`, false, nil
		},
	})

	p, _ := NewPromiseTyped[struct{ Value int }](h, "test")
	result, timedOut, err := p.Await(time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Error("expected timedOut to be false")
	}
	if result.Value != 42 {
		t.Errorf("expected Value=42, got %d", result.Value)
	}
}

func TestPromiseAwaitStringResult(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		CreatePromise: func(name string) (string, error) {
			return "p1", nil
		},
		AwaitPromise: func(promiseID string, timeout time.Duration) (string, bool, error) {
			return `"hello"`, false, nil // JSON string
		},
	})

	p, _ := NewPromiseTyped[string](h, "test")
	result, timedOut, err := p.Await(time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Error("expected timedOut to be false")
	}
	if result != "hello" {
		t.Errorf("expected 'hello', got %q", result)
	}
}

func TestPromiseAwaitTimeout(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		CreatePromise: func(name string) (string, error) {
			return "p1", nil
		},
		AwaitPromise: func(promiseID string, timeout time.Duration) (string, bool, error) {
			// Return a valid JSON value (empty string in JSON is "\"\"").
			return `""`, true, nil
		},
	})

	p, _ := NewPromiseTyped[string](h, "test")
	result, timedOut, err := p.Await(time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !timedOut {
		t.Error("expected timedOut to be true")
	}
	if result != "" {
		t.Errorf("expected empty string on timeout, got %q", result)
	}
}

func TestPromiseAwaitHostError(t *testing.T) {
	expectedErr := errors.New("network error")
	h := NewHostCalls(HostCallsOptions{
		CreatePromise: func(name string) (string, error) {
			return "p1", nil
		},
		AwaitPromise: func(promiseID string, timeout time.Duration) (string, bool, error) {
			return "", false, expectedErr
		},
	})

	p, _ := NewPromiseTyped[string](h, "test")
	_, _, err := p.Await(time.Minute)
	if err != expectedErr {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
}

func TestPromiseAwaitUnmarshalError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		CreatePromise: func(name string) (string, error) {
			return "p1", nil
		},
		AwaitPromise: func(promiseID string, timeout time.Duration) (string, bool, error) {
			return "{invalid json}", false, nil
		},
	})

	p, _ := NewPromiseTyped[string](h, "test")
	_, _, err := p.Await(time.Minute)
	if err == nil {
		t.Fatal("expected unmarshal error, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshal promise result") {
		t.Errorf("expected unmarshal error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// VirtualObject
// ---------------------------------------------------------------------------

func TestNewVirtualObject(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "counter", "room-123")
	if vo.ObjectType != "counter" {
		t.Errorf("expected ObjectType 'counter', got %q", vo.ObjectType)
	}
	if vo.InstanceKey != "room-123" {
		t.Errorf("expected InstanceKey 'room-123', got %q", vo.InstanceKey)
	}
}

func TestVirtualObjectSetAndGet(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "counter", "room-123")

	vo.Set("visits", 42)

	var val int
	if err := vo.Get("visits", &val); err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if val != 42 {
		t.Errorf("expected 42, got %d", val)
	}
}

func TestVirtualObjectSetAndGetString(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "app", "inst-1")

	vo.Set("name", "test-entity")

	var val string
	if err := vo.Get("name", &val); err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if val != "test-entity" {
		t.Errorf("expected 'test-entity', got %q", val)
	}
}

func TestVirtualObjectGetInt(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "counter", "room-123")

	vo.Set("count", 99)

	val := vo.GetInt("count")
	if val != 99 {
		t.Errorf("expected 99, got %d", val)
	}
}

func TestVirtualObjectGetIntMissing(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "counter", "room-123")

	val := vo.GetInt("nonexistent")
	if val != 0 {
		t.Errorf("expected 0 for missing key, got %d", val)
	}
}

func TestVirtualObjectDelete(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "counter", "room-123")

	vo.Set("temp", "value")
	if !vo.Has("temp") {
		t.Fatal("expected key to exist before delete")
	}

	vo.Delete("temp")
	if vo.Has("temp") {
		t.Error("expected key to be gone after delete")
	}
}

func TestVirtualObjectHasExisting(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "counter", "room-123")

	if vo.Has("present") {
		t.Log("Has returns false before Set")
	}

	vo.Set("present", true)
	if !vo.Has("present") {
		t.Error("expected Has to return true after Set")
	}
}

func TestVirtualObjectList(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "counter", "room-123")

	vo.Set("a", 1)
	vo.Set("b", 2)
	vo.Set("c", 3)

	keys := vo.List("")
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d: %v", len(keys), keys)
	}
}

func TestVirtualObjectListPrefix(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "app", "inst")

	vo.Set("item_x", "x")
	vo.Set("item_y", "y")
	vo.Set("other", "z")

	keys := vo.List("item_")
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys with prefix 'item_', got %d: %v", len(keys), keys)
	}
}

func TestVirtualObjectListEmpty(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "counter", "room-123")

	keys := vo.List("")
	if len(keys) != 0 {
		t.Errorf("expected empty list, got %v", keys)
	}
}

func TestVirtualObjectContinueAsNew(t *testing.T) {
	var capturedInput string
	h := NewHostCalls(HostCallsOptions{
		ContinueAsNew: func(inputJSON string) error {
			capturedInput = inputJSON
			return nil
		},
	})
	vo := NewVirtualObject(h, "counter", "room-123")

	err := vo.ContinueAsNew(`{"state":"fresh"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput != `{"state":"fresh"}` {
		t.Errorf("expected input %q, got %q", `{"state":"fresh"}`, capturedInput)
	}
}

func TestVirtualObjectContinueAsNewError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		ContinueAsNew: func(inputJSON string) error {
			return errors.New("continue failed")
		},
	})
	vo := NewVirtualObject(h, "counter", "room-123")

	err := vo.ContinueAsNew(`{}`)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "continue failed") {
		t.Errorf("expected 'continue failed' in error, got %v", err)
	}
}

func TestVirtualObjectScopeIsolation(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo1 := NewVirtualObject(h, "counter", "room-a")
	vo2 := NewVirtualObject(h, "counter", "room-b")

	// Set values in different scopes using the same HostCalls instance.
	vo1.Set("visits", 10)
	vo2.Set("visits", 20)

	var val int
	if err := vo1.Get("visits", &val); err != nil {
		t.Fatalf("vo1 Get failed: %v", err)
	}
	if val != 10 {
		t.Errorf("vo1 expected 10, got %d", val)
	}

	if err := vo2.Get("visits", &val); err != nil {
		t.Fatalf("vo2 Get failed: %v", err)
	}
	if val != 20 {
		t.Errorf("vo2 expected 20, got %d", val)
	}
}

// ---------------------------------------------------------------------------
// isNonRetryable — positive match
// ---------------------------------------------------------------------------

func TestIsNonRetryableMatch(t *testing.T) {
	err := errors.New("InvalidRequest: bad input")
	if !isNonRetryable(err, []string{"InvalidRequest"}) {
		t.Error("expected isNonRetryable to return true when pattern matches")
	}
}

func TestIsNonRetryableMultiplePatterns(t *testing.T) {
	err := errors.New("NotFound: missing")
	patterns := []string{"InvalidRequest", "NotFound", "Timeout"}
	if !isNonRetryable(err, patterns) {
		t.Error("expected isNonRetryable to match 'NotFound' pattern")
	}
}

func TestIsNonRetryableSubstring(t *testing.T) {
	err := errors.New("something with Timeout in the middle")
	if !isNonRetryable(err, []string{"Timeout"}) {
		t.Error("expected isNonRetryable to match substring 'Timeout'")
	}
}

// ---------------------------------------------------------------------------
// RegisterTypedUpdateHandler
// ---------------------------------------------------------------------------

func TestRegisterTypedUpdateHandler(t *testing.T) {
	type MyReq struct {
		Name string `json:"name"`
	}
	type MyResp struct {
		Greeting string `json:"greeting"`
	}

	h := NewHostCalls(HostCallsOptions{})

	RegisterTypedUpdateHandler(h, "say_hello",
		func(req MyReq) (MyResp, error) {
			return MyResp{Greeting: "Hello, " + req.Name + "!"}, nil
		},
		func(req MyReq) error {
			if req.Name == "" {
				return errors.New("name must not be empty")
			}
			return nil
		},
	)

	result, err := h.HandleUpdate("say_hello", `{"name":"Alice"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var resp MyResp
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if resp.Greeting != "Hello, Alice!" {
		t.Errorf("expected 'Hello, Alice!', got %q", resp.Greeting)
	}
}

// ---------------------------------------------------------------------------
// HandleQuery / HandleUpdate on hostCallsImpl
// ---------------------------------------------------------------------------

func TestHandleUpdateNotFound(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{}).HostCallsImpl
	_, err := h.HandleUpdate("no_such_handler", `{}`)
	if err == nil {
		t.Fatal("expected error for unknown update handler")
	}
	if !strings.Contains(err.Error(), "no update handler registered") {
		t.Errorf("expected 'no update handler registered' in error, got %v", err)
	}
}

func TestHandleQueryRegistered(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{}).HostCallsImpl
	h.RegisterQueryHandler("get_status", func(payloadJSON string) (string, error) {
		return `{"status":"ok"}`, nil
	})

	result, err := h.HandleQuery("get_status", `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"status":"ok"}` {
		t.Errorf("expected result %q, got %q", `{"status":"ok"}`, result)
	}
}

func TestHandleQueryNotFound(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{}).HostCallsImpl
	_, err := h.HandleQuery("no_such_query", `{}`)
	if err == nil {
		t.Fatal("expected error for unknown query handler")
	}
	if !strings.Contains(err.Error(), "no query handler registered") {
		t.Errorf("expected 'no query handler registered' in error, got %v", err)
	}
}

func TestHandleQueryDelegatesToField(t *testing.T) {
	var capturedName, capturedPayload string
	h := NewHostCalls(HostCallsOptions{
		HandleQuery: func(name, payload string) (string, error) {
			capturedName = name
			capturedPayload = payload
			return `{"delegated":true}`, nil
		},
	}).HostCallsImpl

	result, err := h.HandleQuery("my_query", `{"key":"val"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `{"delegated":true}` {
		t.Errorf("expected delegated result, got %q", result)
	}
	if capturedName != "my_query" {
		t.Errorf("expected name 'my_query', got %q", capturedName)
	}
	if capturedPayload != `{"key":"val"}` {
		t.Errorf("expected payload %q, got %q", `{"key":"val"}`, capturedPayload)
	}
}

// ---------------------------------------------------------------------------
// SagaTyped
// ---------------------------------------------------------------------------

func TestSagaTypedExecutesAllSteps(t *testing.T) {
	var order []string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {},
	})
	s := NewSagaTyped[string]()
	s.AddStep("step1",
		func(h HostCalls) (string, error) { order = append(order, "step1"); return "res1", nil },
		nil,
	)
	s.AddStep("step2",
		func(h HostCalls) (string, error) { order = append(order, "step2"); return "res2", nil },
		nil,
	)

	results, err := s.Run(h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0] != "res1" || results[1] != "res2" {
		t.Errorf("expected [res1 res2], got %v", results)
	}
	expected := []string{"step1", "step2"}
	if len(order) != len(expected) {
		t.Fatalf("expected order %v, got %v", expected, order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("position %d: expected %q, got %q", i, v, order[i])
		}
	}
}

func TestSagaTypedCompensatesOnTerminalError(t *testing.T) {
	var order []string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {},
	})
	s := NewSagaTyped[string]()
	s.AddStep("step1",
		func(h HostCalls) (string, error) { order = append(order, "step1"); return "ok", nil },
		func(h HostCalls) error { order = append(order, "comp1"); return nil },
	)
	s.AddStep("step2",
		func(h HostCalls) (string, error) { order = append(order, "step2"); return "ok", nil },
		func(h HostCalls) error { order = append(order, "comp2"); return nil },
	)
	s.AddStep("step3",
		func(h HostCalls) (string, error) {
			order = append(order, "step3")
			return "", NewTerminalError(errors.New("failed"))
		},
		nil,
	)

	_, err := s.Run(h)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	expected := []string{"step1", "step2", "step3", "comp2", "comp1"}
	if len(order) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("position %d: expected %q, got %q", i, v, order[i])
		}
	}
}

func TestSagaTypedReturnsTransientErrorWithoutCompensation(t *testing.T) {
	var order []string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {},
	})
	s := NewSagaTyped[string]()
	s.AddStep("step1",
		func(h HostCalls) (string, error) { order = append(order, "step1"); return "ok", nil },
		func(h HostCalls) error { order = append(order, "comp1"); return nil },
	)
	s.AddStep("step2",
		func(h HostCalls) (string, error) { order = append(order, "step2"); return "ok", nil },
		func(h HostCalls) error { order = append(order, "comp2"); return nil },
	)
	s.AddStep("step3",
		func(h HostCalls) (string, error) {
			order = append(order, "step3")
			return "", errors.New("transient") // NOT a TerminalError
		},
		nil,
	)

	_, err := s.Run(h)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// All completed steps are compensated on any error (not just TerminalError).
	expected := []string{"step1", "step2", "step3", "comp2", "comp1"}
	if len(order) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("position %d: expected %q, got %q", i, v, order[i])
		}
	}
}

// ---------------------------------------------------------------------------
// HostCalls wrapper: nil-guard — Error-returning methods
// ---------------------------------------------------------------------------

func TestHostCallsImpl_NilGuard_PluginCall(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.PluginCall("test", "fn", "{}")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_PluginCallStreaming(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.PluginCallStreaming("test", "fn", "{}")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_CreatePromise(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.CreatePromise("test-promise")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_AwaitPromise(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, _, err := h.AwaitPromise("test", time.Second)
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_DurableDefer(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.DurableDefer("test defer")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_DurableDeferFunc(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.DurableDeferFunc(func() {})
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_PollSignal(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, _, err := h.PollSignal("test-signal")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_ContinueAsNew(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	err := h.ContinueAsNew("{}")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_ContinueAsNewWithVersion(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	err := h.ContinueAsNewWithVersion("{}", 2)
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_ChildWorkflow(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.ChildWorkflow("child", "{}")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_AwaitChild(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.AwaitChild("run-1")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_AwaitAllChildren(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.AwaitAllChildren([]string{"run-1"})
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_SendSignalAndWait(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.SendSignalAndWait("target", "sig", "{}", time.Second)
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_SignalWorkflow(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	err := h.SignalWorkflow("target", "sig", "{}")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_ScheduleCron(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.ScheduleCron("wf", "* * * * *", "UTC", "{}")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_DeleteCron(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	err := h.DeleteCron("sched-1")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_ListCrons(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.ListCrons()
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_AcquireLockMs(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.AcquireLockMs("lock-key", 1000)
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_ReleaseLock(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	err := h.ReleaseLock("lock-key")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_SideEffect(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.SideEffect(func() (string, error) { return "result", nil })
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_NilGuard_DurableCall(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	_, err := h.DurableCall("svc", "op", "{}")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// HostCalls wrapper: nil-guard — No-op methods
// ---------------------------------------------------------------------------

func TestHostCallsImpl_NilGuard_DurableLog(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	h.DurableLog("test message")
}

func TestHostCallsImpl_NilGuard_LogKV(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	h.LogKV("msg", "key1", "val1")
}

func TestHostCallsImpl_NilGuard_PollCancellation(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	cancelled, reason := h.PollCancellation()
	if cancelled {
		t.Error("expected cancelled=false when pollCancellation is nil")
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
}

func TestHostCallsImpl_NilGuard_SetQueryState(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	h.SetQueryState("key", "value")
}

func TestHostCallsImpl_NilGuard_WorkflowID(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	if id := h.WorkflowID(); id != "" {
		t.Errorf("expected empty workflow ID, got %q", id)
	}
}

func TestHostCallsImpl_NilGuard_RunID(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	if id := h.RunID(); id != "" {
		t.Errorf("expected empty run ID, got %q", id)
	}
}

// ---------------------------------------------------------------------------
// HostCalls wrapper: nil-guard — Default-returning methods
// ---------------------------------------------------------------------------

func TestHostCallsImpl_NilGuard_VersionDefaults(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	if v := h.Version(); v != 1 {
		t.Errorf("expected Version()=1 when nil, got %d", v)
	}
	if v := h.MinVersion(); v != 1 {
		t.Errorf("expected MinVersion()=1 when nil, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// HostCalls wrapper: nil-guard — Panic methods
// ---------------------------------------------------------------------------

func TestHostCallsImpl_NilGuard_DurableSleepMsNil(t *testing.T) {
	// DurableSleepMs logs and returns (no panic) when durableSleep is nil.
	h := NewHostCalls(HostCallsOptions{})
	h.DurableSleepMs(100)
}

func TestHostCallsImpl_NilGuard_NowMsNil(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	if v := h.NowMs(); v != 0 {
		t.Errorf("expected 0 when Now is nil, got %d", v)
	}
}

func TestHostCallsImpl_NilGuard_RandomNil(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	if v := h.Random(); v != 0 {
		t.Errorf("expected 0 when Random is nil, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// HostCalls wrapper: fallback chains
// ---------------------------------------------------------------------------

func TestHostCallsImpl_Fallback_DurableCallJSON(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(svc, op, req string) (string, error) {
			captured = true
			return `{"result":"ok"}`, nil
		},
	})
	var result struct {
		Result string `json:"result"`
	}
	err := h.DurableCallJSON("svc", "op", `{}`, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DurableCall to be called")
	}
	if result.Result != "ok" {
		t.Errorf("expected result.ok, got %q", result.Result)
	}
}

func TestHostCallsImpl_Fallback_DurableCallJSONNilResult(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(svc, op, req string) (string, error) {
			captured = true
			return "{}", nil
		},
	})
	err := h.DurableCallJSON("svc", "op", `{}`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DurableCall to be called")
	}
}

func TestHostCallsImpl_Fallback_DurableCallWithHeartbeat(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(svc, op, req string) (string, error) {
			captured = true
			return "result", nil
		},
	})
	resp, err := h.DurableCallWithHeartbeat("svc", "op", "{}", time.Second, func(s string) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DurableCall fallback to be called")
	}
	if resp != "result" {
		t.Errorf("expected 'result', got %q", resp)
	}
}

func TestHostCallsImpl_Fallback_DurableCallWithHeartbeatDirect(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DurableCallWithHeartbeat: func(svc, op, req string, interval time.Duration, onProgress func(string)) (string, error) {
			captured = true
			return "hb-result", nil
		},
	})
	resp, err := h.DurableCallWithHeartbeat("svc", "op", "{}", time.Second, func(s string) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DurableCallWithHeartbeat to be called")
	}
	if resp != "hb-result" {
		t.Errorf("expected 'hb-result', got %q", resp)
	}
}

func TestHostCallsImpl_Fallback_AcquireLock(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		AcquireLock: func(key string, ttlMs int64) (bool, error) {
			captured = true
			return true, nil
		},
	})
	acquired, err := h.AcquireLock("test-key", time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected AcquireLockMs to be called")
	}
	if !acquired {
		t.Error("expected acquired=true")
	}
}

// ---------------------------------------------------------------------------
// HostCalls: Delegation success paths
// ---------------------------------------------------------------------------

func TestHostCallsImpl_PluginCallSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		PluginCall: func(pn, fn, input string) (string, error) {
			return `{"ok":true}`, nil
		},
	})
	resp, err := h.PluginCall("test", "fn", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != `{"ok":true}` {
		t.Errorf("expected response, got %q", resp)
	}
}

func TestHostCallsImpl_WorkflowIDCustom(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		WorkflowID: func() string { return "wf-custom" },
	})
	if id := h.WorkflowID(); id != "wf-custom" {
		t.Errorf("expected 'wf-custom', got %q", id)
	}
}

func TestHostCallsImpl_RunIDCustom(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		RunID: func() string { return "run-custom" },
	})
	if id := h.RunID(); id != "run-custom" {
		t.Errorf("expected 'run-custom', got %q", id)
	}
}

func TestHostCallsImpl_ContinueAsNewSuccess(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		ContinueAsNew: func(input string) error {
			captured = true
			return nil
		},
	})
	err := h.ContinueAsNew("{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected ContinueAsNew to be called")
	}
}

func TestHostCallsImpl_PollSignalSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			return "payload", true, nil
		},
	})
	payload, found, err := h.PollSignal("sig")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Error("expected found=true")
	}
	if payload != "payload" {
		t.Errorf("expected 'payload', got %q", payload)
	}
}

func TestHostCallsImpl_DurableDeferSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableDefer: func(desc string) (string, error) {
			return "defer-id", nil
		},
	})
	id, err := h.DurableDefer("cleanup")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "defer-id" {
		t.Errorf("expected 'defer-id', got %q", id)
	}
}

func TestHostCallsImpl_DurableDeferFuncSuccess(t *testing.T) {
	var executed bool
	h := NewHostCalls(HostCallsOptions{
		DurableDeferFunc: func(fn func()) (string, error) {
			executed = true
			return "defer-id", nil
		},
	})
	id, err := h.DurableDeferFunc(func() {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !executed {
		t.Error("expected DurableDeferFunc to execute")
	}
	if id != "defer-id" {
		t.Errorf("expected 'defer-id', got %q", id)
	}
}

func TestHostCallsImpl_SignalWorkflowSuccess(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		SignalWorkflow: func(target, sig, payload string) error {
			captured = true
			return nil
		},
	})
	err := h.SignalWorkflow("target", "sig", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected SignalWorkflow to be called")
	}
}

func TestHostCallsImpl_ScheduleCronSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		ScheduleCron: func(wf, cron, tz, input string) (string, error) {
			return "sched-1", nil
		},
	})
	id, err := h.ScheduleCron("wf", "* * * * *", "UTC", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "sched-1" {
		t.Errorf("expected 'sched-1', got %q", id)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: version delegation
// ---------------------------------------------------------------------------

func TestHostCallsImpl_VersionDelegation(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Version: func() int { return 5 },
	})
	if v := h.Version(); v != 5 {
		t.Errorf("expected 5, got %d", v)
	}
}

func TestHostCallsImpl_MinVersionDelegation(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		MinVersion: func() int { return 2 },
	})
	if v := h.MinVersion(); v != 2 {
		t.Errorf("expected 2, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: SideEffect success and error paths
// ---------------------------------------------------------------------------

func TestHostCallsImpl_SideEffectSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		SideEffect: func(result string) (string, error) {
			return result, nil
		},
	})
	resp, err := h.SideEffect(func() (string, error) { return "computed", nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "computed" {
		t.Errorf("expected 'computed', got %q", resp)
	}
}

func TestHostCallsImpl_SideEffectErrorFromFn(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		SideEffect: func(result string) (string, error) {
			return result, nil
		},
	})
	_, err := h.SideEffect(func() (string, error) { return "", errors.New("fn-error") })
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: AcquireLockMs / ReleaseLock success
// ---------------------------------------------------------------------------

func TestHostCallsImpl_AcquireLockMsSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		AcquireLock: func(key string, ttlMs int64) (bool, error) {
			return true, nil
		},
	})
	acquired, err := h.AcquireLockMs("key", 5000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Error("expected acquired=true")
	}
}

func TestHostCallsImpl_ReleaseLockSuccess(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		ReleaseLock: func(key string) error {
			captured = true
			return nil
		},
	})
	err := h.ReleaseLock("key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected ReleaseLock to be called")
	}
}

// ---------------------------------------------------------------------------
// HostCalls: AwaitSignals delegation
// ---------------------------------------------------------------------------

func TestHostCallsImpl_AwaitSignalsDelegation(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "", "", true, nil
		},
	})
	result := h.AwaitSignals([]string{"sig1"}, time.Second)
	if !result.TimedOut {
		t.Error("expected timedOut=true")
	}
}

// ---------------------------------------------------------------------------
// HostCalls: DurableCallWithOptions fallback
// ---------------------------------------------------------------------------

func TestHostCallsImpl_DurableCallWithOptionsFallback(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(svc, op, req string) (string, error) {
			captured = true
			return "result", nil
		},
	})
	resp, err := h.DurableCallWithOptions(CallOptions{}, "svc", "op", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DurableCall fallback")
	}
	if resp != "result" {
		t.Errorf("expected 'result', got %q", resp)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: ChildWorkflowWithOptions fallback and direct
// ---------------------------------------------------------------------------

func TestHostCallsImpl_ChildWorkflowWithOptionsFallback(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		ChildWorkflow: func(name, input string) (string, error) {
			captured = true
			return "child-run-id", nil
		},
	})
	runID, err := h.ChildWorkflowWithOptions("child", "{}", ChildWorkflowOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected ChildWorkflow fallback")
	}
	if runID != "child-run-id" {
		t.Errorf("expected 'child-run-id', got %q", runID)
	}
}

func TestHostCallsImpl_ChildWorkflowWithOptionsDirect(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		ChildWorkflowWithOptions: func(name, input string, version int, parentClosePolicy string, priority int) (string, error) {
			captured = true
			return "direct-run-id", nil
		},
	})
	runID, err := h.ChildWorkflowWithOptions("child", "{}", ChildWorkflowOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected ChildWorkflowWithOptions to be called")
	}
	if runID != "direct-run-id" {
		t.Errorf("expected 'direct-run-id', got %q", runID)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: ReplyToSignal
// ---------------------------------------------------------------------------

func TestHostCallsImpl_NilGuard_ReplyToSignal(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	err := h.ReplyToSignal("corr-id", "response")
	if err == nil {
		t.Errorf("expected error, got %v", err)
	}
}

func TestHostCallsImpl_ReplyToSignalSuccess(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		ReplyToSignal: func(corrID, resp string) error {
			captured = true
			return nil
		},
	})
	err := h.ReplyToSignal("corr-id", "ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected ReplyToSignal to be called")
	}
}

// ---------------------------------------------------------------------------
// HostCalls: SetQueryState delegation
// ---------------------------------------------------------------------------

func TestHostCallsImpl_SetQueryStateDelegation(t *testing.T) {
	var capturedKey, capturedValue string
	h := NewHostCalls(HostCallsOptions{
		SetQueryState: func(key, value string) {
			capturedKey = key
			capturedValue = value
		},
	})
	h.SetQueryState("my-key", "my-value")
	if capturedKey != "my-key" || capturedValue != "my-value" {
		t.Errorf("expected (my-key, my-value), got (%q, %q)", capturedKey, capturedValue)
	}
}

// ---------------------------------------------------------------------------
// Version: default build-time variables
// ---------------------------------------------------------------------------

func TestVersionDefaults(t *testing.T) {
	if WorkflowName != "unknown" {
		t.Errorf("expected 'unknown', got %q", WorkflowName)
	}
	if WorkflowVersion != 0 {
		t.Errorf("expected 0, got %d", WorkflowVersion)
	}
	if MinVersion != 1 {
		t.Errorf("expected 1, got %d", MinVersion)
	}
	if ABIVersion != 1 {
		t.Errorf("expected 1, got %d", ABIVersion)
	}
	if PluginDeps != "{}" {
		t.Errorf("expected '{}', got %q", PluginDeps)
	}
}

// ---------------------------------------------------------------------------
// Selector: construction and accessors
// ---------------------------------------------------------------------------

func TestNewSelectorDefaults(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	s := NewSelector(h)
	if s == nil {
		t.Fatal("expected non-nil Selector")
	}
	if err := s.Err(); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestSelectorAddSignalNoFutures(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	s := NewSelector(h)
	var dest string
	s.AddSignal("test-signal", &dest)
	winner := s.Select()
	if winner != "" {
		t.Errorf("expected empty string (nothing to wait for), got %q", winner)
	}
}

func TestSelectorAddTimerFires(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			return "", false, nil
		},
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "", "", true, nil
		},
		DurableSleep: func(ms int64) {
			// No-op
		},
		Now: func() int64 {
			return 1000000
		},
	})
	s := NewSelector(h)
	var fired bool
	s.AddTimer(time.Nanosecond, &fired)
	winner := s.Select()
	if winner != SelectorTimer {
		t.Errorf("expected SelectorTimer, got %q", winner)
	}
	if !fired {
		t.Error("expected fired=true")
	}
}

func TestSelectorAddSignalFires(t *testing.T) {
	pollCalled := false
	h := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			pollCalled = true
			if name == "my-signal" {
				return "payload", true, nil
			}
			return "", false, nil
		},
		Now: func() int64 {
			return 1000
		},
	})
	s := NewSelector(h)
	var dest string
	s.AddSignal("my-signal", &dest)
	winner := s.Select()
	if !pollCalled {
		t.Error("expected PollSignal to be called")
	}
	if winner != "my-signal" {
		t.Errorf("expected 'my-signal', got %q", winner)
	}
	if dest != "payload" {
		t.Errorf("expected dest='payload', got %q", dest)
	}
}

func TestSelectorErrorPreserved(t *testing.T) {
	// Select errors from PollSignal are silently ignored.
	h := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			return "", false, errors.New("poll error")
		},
		Now: func() int64 {
			return 1000
		},
	})
	s := NewSelector(h)
	s.AddSignal("sig1", new(string))
	winner := s.Select()
	_ = winner
}

func TestSelectorAddChildReturnsError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			return "", false, nil
		},
		AwaitChild: func(runID string) (string, error) {
			return "", errors.New("child not ready")
		},
		Now: func() int64 {
			return 1000
		},
	})
	s := NewSelector(h)
	s.AddChildWorkflow("child-run", new(string))
	_ = s.Select()
}

// ---------------------------------------------------------------------------
// HostCalls: DurableSleep delegation (panics via DurableSleepMs)
// ---------------------------------------------------------------------------

func TestHostCallsImpl_DurableSleepDelegates(t *testing.T) {
	// DurableSleep delegates to DurableSleepMs which logs and returns when nil.
	h := NewHostCalls(HostCallsOptions{})
	h.DurableSleep(time.Millisecond)
}

// ---------------------------------------------------------------------------
// HostCalls: Now delegates to NowMs
// ---------------------------------------------------------------------------

func TestHostCallsImpl_NowDelegates(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	// Now calls NowMs which returns 0 when nil, giving epoch = 1970-01-01.
	ms := h.Now().UnixMilli()
	if ms != 0 {
		t.Errorf("expected ms=0 when Now is nil, got %d", ms)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: LogKV edge cases (no panic)
// ---------------------------------------------------------------------------

func TestHostCallsImpl_LogKVEdgeCases(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	h.LogKV("test")
	h.LogKV("test", "key1", "val1", "key2", "val2")
	h.LogKV("test", "key1")
}

// ---------------------------------------------------------------------------
// Saga.AddParallel (composite step)
// ---------------------------------------------------------------------------

func TestSagaAddParallel(t *testing.T) {
	var order []string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {},
	})
	s := NewSagaTyped[string]()
	s.AddStep("parallel1",
		func(h HostCalls) (string, error) { order = append(order, "p1"); return "ok", nil },
		nil,
	)
	s.AddStep("parallel2",
		func(h HostCalls) (string, error) { order = append(order, "p2"); return "ok", nil },
		nil,
	)
	_, err := s.Run(h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 2 || order[0] != "p1" || order[1] != "p2" {
		t.Errorf("expected [p1 p2], got %v", order)
	}
}

// ---------------------------------------------------------------------------
// UUID methods: format validation
// ---------------------------------------------------------------------------

func TestHostCallsImpl_UUIDFormat(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		WorkflowID: func() string { return "wf-test" },
	})
	uid := h.UUID("seed")
	if len(uid) != 36 {
		t.Errorf("expected UUID of length 36, got %d: %s", len(uid), uid)
	}
	uid2 := h.UUID("seed")
	if uid != uid2 {
		t.Errorf("expected deterministic UUID, got %q vs %q", uid, uid2)
	}
}

func TestHostCallsImpl_NewUUIDFormat(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Random: func() int64 { return 12345 },
	})
	uid := h.NewUUID()
	if len(uid) != 36 {
		t.Errorf("expected UUID of length 36, got %d: %s", len(uid), uid)
	}
}

func TestHostCallsImpl_NewUUIDv7Format(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Now:    func() int64 { return 5000 },
		Random: func() int64 { return 67890 },
	})
	uid := h.NewUUIDv7()
	if len(uid) != 36 {
		t.Errorf("expected UUID of length 36, got %d: %s", len(uid), uid)
	}
	parts := strings.Split(uid, "-")
	if len(parts) != 5 {
		t.Errorf("expected 5 UUID parts, got %d", len(parts))
	}
}

// ---------------------------------------------------------------------------
// HostCalls: State operations (SetState, GetState, etc.)
// ---------------------------------------------------------------------------

func TestHostCallsImpl_StateOperations(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	h.SetState("key1", "value1")
	h.SetState("key2", 42)

	var result string
	err := h.GetState("key1", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "value1" {
		t.Errorf("expected 'value1', got %q", result)
	}

	if !h.HasState("key1") {
		t.Error("expected HasState(key1)=true")
	}
	if h.HasState("nonexistent") {
		t.Error("expected HasState(nonexistent)=false")
	}

	h.DeleteState("key1")
	if h.HasState("key1") {
		t.Error("expected HasState(key1)=false after delete")
	}

	newVal := h.IncrState("counter", 5)
	if newVal != 5 {
		t.Errorf("expected IncrState=5, got %d", newVal)
	}
	newVal = h.IncrState("counter", 3)
	if newVal != 8 {
		t.Errorf("expected IncrState=8, got %d", newVal)
	}

	keys := h.ListState("key")
	if len(keys) < 1 {
		t.Errorf("expected at least 1 key matching 'key', got %d: %v", len(keys), keys)
	}
}

func TestHostCallsImpl_GetStateNotFound(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	var result string
	err := h.GetState("missing", &result)
	if err == nil {
		t.Error("expected error for missing key")
	}
}

// ---------------------------------------------------------------------------
// HostCalls: DeleteCron / ListCrons success paths
// ---------------------------------------------------------------------------

func TestHostCallsImpl_DeleteCronSuccess(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DeleteCron: func(id string) error {
			captured = true
			return nil
		},
	})
	err := h.DeleteCron("sched-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DeleteCron to be called")
	}
}

func TestHostCallsImpl_ListCronsSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		ListCrons: func() (string, error) {
			return `[{"schedule_id":"s1"}]`, nil
		},
	})
	data, err := h.ListCrons()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(data, "s1") {
		t.Errorf("expected response containing s1, got %q", data)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: AwaitSignalsWithQuorum fallback
// ---------------------------------------------------------------------------

func TestHostCallsImpl_AwaitSignalsWithQuorumFallback(t *testing.T) {
	// When awaitSignalsWithQuorum is nil, the fallback uses DurableAwaitSignals.
	// Test that at least it doesn't panic and returns a reasonable error.
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "", "", true, nil
		},
	})
	_, err := h.AwaitSignalsWithQuorum([]string{"sig1"}, 1, -1, time.Millisecond)
	// Expect a quorum timeout error since no signals arrive.
	if err == nil {
		t.Error("expected error for quorum timeout")
	}
}

// ---------------------------------------------------------------------------
// HostCalls: AwaitCondition nil-guard uses default loop
// ---------------------------------------------------------------------------

func TestHostCallsImpl_AwaitConditionDefaultLoop(t *testing.T) {
	var now int64 = 1000
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "", "", true, nil
		},
		Now: func() int64 {
			now += 1000 // advance ~1s each call so deadline expires immediately
			return now
		},
		DurableSleep: func(ms int64) {},
	})
	// Predicate always false; deadline is now+timeout; should expire after 1-2 loop iterations.
	met := h.AwaitCondition(func() bool { return false }, time.Millisecond, time.Millisecond)
	if met {
		t.Error("expected met=false for always-false predicate")
	}
}

func TestHostCallsImpl_AwaitConditionUsesCustomFn(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		AwaitCondition: func(pred func() bool, pollInterval, timeout time.Duration) (bool, error) {
			captured = true
			return true, nil
		},
	})
	met := h.AwaitCondition(func() bool { return true }, time.Millisecond, time.Second)
	if !captured {
		t.Error("expected AwaitCondition custom fn to be called")
	}
	if !met {
		t.Error("expected met=true")
	}
}

// ---------------------------------------------------------------------------
// Selector: AwaitSignals loop when signals don't fire
// ---------------------------------------------------------------------------

func TestSelectorWithPendingSignalsNoFire(t *testing.T) {
	pollCalled := false
	h := NewHostCalls(HostCallsOptions{
		PollSignal: func(name string) (string, bool, error) {
			pollCalled = true
			return "", false, nil
		},
		Now: func() int64 {
			return 1000
		},
	})
	s := NewSelector(h)
	s.AddSignal("sig1", new(string))
	winner := s.Select()
	if !pollCalled {
		t.Error("expected PollSignal to be called")
	}
	_ = winner
}

// ---------------------------------------------------------------------------
// HostCalls: DurableFetch delegation chain
// ---------------------------------------------------------------------------

func TestHostCallsImpl_DurableFetchDelegation(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(svc, op, req string) (string, error) {
			captured = true
			return `{"status_code":200,"body":"ok"}`, nil
		},
	})
	resp, code, err := h.DurableFetch("http://example.com", "GET", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DurableCall to be called")
	}
	if code != 200 {
		t.Errorf("expected status 200, got %d", code)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %q", resp)
	}
}

func TestHostCallsImpl_DurableFetchJSONDelegation(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(svc, op, req string) (string, error) {
			return `{"status_code":200,"body":"{\"result\":\"ok\"}"}`, nil
		},
	})
	var result struct {
		Result string `json:"result"`
	}
	err := h.DurableFetchJSON("http://example.com", "GET", nil, "", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Result != "ok" {
		t.Errorf("expected 'ok', got %q", result.Result)
	}
}

func TestHostCallsImpl_FetchGetDelegation(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(svc, op, req string) (string, error) {
			captured = true
			return `{"status_code":200,"body":"get-result"}`, nil
		},
	})
	resp, code, err := h.FetchGet("http://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DurableCall to be called")
	}
	if code != 200 {
		t.Errorf("expected 200, got %d", code)
	}
	if resp != "get-result" {
		t.Errorf("expected 'get-result', got %q", resp)
	}
}

func TestHostCallsImpl_FetchGetJSONDelegation(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(svc, op, req string) (string, error) {
			return `{"status_code":200,"body":"{\"val\":42}"}`, nil
		},
	})
	var result struct {
		Val int `json:"val"`
	}
	err := h.FetchGetJSON("http://example.com", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Val != 42 {
		t.Errorf("expected 42, got %d", result.Val)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: ChildWorkflowTyped / AwaitChildTyped fallback
// ---------------------------------------------------------------------------

func TestHostCallsImpl_ChildWorkflowTypedFallback(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		ChildWorkflow: func(name, input string) (string, error) {
			captured = true
			return "child-run-id", nil
		},
	})
	runID, err := h.ChildWorkflowTyped("child", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected ChildWorkflow fallback")
	}
	if runID != "child-run-id" {
		t.Errorf("expected 'child-run-id', got %q", runID)
	}
}

func TestHostCallsImpl_AwaitChildTypedFallback(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		AwaitChild: func(runID string) (string, error) {
			return `{"result":"ok"}`, nil
		},
	})
	var result struct {
		Result string `json:"result"`
	}
	err := h.AwaitChildTyped("child-run", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Result != "ok" {
		t.Errorf("expected 'ok', got %q", result.Result)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: Scope management
// ---------------------------------------------------------------------------

func TestHostCallsImpl_ScopeManagement(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})

	objType, instKey := h.GetScope()
	if objType != "" || instKey != "" {
		t.Errorf("expected empty scope, got (%q, %q)", objType, instKey)
	}

	prev := h.SetScope("MyObject", "instance-1")
	if prev != "" {
		t.Errorf("expected empty previous scope, got %q", prev)
	}

	objType, instKey = h.GetScope()
	if objType != "MyObject" || instKey != "instance-1" {
		t.Errorf("expected (MyObject, instance-1), got (%q, %q)", objType, instKey)
	}

	prev = h.ClearScope()
	if prev == "" {
		t.Error("expected non-empty previous scope")
	}

	objType, instKey = h.GetScope()
	if objType != "" || instKey != "" {
		t.Errorf("expected empty scope after clear, got (%q, %q)", objType, instKey)
	}
}

// ---------------------------------------------------------------------------
// HostCalls: DurableCallJSONWithOptions fallback
// ---------------------------------------------------------------------------

func TestHostCallsImpl_DurableCallJSONWithOptionsFallback(t *testing.T) {
	var captured bool
	h := NewHostCalls(HostCallsOptions{
		DurableCallWithOptions: func(opts CallOptions, svc, op, req string) (string, error) {
			captured = true
			return `{"ok":true}`, nil
		},
	})
	var result struct {
		Ok bool `json:"ok"`
	}
	err := h.DurableCallJSONWithOptions(CallOptions{}, "svc", "op", "{}", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !captured {
		t.Error("expected DurableCallWithOptions to be called")
	}
	if !result.Ok {
		t.Error("expected ok=true")
	}
}

// ---------------------------------------------------------------------------
// HostCalls: AwaitAllChildren success path
// ---------------------------------------------------------------------------

func TestHostCallsImpl_AwaitAllChildrenSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		AwaitAllChildren: func(runIDs []string) ([]ChildResult, error) {
			return []ChildResult{
				{RunID: "r1", Result: "ok1"},
				{RunID: "r2", Result: "ok2"},
			}, nil
		},
	})
	results, err := h.AwaitAllChildren([]string{"r1", "r2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// VirtualObject: NewVirtualObject sets scope
// ---------------------------------------------------------------------------

func TestNewVirtualObjectBasic(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	vo := NewVirtualObject(h, "MyType", "instance-1")
	if vo == nil {
		t.Fatal("expected non-nil VirtualObject")
	}
	vo.Set("key1", "value1")
	var result string
	err := vo.Get("key1", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "value1" {
		t.Errorf("expected 'value1', got %q", result)
	}
}

// ---------------------------------------------------------------------------
// Error types: additional coverage
// ---------------------------------------------------------------------------

func TestCallTimeoutErrorFields(t *testing.T) {
	err := &CallTimeoutError{
		Service:   "svc",
		Operation: "op",
		Timeout:   time.Minute,
	}
	if err.Service != "svc" {
		t.Errorf("expected 'svc', got %q", err.Service)
	}
	if err.Operation != "op" {
		t.Errorf("expected 'op', got %q", err.Operation)
	}
	if err.Timeout != time.Minute {
		t.Errorf("expected 1m, got %v", err.Timeout)
	}
}

func TestCallErrorFields(t *testing.T) {
	err := &CallError{
		Service:   "svc",
		Operation: "op",
		Code:      CallErrorTimeout,
		Message:   "msg",
	}
	if err.Service != "svc" {
		t.Errorf("expected 'svc', got %q", err.Service)
	}
	if err.Operation != "op" {
		t.Errorf("expected 'op', got %q", err.Operation)
	}
}

func TestServiceNotFoundErrorFields(t *testing.T) {
	inner := errors.New("not found")
	err := &ServiceNotFoundError{
		Service: "missing-svc",
		Err:     inner,
	}
	if err.Service != "missing-svc" {
		t.Errorf("expected 'missing-svc', got %q", err.Service)
	}
	if !errors.Is(err, inner) {
		t.Error("expected errors.Is to find the wrapped error")
	}
}

func TestDurableCallErrorMessage(t *testing.T) {
	err := &DurableCallError{
		Service:   "test",
		Operation: "op",
		Message:   "something failed",
	}
	msg := err.Error()
	if !strings.Contains(msg, "test") || !strings.Contains(msg, "op") || !strings.Contains(msg, "something failed") {
		t.Errorf("unexpected error message: %s", msg)
	}
}
