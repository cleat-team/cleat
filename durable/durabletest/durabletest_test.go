package durabletest

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/rcownie/durable/durable"
)

// mockT is a TestingT that captures Fatalf calls.
type mockT struct {
	fatalfCalled bool
	format       string
	args         []interface{}
}

func (m *mockT) Fatalf(format string, args ...interface{}) {
	m.fatalfCalled = true
	m.format = format
	m.args = args
}

// ---------------------------------------------------------------------------
// 1. H() returns non-nil
// ---------------------------------------------------------------------------

func TestHReturnsNonNil(t *testing.T) {
	env := NewTestEnv()
	if env.H() == nil {
		t.Fatal("H() returned nil")
	}
}

// ---------------------------------------------------------------------------
// 2. OnCall with string matcher
// ---------------------------------------------------------------------------

func TestOnCallStringMatcher(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", "exact-request").Return("response-value", nil)

	resp, err := env.H().DurableCall("svc", "op", "exact-request")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "response-value" {
		t.Fatalf("expected %q, got %q", "response-value", resp)
	}

	// A non-matching request should NOT match (and produce no stub error).
	_, err = env.H().DurableCall("svc", "op", "other-request")
	if err == nil {
		t.Fatal("expected error for non-matching request")
	}
}

// ---------------------------------------------------------------------------
// 3. OnCall with nil matcher matches any
// ---------------------------------------------------------------------------

func TestOnCallNilMatcher(t *testing.T) {
	env := NewTestEnv()

	// A nil matcher should match any request string.
	env.OnCall("svc", "op", nil).Return("any-match", nil)

	resp, err := env.H().DurableCall("svc", "op", "request-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "any-match" {
		t.Fatalf("expected %q, got %q", "any-match", resp)
	}

	// A different request also matches (with a new stub for consuming).
	env.OnCall("svc", "op", nil).Return("also-match", nil)
	resp, err = env.H().DurableCall("svc", "op", "request-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "also-match" {
		t.Fatalf("expected %q, got %q", "also-match", resp)
	}

	// Verify the call history captured the actual requests.
	history := env.CallHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(history))
	}
	if history[0].Request != "request-a" {
		t.Fatalf("expected request %q, got %q", "request-a", history[0].Request)
	}
	if history[1].Request != "request-b" {
		t.Fatalf("expected request %q, got %q", "request-b", history[1].Request)
	}
}

// ---------------------------------------------------------------------------
// 4. OnCall with func matcher
// ---------------------------------------------------------------------------

func TestOnCallFuncMatcher(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", func(s string) bool { return len(s) > 5 }).Return("long", nil)
	env.OnCall("svc", "op", func(s string) bool { return len(s) <= 5 }).Return("short", nil)

	resp, err := env.H().DurableCall("svc", "op", "hello!!")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "long" {
		t.Fatalf("expected %q, got %q", "long", resp)
	}

	resp, err = env.H().DurableCall("svc", "op", "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "short" {
		t.Fatalf("expected %q, got %q", "short", resp)
	}
}

// ---------------------------------------------------------------------------
// 5. ReturnJSON marshals correctly
// ---------------------------------------------------------------------------

func TestReturnJSON(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).ReturnJSON(map[string]string{"key": "val"}, nil)

	resp, err := env.H().DurableCall("svc", "op", "req")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(resp), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got["key"] != "val" {
		t.Fatalf("expected key=val in JSON, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 6. Call history records calls
// ---------------------------------------------------------------------------

func TestCallHistory(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op1", nil).Return("resp1", nil)
	env.OnCall("svc", "op2", nil).Return("resp2", nil)

	env.H().DurableCall("svc", "op1", "req1")
	env.H().DurableCall("svc", "op2", "req2")

	history := env.CallHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(history))
	}

	check := func(idx int, service, operation, request, response string) {
		r := history[idx]
		if r.Service != service || r.Operation != operation || r.Request != request || r.Response != response {
			t.Fatalf("call[%d] = %+v; want Service=%q Operation=%q Request=%q Response=%q",
				idx, r, service, operation, request, response)
		}
	}
	check(0, "svc", "op1", "req1", "resp1")
	check(1, "svc", "op2", "req2", "resp2")

	// Err should be nil for successful calls
	if history[0].Err != nil {
		t.Fatalf("expected nil error, got %v", history[0].Err)
	}
}

// ---------------------------------------------------------------------------
// 7. AssertCalled / AssertNotCalled
// ---------------------------------------------------------------------------

func TestAssertCalledPasses(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return("resp", nil)
	env.H().DurableCall("svc", "op", "req")

	// This should not call Fatalf.
	env.AssertCalled(t, "svc", "op")
}

func TestAssertCalledFails(t *testing.T) {
	env := NewTestEnv()
	mt := &mockT{}
	env.AssertCalled(mt, "svc", "op")
	if !mt.fatalfCalled {
		t.Fatal("expected Fatalf to be called when call was not made")
	}
}

func TestAssertNotCalledPasses(t *testing.T) {
	env := NewTestEnv()
	env.AssertNotCalled(t, "svc", "op")
}

func TestAssertNotCalledFails(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return("resp", nil)
	env.H().DurableCall("svc", "op", "req")

	mt := &mockT{}
	env.AssertNotCalled(mt, "svc", "op")
	if !mt.fatalfCalled {
		t.Fatal("expected Fatalf to be called when call was made")
	}
}

// ---------------------------------------------------------------------------
// 8. AfterSignal -> AwaitSignals receives it
// ---------------------------------------------------------------------------

func TestAfterSignal(t *testing.T) {
	env := NewTestEnv()
	env.AfterSignal(100*time.Millisecond, "sig1", "payload1")

	// Before time advances, no signal should be available.
	name, payload, timedOut, err := env.H().DurableAwaitSignals([]string{"sig1"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !timedOut {
		t.Fatal("expected timeout before time advance")
	}

	// Advance time past the delay.
	env.AdvanceTime(200 * time.Millisecond)

	// Now the signal should be available.
	name, payload, timedOut, err = env.H().DurableAwaitSignals([]string{"sig1"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout after advancing time")
	}
	if name != "sig1" {
		t.Fatalf("expected signal name %q, got %q", "sig1", name)
	}
	if payload != "payload1" {
		t.Fatalf("expected signal payload %q, got %q", "payload1", payload)
	}
}

func TestSignalDeliversImmediately(t *testing.T) {
	env := NewTestEnv()
	env.Signal("sig1", "immediate-payload")

	name, payload, timedOut, err := env.H().DurableAwaitSignals([]string{"sig1"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout for immediately delivered signal")
	}
	if name != "sig1" {
		t.Fatalf("expected signal name %q, got %q", "sig1", name)
	}
	if payload != "immediate-payload" {
		t.Fatalf("expected payload %q, got %q", "immediate-payload", payload)
	}
}

// ---------------------------------------------------------------------------
// 9. AdvanceTime advances clock
// ---------------------------------------------------------------------------

func TestAdvanceTime(t *testing.T) {
	env := NewTestEnv()
	start := env.Now()

	env.AdvanceTime(5 * time.Second)

	got := env.Now()
	expected := start.Add(5 * time.Second)
	if !got.Equal(expected) {
		t.Fatalf("expected Now()=%v after advancing 5s, got %v", expected, got)
	}

	// Advance again.
	env.AdvanceTime(3 * time.Second)
	expected2 := expected.Add(3 * time.Second)
	if !env.Now().Equal(expected2) {
		t.Fatalf("expected Now()=%v after another 3s, got %v", expected2, env.Now())
	}
}

// ---------------------------------------------------------------------------
// 10. DurableSleep unblocks after AdvanceTime
// ---------------------------------------------------------------------------

func TestDurableSleep(t *testing.T) {
	env := NewTestEnv()

	slept := make(chan struct{})
	go func() {
		env.H().DurableSleep(1 * time.Second)
		close(slept)
	}()

	// Give the goroutine a moment to enter the sleep.
	time.Sleep(5 * time.Millisecond)

	// Should still be blocked.
	select {
	case <-slept:
		t.Fatal("DurableSleep returned before AdvanceTime")
	default:
	}

	// Advance time past the sleep duration.
	env.AdvanceTime(2 * time.Second)

	select {
	case <-slept:
		// Success.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("DurableSleep did not unblock after AdvanceTime")
	}
}

// ---------------------------------------------------------------------------
// 11. SetVersion / SetMinVersion
// ---------------------------------------------------------------------------

func TestSetVersionAndMinVersion(t *testing.T) {
	env := NewTestEnv()

	if v := env.H().Version(); v != 1 {
		t.Fatalf("expected default Version()=1, got %d", v)
	}
	if v := env.H().MinVersion(); v != 1 {
		t.Fatalf("expected default MinVersion()=1, got %d", v)
	}

	env.SetVersion(5)
	if v := env.H().Version(); v != 5 {
		t.Fatalf("expected Version()=5, got %d", v)
	}

	env.SetMinVersion(3)
	if v := env.H().MinVersion(); v != 3 {
		t.Fatalf("expected MinVersion()=3, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// 12. QueryState round-trip
// ---------------------------------------------------------------------------

func TestQueryState(t *testing.T) {
	env := NewTestEnv()

	env.H().SetQueryState("key1", "value1")
	env.H().SetQueryState("key2", "value2")

	val, ok := env.QueryState("key1")
	if !ok {
		t.Fatal("expected key1 to exist")
	}
	if val != "value1" {
		t.Fatalf("expected %q, got %q", "value1", val)
	}

	val, ok = env.QueryState("key2")
	if !ok {
		t.Fatal("expected key2 to exist")
	}
	if val != "value2" {
		t.Fatalf("expected %q, got %q", "value2", val)
	}

	// Non-existent key.
	_, ok = env.QueryState("nonexistent")
	if ok {
		t.Fatal("expected nonexistent key to return false")
	}
}

// ---------------------------------------------------------------------------
// 13. Reset clears state
// ---------------------------------------------------------------------------

func TestReset(t *testing.T) {
	env := NewTestEnv()

	// Set up various state.
	env.OnCall("svc", "op", nil).Return("resp", nil)
	env.H().DurableCall("svc", "op", "req")
	env.SetVersion(5)
	env.H().SetQueryState("k", "v")
	env.SetRandomSeq([]int64{42})

	_ = env.H().Random() // consume the random value

	env.Reset()

	// Call history cleared.
	if len(env.CallHistory()) != 0 {
		t.Fatal("expected empty call history after Reset")
	}

	// Stubs cleared -- unregistered call should now error.
	_, err := env.H().DurableCall("svc", "op", "req")
	if err == nil {
		t.Fatal("expected error after Reset because stubs were cleared")
	}

	// Version reset.
	if v := env.H().Version(); v != 1 {
		t.Fatalf("expected Version()=1 after Reset, got %d", v)
	}
	if v := env.H().MinVersion(); v != 1 {
		t.Fatalf("expected MinVersion()=1 after Reset, got %d", v)
	}

	// Query state cleared.
	if _, ok := env.QueryState("k"); ok {
		t.Fatal("expected query state to be cleared after Reset")
	}

	// Random sequence cleared.
	// SetRandomSeq sets sequence; after Reset, the old seq should be gone.
	// We verify by checking Random() returns 0 (no sequence).
	if v := env.H().Random(); v != 0 {
		t.Fatalf("expected Random()=0 after Reset, got %d", v)
	}

	// Time reset to default (2024-01-01).
	expected := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if !env.Now().Equal(expected) {
		t.Fatalf("expected Now()=%v after Reset, got %v", expected, env.Now())
	}
}

// ---------------------------------------------------------------------------
// 14. SetRandomSeq
// ---------------------------------------------------------------------------

func TestSetRandomSeq(t *testing.T) {
	env := NewTestEnv()
	env.SetRandomSeq([]int64{42, 100, 200})

	vals := []int64{env.H().Random(), env.H().Random(), env.H().Random()}
	expected := []int64{42, 100, 200}
	for i, v := range vals {
		if v != expected[i] {
			t.Fatalf("Random() call %d: expected %d, got %d", i, expected[i], v)
		}
	}

	// After exhaustion, returns 0.
	if v := env.H().Random(); v != 0 {
		t.Fatalf("expected 0 after sequence exhaustion, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// 15. Unregistered call returns error
// ---------------------------------------------------------------------------

func TestUnregisteredCall(t *testing.T) {
	env := NewTestEnv()

	_, err := env.H().DurableCall("svc", "op", "req")
	if err == nil {
		t.Fatal("expected error for unregistered call")
	}

	// Verify the error message is descriptive.
	msg := err.Error()
	if msg == "" {
		t.Fatal("expected non-empty error message")
	}
}

// ---------------------------------------------------------------------------
// 16. Multiple signals ordering
// ---------------------------------------------------------------------------

func TestMultipleSignalsOrdering(t *testing.T) {
	env := NewTestEnv()
	env.AfterSignal(10*time.Millisecond, "sig1", "payload1")
	env.AfterSignal(10*time.Millisecond, "sig2", "payload2")

	env.AdvanceTime(20 * time.Millisecond)

	// Should receive sig1 first (first registered).
	name, payload, timedOut, err := env.H().DurableAwaitSignals([]string{"sig1", "sig2"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	if name != "sig1" {
		t.Fatalf("expected sig1 first, got %q", name)
	}
	if payload != "payload1" {
		t.Fatalf("expected payload1, got %q", payload)
	}

	// Then sig2.
	name, payload, timedOut, err = env.H().DurableAwaitSignals([]string{"sig2"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	if name != "sig2" {
		t.Fatalf("expected sig2, got %q", name)
	}
	if payload != "payload2" {
		t.Fatalf("expected payload2, got %q", payload)
	}
}

// ---------------------------------------------------------------------------
// Additional: SetTime
// ---------------------------------------------------------------------------

func TestSetTime(t *testing.T) {
	env := NewTestEnv()
	newTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	env.SetTime(newTime)

	if !env.Now().Equal(newTime) {
		t.Fatalf("expected Now()=%v, got %v", newTime, env.Now())
	}

	// H().Now() should also reflect the set time.
	hNow := env.H().Now()
	if !hNow.Equal(newTime) {
		t.Fatalf("expected H().Now()=%v, got %v", newTime, hNow)
	}
}

// ---------------------------------------------------------------------------
// Additional: DurableCall variants route through stubs
// ---------------------------------------------------------------------------

func TestDurableCallWithOptionsRoutesToStubs(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return("stubbed", nil)

	resp, err := env.H().DurableCallWithOptions(durable.CallOptions{}, "svc", "op", "req")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "stubbed" {
		t.Fatalf("expected %q, got %q", "stubbed", resp)
	}

	// Verify it was recorded.
	if len(env.CallHistory()) != 1 {
		t.Fatalf("expected 1 call in history, got %d", len(env.CallHistory()))
	}
}

func TestDurableCallJSONRoutesToStubs(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).ReturnJSON(map[string]string{"result": "ok"}, nil)

	var result struct {
		Result string `json:"result"`
	}
	err := env.H().DurableCallJSON("svc", "op", `{"input":"data"}`, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Result != "ok" {
		t.Fatalf("expected result=ok, got %q", result.Result)
	}
}

// ---------------------------------------------------------------------------
// Additional: PollSignal returns due signals
// ---------------------------------------------------------------------------

func TestPollSignal(t *testing.T) {
	env := NewTestEnv()
	env.Signal("sig1", "poll-payload")

	payload, found, err := env.H().PollSignal("sig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected PollSignal to find signal")
	}
	if payload != "poll-payload" {
		t.Fatalf("expected %q, got %q", "poll-payload", payload)
	}

	// Second poll should not find it (consumed).
	_, found, err = env.H().PollSignal("sig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected PollSignal to not find consumed signal")
	}
}

// ---------------------------------------------------------------------------
// Additional: thread safety smoke test
// ---------------------------------------------------------------------------

func TestConcurrentAccess(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return("resp", nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			env.H().DurableCall("svc", "op", "req")
			env.AdvanceTime(time.Millisecond)
			env.Signal("s", "p")
		}
		close(done)
	}()

	go func() {
		for i := 0; i < 100; i++ {
			env.CallHistory()
			env.Now()
			env.H().Version()
			env.QueryState("k")
		}
	}()

	<-done
	// If we get here without a race, the smoke test passes.
}

// ---------------------------------------------------------------------------
// Additional: signal waiting with timeout via AwaitSignals
// ---------------------------------------------------------------------------

func TestAwaitSignalsWithTimeout(t *testing.T) {
	env := NewTestEnv()

	// Wait with a non-zero timeout but no matching signal ever arrives.
	// In the test env, AdvanceTime will cause the waiter to time out.
	result := make(chan durable.SignalResult)
	go func() {
		result <- env.H().AwaitSignals([]string{"never"}, 100*time.Millisecond)
	}()

	time.Sleep(5 * time.Millisecond)

	// Advance time past the deadline.
	env.AdvanceTime(200 * time.Millisecond)

	sr := <-result
	if !sr.TimedOut {
		t.Fatal("expected timeout from AwaitSignals")
	}
	if sr.Err != nil {
		t.Fatalf("unexpected error: %v", sr.Err)
	}
}

// ---------------------------------------------------------------------------
// Additional: CallRecord captures errors
// ---------------------------------------------------------------------------

func TestCallRecordCapturesError(t *testing.T) {
	env := NewTestEnv()
	env.OnCall("svc", "op", nil).Return("", fmt.Errorf("service failure"))

	_, err := env.H().DurableCall("svc", "op", "req")
	if err == nil {
		t.Fatal("expected error from stub")
	}

	history := env.CallHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 call, got %d", len(history))
	}
	if history[0].Err == nil {
		t.Fatal("expected non-nil Err in CallRecord")
	}
	if history[0].Err.Error() != "service failure" {
		t.Fatalf("expected error %q, got %q", "service failure", history[0].Err.Error())
	}
}

// ---------------------------------------------------------------------------
// Additional: DurableDefer integration
// ---------------------------------------------------------------------------

func TestDurableDefer(t *testing.T) {
	env := NewTestEnv()
	id, err := env.H().DurableDefer("cleanup task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty defer ID")
	}
}
