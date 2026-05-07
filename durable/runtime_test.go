package durable

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// NewHostCalls
// ---------------------------------------------------------------------------

func TestNewHostCallsCreatesNonNil(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})
	if h == nil {
		t.Fatal("NewHostCalls returned nil")
	}
}

// ---------------------------------------------------------------------------
// DurableCall
// ---------------------------------------------------------------------------

func TestDurableCallPassesThrough(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(service, operation, requestJSON string) (string, error) {
			if service != "my_svc" || operation != "my_op" || requestJSON != `{"key":"val"}` {
				t.Errorf("unexpected args: %q %q %q", service, operation, requestJSON)
			}
			return `{"result":"ok"}`, nil
		},
	})

	resp, err := h.DurableCall("my_svc", "my_op", `{"key":"val"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != `{"result":"ok"}` {
		t.Errorf("expected %q, got %q", `{"result":"ok"}`, resp)
	}
}

func TestDurableCallReturnsErrorWhenNotInitialized(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})

	_, err := h.DurableCall("s", "o", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("expected 'not initialized' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DurableCallJSON
// ---------------------------------------------------------------------------

func TestDurableCallJSONUnmarshalsResponse(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			return `{"name":"test","value":42}`, nil
		},
	})

	var result struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}
	err := h.DurableCallJSON("svc", "op", "{}", &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Name != "test" || result.Value != 42 {
		t.Errorf("expected {test 42}, got {%s %d}", result.Name, result.Value)
	}
}

func TestDurableCallJSONReturnsErrorOnBadJSON(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			return "{bad json}", nil
		},
	})

	var result struct{}
	err := h.DurableCallJSON("svc", "op", "{}", &result)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshaling") {
		t.Errorf("expected unmarshaling error, got: %v", err)
	}
}

func TestDurableCallJSONPropagatesCallErrors(t *testing.T) {
	callErr := errors.New("service unavailable")
	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			return "", callErr
		},
	})

	var result struct{}
	err := h.DurableCallJSON("svc", "op", "{}", &result)
	if err != callErr {
		t.Errorf("expected original error %v, got %v", callErr, err)
	}
}

// ---------------------------------------------------------------------------
// DurableSleep / DurableSleepMs
// ---------------------------------------------------------------------------

func TestDurableSleepCallsDurableSleepMs(t *testing.T) {
	var capturedMs int64
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {
			capturedMs = ms
		},
	})

	h.DurableSleep(2 * time.Second)
	if capturedMs != 2000 {
		t.Errorf("expected 2000ms, got %d", capturedMs)
	}
}

func TestDurableSleepMsPanicsWhenNotInitialized(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()

	h := NewHostCalls(HostCallsOptions{})
	h.DurableSleepMs(100)
}

// ---------------------------------------------------------------------------
// AwaitSignals
// ---------------------------------------------------------------------------

func TestAwaitSignalsReturnsSignalResultOnSuccess(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "order_confirmed", `{"id":"abc"}`, false, nil
		},
	})

	result := h.AwaitSignals([]string{"order_confirmed"}, 30*time.Second)
	if result.Name != "order_confirmed" {
		t.Errorf("expected Name 'order_confirmed', got %q", result.Name)
	}
	if result.Payload != `{"id":"abc"}` {
		t.Errorf("expected Payload, got %q", result.Payload)
	}
	if result.TimedOut {
		t.Error("expected TimedOut to be false")
	}
	if result.Err != nil {
		t.Errorf("expected no error, got %v", result.Err)
	}
}

func TestAwaitSignalsReturnsSignalResultOnTimeout(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "", "", true, nil
		},
	})

	result := h.AwaitSignals([]string{"my_signal"}, time.Second)
	if !result.TimedOut {
		t.Error("expected TimedOut to be true")
	}
	if result.Name != "" {
		t.Errorf("expected empty Name, got %q", result.Name)
	}
}

func TestAwaitSignalsReturnsSignalResultOnSignal(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableAwaitSignals: func(names []string, timeoutMs int64) (string, string, bool, error) {
			return "mysignal", "mypayload", false, nil
		},
	})

	result := h.AwaitSignals([]string{"mysignal"}, time.Second)
	if result.Name != "mysignal" {
		t.Errorf("expected Name 'mysignal', got %q", result.Name)
	}
	if result.Payload != "mypayload" {
		t.Errorf("expected Payload 'mypayload', got %q", result.Payload)
	}
	if result.TimedOut {
		t.Error("expected TimedOut to be false")
	}
}

// ---------------------------------------------------------------------------
// LogKV
// ---------------------------------------------------------------------------

func TestLogKVProducesStructuredJSON(t *testing.T) {
	var captured string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {
			captured = msg
		},
	})

	h.LogKV("test message", "key1", "val1", "key2", 42)

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(captured), &result); err != nil {
		t.Fatalf("LogKV did not produce valid JSON: %v", err)
	}

	if result["msg"] != "test message" {
		t.Errorf("expected msg %q, got %v", "test message", result["msg"])
	}

	kvs, ok := result["kvs"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected kvs map, got %T", result["kvs"])
	}
	if kvs["key1"] != "val1" {
		t.Errorf("expected key1='val1', got %v", kvs["key1"])
	}
	// JSON numbers decode as float64
	if kvs["key2"] != float64(42) {
		t.Errorf("expected key2=42, got %v", kvs["key2"])
	}
}

func TestLogKVUnpairedKey(t *testing.T) {
	var captured string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {
			captured = msg
		},
	})

	h.LogKV("odd", "k1", "v1", "lonely")

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(captured), &result); err != nil {
		t.Fatalf("LogKV did not produce valid JSON: %v", err)
	}

	kvs := result["kvs"].(map[string]interface{})
	if kvs["_unpaired"] != "lonely" {
		t.Errorf("expected _unpaired='lonely', got %v", kvs["_unpaired"])
	}
}

func TestLogKVNoKVs(t *testing.T) {
	var captured string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {
			captured = msg
		},
	})

	h.LogKV("just a message")

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(captured), &result); err != nil {
		t.Fatalf("LogKV did not produce valid JSON: %v", err)
	}
	if result["msg"] != "just a message" {
		t.Errorf("expected msg %q, got %v", "just a message", result["msg"])
	}
	if _, ok := result["kvs"]; ok {
		t.Error("expected no kvs key when no key-value pairs provided")
	}
}

// ---------------------------------------------------------------------------
// Now / NowMs
// ---------------------------------------------------------------------------

func TestNowReturnsTimeFromMilliseconds(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		Now: func() int64 {
			return 1_600_000_000_000 // 2020-09-13T12:26:40Z
		},
	})

	now := h.Now()
	expected := time.Unix(1600000000000/1000, (1600000000000%1000)*1_000_000)
	if !now.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, now)
	}
}

func TestNowMsPanicsWhenNotInitialized(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()

	h := NewHostCalls(HostCallsOptions{})
	h.NowMs()
}

// ---------------------------------------------------------------------------
// PollCancellation, Version, SetQueryState (nil field defaults)
// ---------------------------------------------------------------------------

func TestPollCancellationReturnsFalseEmptyWhenNotInitialized(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})

	cancelled, reason := h.PollCancellation()
	if cancelled {
		t.Error("expected cancelled to be false")
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
}

func TestVersionReturnsOneWhenNotInitialized(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})

	v := h.Version()
	if v != 1 {
		t.Errorf("expected version 1, got %d", v)
	}
}

func TestSetQueryStateIsNoOpWhenNotInitialized(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{})

	// Should not panic.
	h.SetQueryState("key", "value")
}

// ---------------------------------------------------------------------------
// Saga
// ---------------------------------------------------------------------------

func TestSagaExecutesAllStepsInOrder(t *testing.T) {
	var order []string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {},
	})
	s := NewSaga()
	s.AddStep("step1",
		func(h HostCalls) (string, error) { order = append(order, "step1"); return "", nil },
		func(h HostCalls) error { order = append(order, "comp1"); return nil },
	)
	s.AddStep("step2",
		func(h HostCalls) (string, error) { order = append(order, "step2"); return "", nil },
		func(h HostCalls) error { order = append(order, "comp2"); return nil },
	)
	s.AddStep("step3",
		func(h HostCalls) (string, error) { order = append(order, "step3"); return "", nil },
		func(h HostCalls) error { order = append(order, "comp3"); return nil },
	)

	err := s.Run(h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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

func TestSagaCompensatesInReverseOrderOnFailure(t *testing.T) {
	var order []string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {},
	})
	s := NewSaga()
	s.AddStep("step1",
		func(h HostCalls) (string, error) { order = append(order, "step1"); return "", nil },
		func(h HostCalls) error { order = append(order, "comp1"); return nil },
	)
	s.AddStep("step2",
		func(h HostCalls) (string, error) { order = append(order, "step2"); return "", nil },
		func(h HostCalls) error { order = append(order, "comp2"); return nil },
	)
	s.AddStep("step3",
		func(h HostCalls) (string, error) { order = append(order, "step3"); return "", errors.New("step3 failed") },
		func(h HostCalls) error { order = append(order, "comp3"); return nil },
	)

	err := s.Run(h)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "step3 failed") {
		t.Errorf("expected error containing 'step3 failed', got %v", err)
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

func TestSagaRunsCompensationEvenIfSomeCompensationsFail(t *testing.T) {
	var order []string
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {},
	})
	s := NewSaga()
	s.AddStep("step1",
		func(h HostCalls) (string, error) { order = append(order, "step1"); return "", nil },
		func(h HostCalls) error { order = append(order, "comp1"); return nil },
	)
	s.AddStep("step2",
		func(h HostCalls) (string, error) { order = append(order, "step2"); return "", nil },
		func(h HostCalls) error { order = append(order, "comp2"); return nil },
	)
	s.AddStep("step3",
		func(h HostCalls) (string, error) { order = append(order, "step3"); return "", errors.New("step3 failed") },
		func(h HostCalls) error { order = append(order, "comp3"); return nil },
	)

	err := s.Run(h)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "step3 failed") {
		t.Errorf("expected error containing 'step3 failed', got %v", err)
	}

	// comp1 may have failed but it should still have been called.
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

func TestSagaReturnsForwardError(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableLog: func(msg string) {},
	})
	s := NewSaga()
	s.AddStep("goodStep",
		func(h HostCalls) (string, error) { return "", nil },
		func(h HostCalls) error { return nil },
	)
	s.AddStep("badStep",
		func(h HostCalls) (string, error) { return "", errors.New("boom") },
		func(h HostCalls) error { return nil },
	)

	err := s.Run(h)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "saga:") {
		t.Errorf("expected error wrapping 'saga:', got %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected error containing 'boom', got %v", err)
	}
}

// ---------------------------------------------------------------------------
// PollUntil
// ---------------------------------------------------------------------------

func TestPollUntilReturnsValueWhenDoneConditionMet(t *testing.T) {
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		Now:          func() int64 { return 1000 },
	})

	result, err := PollUntil(h, time.Second, time.Minute,
		func() (string, error) { return "done_value", nil },
		func(s string) bool { return s == "done_value" },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "done_value" {
		t.Errorf("expected 'done_value', got %q", result)
	}
}

func TestPollUntilErrorsWhenFnReturnsError(t *testing.T) {
	expectedErr := errors.New("internal error")
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		Now:          func() int64 { return 1000 },
	})

	result, err := PollUntil(h, time.Second, time.Minute,
		func() (string, error) { return "", expectedErr },
		func(s string) bool { return false },
	)
	if err != expectedErr {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}
	if result != "" {
		t.Errorf("expected zero value, got %q", result)
	}
}

func TestPollUntilErrorsOnDeadlineExceeded(t *testing.T) {
	nowCalls := 0
	h := NewHostCalls(HostCallsOptions{
		DurableSleep: func(ms int64) {},
		Now: func() int64 {
			nowCalls++
			if nowCalls == 1 {
				return 1000 // 1 second
			}
			return 5000 // 5 seconds (past deadline of now+1s → Unix(2,0))
		},
	})

	_, err := PollUntil(h, time.Second, time.Second,
		func() (string, error) { return "pending", nil },
		func(s string) bool { return false },
	)
	if err == nil {
		t.Fatal("expected deadline exceeded error, got nil")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("expected 'deadline exceeded' error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Retry Policy (DurableCallWithOptions default implementation)
// ---------------------------------------------------------------------------

func TestRetryBackoffCalculation(t *testing.T) {
	var attemptCount int
	var sleepDurations []time.Duration

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			attemptCount++
			return "", errors.New("transient error")
		},
		DurableSleep: func(ms int64) {
			sleepDurations = append(sleepDurations, time.Duration(ms)*time.Millisecond)
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 2.0,
		MaxInterval:        30 * time.Second,
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attemptCount != 3 {
		t.Errorf("expected 3 attempts, got %d", attemptCount)
	}

	expectedSleeps := []time.Duration{1 * time.Second, 2 * time.Second}
	if len(sleepDurations) != len(expectedSleeps) {
		t.Fatalf("expected %d sleeps, got %d: %v", len(expectedSleeps), len(sleepDurations), sleepDurations)
	}
	for i, expected := range expectedSleeps {
		if sleepDurations[i] != expected {
			t.Errorf("sleep %d: expected %v, got %v", i, expected, sleepDurations[i])
		}
	}
}

func TestRetryMaxIntervalCapsBackoff(t *testing.T) {
	var attemptCount int
	var sleepDurations []time.Duration

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			attemptCount++
			return "", errors.New("transient error")
		},
		DurableSleep: func(ms int64) {
			sleepDurations = append(sleepDurations, time.Duration(ms)*time.Millisecond)
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        4,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 100.0,
		MaxInterval:        5 * time.Second,
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attemptCount != 4 {
		t.Errorf("expected 4 attempts, got %d", attemptCount)
	}
	if len(sleepDurations) != 3 {
		t.Fatalf("expected 3 sleeps, got %d: %v", len(sleepDurations), sleepDurations)
	}
	// attempt=1: 1s * 100.0^0 = 1s (below MaxInterval=5s, no cap)
	// attempt=2: 1s * 100.0^1 = 100s (capped to 5s)
	// attempt=3: 1s * 100.0^2 = 10000s (capped to 5s)
	expected := []time.Duration{1 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, exp := range expected {
		if sleepDurations[i] != exp {
			t.Errorf("sleep %d: expected %v, got %v", i, exp, sleepDurations[i])
		}
	}
}

func TestRetryNonRetryableError(t *testing.T) {
	var attemptCount int
	var sleepDurations []time.Duration

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			attemptCount++
			return "", errors.New("InvalidRequest: bad input")
		},
		DurableSleep: func(ms int64) {
			sleepDurations = append(sleepDurations, time.Duration(ms)*time.Millisecond)
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 2.0,
		MaxInterval:        30 * time.Second,
		NonRetryableErrors: []string{"InvalidRequest"},
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "InvalidRequest") {
		t.Errorf("expected error containing 'InvalidRequest', got %v", err)
	}
	if attemptCount != 1 {
		t.Errorf("expected 1 attempt, got %d", attemptCount)
	}
	if len(sleepDurations) != 0 {
		t.Errorf("expected 0 sleeps, got %d", len(sleepDurations))
	}
}

func TestRetryExhaustionReturnsLastError(t *testing.T) {
	var attemptCount int
	originalErr := errors.New("service unavailable")

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			attemptCount++
			return "", originalErr
		},
		DurableSleep: func(ms int64) {},
	})

	rp := RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 2.0,
		MaxInterval:        30 * time.Second,
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "retry exhausted") {
		t.Errorf("expected error containing 'retry exhausted', got %v", err)
	}
	if !errors.Is(err, originalErr) {
		t.Errorf("expected error to wrap original error, got %v", err)
	}
	if attemptCount != 3 {
		t.Errorf("expected 3 attempts, got %d", attemptCount)
	}
}

func TestRetryNilPolicyDelegatesToDurableCall(t *testing.T) {
	var callCount int

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			callCount++
			return "ok", nil
		},
		DurableSleep: func(ms int64) {
			t.Error("DurableSleep should not be called")
		},
	})

	resp, err := h.DurableCallWithOptions(CallOptions{}, "svc", "op", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %q", resp)
	}
	if callCount != 1 {
		t.Errorf("expected 1 call, got %d", callCount)
	}
}

// ---------------------------------------------------------------------------
// DefaultRetryPolicy
// ---------------------------------------------------------------------------

func TestDefaultRetryPolicyValues(t *testing.T) {
	rp := DefaultRetryPolicy()
	if rp.MaxAttempts != 3 {
		t.Errorf("expected MaxAttempts=3, got %d", rp.MaxAttempts)
	}
	if rp.InitialInterval != 1*time.Second {
		t.Errorf("expected InitialInterval=1s, got %v", rp.InitialInterval)
	}
	if rp.BackoffCoefficient != 2.0 {
		t.Errorf("expected BackoffCoefficient=2.0, got %f", rp.BackoffCoefficient)
	}
	if rp.MaxInterval != 30*time.Second {
		t.Errorf("expected MaxInterval=30s, got %v", rp.MaxInterval)
	}
	if rp.NonRetryableErrors != nil {
		t.Errorf("expected NonRetryableErrors=nil, got %v", rp.NonRetryableErrors)
	}
}

// ---------------------------------------------------------------------------
// isNonRetryable
// ---------------------------------------------------------------------------

func TestIsNonRetryableEmptyList(t *testing.T) {
	err := errors.New("something went wrong")
	if isNonRetryable(err, []string{}) {
		t.Error("expected false for empty NonRetryableErrors list")
	}
}

func TestIsNonRetryableNilList(t *testing.T) {
	err := errors.New("something went wrong")
	if isNonRetryable(err, nil) {
		t.Error("expected false for nil NonRetryableErrors slice")
	}
}

func TestIsNonRetryableNoMatch(t *testing.T) {
	err := errors.New("something went wrong")
	patterns := []string{"NotFound", "InvalidRequest", "Timeout"}
	if isNonRetryable(err, patterns) {
		t.Error("expected false when no patterns match")
	}
}

// ---------------------------------------------------------------------------
// RetryPolicy edge cases
// ---------------------------------------------------------------------------

func TestRetryZeroMaxAttempts(t *testing.T) {
	var callCount int
	var sleepCount int

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			callCount++
			return "", errors.New("error")
		},
		DurableSleep: func(ms int64) {
			sleepCount++
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        0,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 2.0,
		MaxInterval:        30 * time.Second,
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if callCount != 0 {
		t.Errorf("expected 0 calls, got %d", callCount)
	}
	if sleepCount != 0 {
		t.Errorf("expected 0 sleeps, got %d", sleepCount)
	}
}

func TestRetryBackoffCoefficientOne(t *testing.T) {
	var attemptCount int
	var sleepDurations []time.Duration

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			attemptCount++
			return "", errors.New("transient error")
		},
		DurableSleep: func(ms int64) {
			sleepDurations = append(sleepDurations, time.Duration(ms)*time.Millisecond)
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 1.0,
		MaxInterval:        30 * time.Second,
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attemptCount != 3 {
		t.Errorf("expected 3 attempts, got %d", attemptCount)
	}

	// BackoffCoefficient=1.0 means no growth: all sleeps = InitialInterval
	expected := []time.Duration{1 * time.Second, 1 * time.Second}
	if len(sleepDurations) != len(expected) {
		t.Fatalf("expected %d sleeps, got %d: %v", len(expected), len(sleepDurations), sleepDurations)
	}
	for i, exp := range expected {
		if sleepDurations[i] != exp {
			t.Errorf("sleep %d: expected %v, got %v", i, exp, sleepDurations[i])
		}
	}
}

func TestRetryBackoffCoefficientZero(t *testing.T) {
	var attemptCount int
	var sleepDurations []time.Duration

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			attemptCount++
			return "", errors.New("transient error")
		},
		DurableSleep: func(ms int64) {
			sleepDurations = append(sleepDurations, time.Duration(ms)*time.Millisecond)
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 0,
		MaxInterval:        30 * time.Second,
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attemptCount != 3 {
		t.Errorf("expected 3 attempts, got %d", attemptCount)
	}

	// BackoffCoefficient=0: attempt=1: 1s * 0^0 = 1s; attempt=2: 1s * 0^1 = 0s
	expected := []time.Duration{1 * time.Second, 0}
	if len(sleepDurations) != len(expected) {
		t.Fatalf("expected %d sleeps, got %d: %v", len(expected), len(sleepDurations), sleepDurations)
	}
	for i, exp := range expected {
		if sleepDurations[i] != exp {
			t.Errorf("sleep %d: expected %v, got %v", i, exp, sleepDurations[i])
		}
	}
}

func TestRetryInitialIntervalZero(t *testing.T) {
	var attemptCount int
	var sleepDurations []time.Duration

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			attemptCount++
			return "", errors.New("transient error")
		},
		DurableSleep: func(ms int64) {
			sleepDurations = append(sleepDurations, time.Duration(ms)*time.Millisecond)
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    0,
		BackoffCoefficient: 2.0,
		MaxInterval:        30 * time.Second,
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attemptCount != 3 {
		t.Errorf("expected 3 attempts, got %d", attemptCount)
	}

	// InitialInterval=0: all backoff calculations produce 0
	expected := []time.Duration{0, 0}
	if len(sleepDurations) != len(expected) {
		t.Fatalf("expected %d sleeps, got %d: %v", len(expected), len(sleepDurations), sleepDurations)
	}
	for i, exp := range expected {
		if sleepDurations[i] != exp {
			t.Errorf("sleep %d: expected %v, got %v", i, exp, sleepDurations[i])
		}
	}
}

func TestRetryMaxIntervalSmallerThanInitial(t *testing.T) {
	var attemptCount int
	var sleepDurations []time.Duration

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			attemptCount++
			return "", errors.New("transient error")
		},
		DurableSleep: func(ms int64) {
			sleepDurations = append(sleepDurations, time.Duration(ms)*time.Millisecond)
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    5 * time.Second,
		BackoffCoefficient: 2.0,
		MaxInterval:        2 * time.Second,
	}

	_, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attemptCount != 3 {
		t.Errorf("expected 3 attempts, got %d", attemptCount)
	}

	// All sleeps capped at MaxInterval=2s since even the first backoff
	// (5s * 2.0^0 = 5s) exceeds MaxInterval.
	expected := []time.Duration{2 * time.Second, 2 * time.Second}
	if len(sleepDurations) != len(expected) {
		t.Fatalf("expected %d sleeps, got %d: %v", len(expected), len(sleepDurations), sleepDurations)
	}
	for i, exp := range expected {
		if sleepDurations[i] != exp {
			t.Errorf("sleep %d: expected %v, got %v", i, exp, sleepDurations[i])
		}
	}
}

func TestRetrySuccessOnFirstAttempt(t *testing.T) {
	var callCount int
	var sleepCount int

	h := NewHostCalls(HostCallsOptions{
		DurableCall: func(_, _, _ string) (string, error) {
			callCount++
			return "ok", nil
		},
		DurableSleep: func(ms int64) {
			sleepCount++
		},
	})

	rp := RetryPolicy{
		MaxAttempts:        3,
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 2.0,
		MaxInterval:        30 * time.Second,
	}

	resp, err := h.DurableCallWithOptions(CallOptions{Retry: &rp}, "svc", "op", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %q", resp)
	}
	if callCount != 1 {
		t.Errorf("expected 1 call, got %d", callCount)
	}
	if sleepCount != 0 {
		t.Errorf("expected 0 sleeps, got %d", sleepCount)
	}
}
