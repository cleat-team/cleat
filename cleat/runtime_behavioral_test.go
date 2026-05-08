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
		name     string
		code     CallErrorCode
		want     bool
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

	h := NewHostCalls(HostCallsOptions{}).(*hostCallsImpl)

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
	h := NewHostCalls(HostCallsOptions{}).(*hostCallsImpl)
	_, err := h.HandleUpdate("no_such_handler", `{}`)
	if err == nil {
		t.Fatal("expected error for unknown update handler")
	}
	if !strings.Contains(err.Error(), "no update handler registered") {
		t.Errorf("expected 'no update handler registered' in error, got %v", err)
	}
}

func TestHandleQueryRegistered(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{}).(*hostCallsImpl)
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
	h := NewHostCalls(HostCallsOptions{}).(*hostCallsImpl)
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
	}).(*hostCallsImpl)

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

	// Only step1, step2, step3 executed (no compensation for transient error).
	expected := []string{"step1", "step2", "step3"}
	if len(order) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("position %d: expected %q, got %q", i, v, order[i])
		}
	}
}
