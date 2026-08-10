package cleattest

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat"
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
	if env.H().HostCallsImpl == nil {
		t.Fatal("H() returned nil HostCallsImpl")
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
	// Asserted rather than dropped: ineffassign found `name` being discarded
	// here once the cleat module started being linted. A timeout that also
	// returned a signal name would be a real defect, and nothing said so.
	if name != "" || payload != "" {
		t.Fatalf("timeout returned a signal: name=%q payload=%q", name, payload)
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

	resp, err := env.H().DurableCallWithOptions(cleat.CallOptions{}, "svc", "op", "req")
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
	result := make(chan cleat.SignalResult)
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

// ---------------------------------------------------------------------------
// Additional: Signal delivery during DurableSleep
// ---------------------------------------------------------------------------

func TestSignalDuringDurableSleep(t *testing.T) {
	env := NewTestEnv()

	// Goroutine 1: sleep for 5s
	slept := make(chan struct{})
	go func() {
		env.H().DurableSleep(5 * time.Second)
		close(slept)
	}()

	// Goroutine 2: deliver a signal after a short real-time delay
	signalDone := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		env.Signal("wake_up", "payload")
		close(signalDone)
	}()

	// Wait for the signal to be delivered.
	<-signalDone

	// Give the sleep goroutine time to register.
	time.Sleep(5 * time.Millisecond)

	// Advance time past the sleep deadline.
	env.AdvanceTime(6 * time.Second)

	// Wait for the sleep to finish.
	<-slept

	// The signal should now be available via PollSignal.
	payload, found, err := env.H().PollSignal("wake_up")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected PollSignal to find signal after sleep")
	}
	if payload != "payload" {
		t.Fatalf("expected payload %q, got %q", "payload", payload)
	}
}

// ---------------------------------------------------------------------------
// Additional: Concurrent signal delivery
// ---------------------------------------------------------------------------

func TestConcurrentSignalDelivery(t *testing.T) {
	t.Parallel()

	env := NewTestEnv()

	var wg sync.WaitGroup
	numSignals := 10

	// Deliver 10 signals from 10 goroutines concurrently to the same workflow.
	for i := 0; i < numSignals; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			env.Signal(fmt.Sprintf("sig_%d", idx), fmt.Sprintf("payload_%d", idx))
		}(i)
	}
	wg.Wait()

	// Collect all signal names for polling.
	allNames := make([]string, numSignals)
	for i := 0; i < numSignals; i++ {
		allNames[i] = fmt.Sprintf("sig_%d", i)
	}

	// Consume all signals via DurableAwaitSignals using poll mode (0 timeout).
	// Each call consumes exactly one signal. Verify exactly-once semantics.
	received := make(map[string]int)
	for i := 0; i < numSignals; i++ {
		name, payload, timedOut, err := env.H().DurableAwaitSignals(allNames, 0)
		if err != nil {
			t.Fatalf("unexpected error at signal %d: %v", i, err)
		}
		if timedOut {
			t.Fatalf("unexpected timeout at signal %d: consumed %d/%d signals", i, i, numSignals)
		}
		received[name]++

		// Verify payload matches the signal name.
		var sigIdx int
		if _, parseErr := fmt.Sscanf(name, "sig_%d", &sigIdx); parseErr == nil {
			expectedPayload := fmt.Sprintf("payload_%d", sigIdx)
			if payload != expectedPayload {
				t.Errorf("signal %s: expected payload %q, got %q", name, expectedPayload, payload)
			}
		}
	}

	// Verify exactly once: every signal was received exactly one time.
	for i := 0; i < numSignals; i++ {
		name := fmt.Sprintf("sig_%d", i)
		if received[name] != 1 {
			t.Errorf("signal %s received %d times (expected exactly 1)", name, received[name])
		}
	}

	// Verify no extra signals remain in the queue.
	_, _, timedOut, err := env.H().DurableAwaitSignals(allNames, 0)
	if err != nil {
		t.Fatalf("unexpected error checking for extras: %v", err)
	}
	if !timedOut {
		t.Error("expected timeout after consuming all signals — some signals may be duplicated")
	}
}

// ---------------------------------------------------------------------------
// Additional: Multiple rapid signals with FIFO ordering
// ---------------------------------------------------------------------------

func TestMultipleRapidSignals(t *testing.T) {
	env := NewTestEnv()

	// Send 20 signals with different names and payloads.
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("sig_%d", i)
		payload := fmt.Sprintf("payload_%d", i)
		env.Signal(name, payload)
	}

	// Verify all 20 are retrievable via PollSignal.
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("sig_%d", i)
		payload, found, err := env.H().PollSignal(name)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", name, err)
		}
		if !found {
			t.Fatalf("expected signal %s to be found", name)
		}
		expected := fmt.Sprintf("payload_%d", i)
		if payload != expected {
			t.Fatalf("expected payload %q for %s, got %q", expected, name, payload)
		}
	}

	// Verify FIFO ordering for signals with the same timestamps.
	env2 := NewTestEnv()
	env2.Signal("first", "payload_first")
	env2.Signal("second", "payload_second")
	env2.Signal("third", "payload_third")

	// All three signals share the current simulated time.
	// AwaitSignals should return them in insertion order.
	names := []string{"first", "second", "third"}

	name, payload, timedOut, err := env2.H().DurableAwaitSignals(names, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout for first signal")
	}
	if name != "first" {
		t.Fatalf("expected signal %q, got %q", "first", name)
	}
	if payload != "payload_first" {
		t.Fatalf("expected payload %q, got %q", "payload_first", payload)
	}

	name, payload, timedOut, err = env2.H().DurableAwaitSignals([]string{"second", "third"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout for second signal")
	}
	if name != "second" {
		t.Fatalf("expected signal %q, got %q", "second", name)
	}
	if payload != "payload_second" {
		t.Fatalf("expected payload %q, got %q", "payload_second", payload)
	}

	name, payload, timedOut, err = env2.H().DurableAwaitSignals([]string{"third"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("unexpected timeout for third signal")
	}
	if name != "third" {
		t.Fatalf("expected signal %q, got %q", "third", name)
	}
	if payload != "payload_third" {
		t.Fatalf("expected payload %q, got %q", "payload_third", payload)
	}
}

// ---------------------------------------------------------------------------
// Additional: Zero duration AwaitSignals
// ---------------------------------------------------------------------------

func TestZeroDurationAwaitSignals(t *testing.T) {
	env := NewTestEnv()

	// AwaitSignals with zero timeout should return an error.
	result := env.H().AwaitSignals([]string{"test"}, 0)

	if result.Err == nil {
		t.Fatal("expected error from AwaitSignals with zero timeout (use PollSignals instead)")
	}
	if !result.TimedOut {
		t.Fatal("expected TimedOut from AwaitSignals with zero timeout")
	}
}

// ---------------------------------------------------------------------------
// Concurrency Key tests
// ---------------------------------------------------------------------------

func TestAcquireConcurrencyKeyAcquireRelease(t *testing.T) {
	env := NewTestEnv()

	// First acquire should succeed.
	acquired, err := env.AcquireConcurrencyKey("my-key", "wf-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("expected first acquire to succeed")
	}

	// Second acquire with same key, different workflow should fail.
	acquired, err = env.AcquireConcurrencyKey("my-key", "wf-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acquired {
		t.Fatal("expected second acquire to fail")
	}

	// Release by first workflow.
	env.ReleaseConcurrencyKeys("wf-1")

	// Now re-acquire should succeed.
	acquired, err = env.AcquireConcurrencyKey("my-key", "wf-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("expected re-acquire after release to succeed")
	}
}

func TestAcquireConcurrencyKeySameWorkflow(t *testing.T) {
	env := NewTestEnv()

	acquired, err := env.AcquireConcurrencyKey("my-key", "wf-1")
	if err != nil || !acquired {
		t.Fatalf("first acquire: acquired=%v err=%v", acquired, err)
	}

	// Same workflow re-acquiring should succeed (idempotent).
	acquired, err = env.AcquireConcurrencyKey("my-key", "wf-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("same workflow should be able to re-acquire its key")
	}
}

func TestAcquireConcurrencyKeyFnOverride(t *testing.T) {
	env := NewTestEnv()
	env.AcquireConcurrencyKeyFn = func(key, workflowID string) (bool, error) {
		return key == "allowed-key", nil
	}

	acquired, err := env.AcquireConcurrencyKey("allowed-key", "wf-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire with allowed key to succeed")
	}

	acquired, err = env.AcquireConcurrencyKey("denied-key", "wf-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acquired {
		t.Fatal("expected acquire with denied key to fail")
	}
}

func TestReleaseConcurrencyKeysFnOverride(t *testing.T) {
	env := NewTestEnv()
	releaseCalled := false
	env.ReleaseConcurrencyKeysFn = func(workflowID string) {
		releaseCalled = true
	}

	env.ReleaseConcurrencyKeys("wf-1")
	if !releaseCalled {
		t.Fatal("expected ReleaseConcurrencyKeysFn to be called")
	}
}

func TestAcquireConcurrencyKeyMultipleKeys(t *testing.T) {
	env := NewTestEnv()

	// Acquire two different keys for the same workflow.
	acquired, err := env.AcquireConcurrencyKey("key-1", "wf-1")
	if err != nil || !acquired {
		t.Fatalf("acquire key-1: acquired=%v err=%v", acquired, err)
	}
	acquired, err = env.AcquireConcurrencyKey("key-2", "wf-1")
	if err != nil || !acquired {
		t.Fatalf("acquire key-2: acquired=%v err=%v", acquired, err)
	}

	// A different workflow should not be able to acquire either key.
	acquired, err = env.AcquireConcurrencyKey("key-1", "wf-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acquired {
		t.Fatal("wf-2 should not be able to acquire key-1")
	}

	// Release all keys for wf-1.
	env.ReleaseConcurrencyKeys("wf-1")

	// Now wf-2 can acquire both keys.
	acquired, err = env.AcquireConcurrencyKey("key-1", "wf-2")
	if err != nil || !acquired {
		t.Fatalf("re-acquire key-1: acquired=%v err=%v", acquired, err)
	}
	acquired, err = env.AcquireConcurrencyKey("key-2", "wf-2")
	if err != nil || !acquired {
		t.Fatalf("re-acquire key-2: acquired=%v err=%v", acquired, err)
	}
}

// ---------------------------------------------------------------------------
// Retry simulation tests
// ---------------------------------------------------------------------------

func TestWithRetrySimulation(t *testing.T) {
	env := NewTestEnv(WithRetrySimulation(2))
	env.OnCall("svc", "op", nil).Return("success", nil)

	// First call should fail (attempt 1/2).
	_, err := env.H().DurableCall("svc", "op", "req")
	if err == nil {
		t.Fatal("expected retry simulation failure on first call")
	}

	// Second call should also fail (attempt 2/2).
	_, err = env.H().DurableCall("svc", "op", "req")
	if err == nil {
		t.Fatal("expected retry simulation failure on second call")
	}

	// Third call should succeed.
	resp, err := env.H().DurableCall("svc", "op", "req")
	if err != nil {
		t.Fatalf("unexpected error on third call: %v", err)
	}
	if resp != "success" {
		t.Fatalf("expected %q, got %q", "success", resp)
	}
}

func TestWithRetrySimulationZero(t *testing.T) {
	// Zero retry simulation count = no simulation.
	env := NewTestEnv(WithRetrySimulation(0))
	env.OnCall("svc", "op", nil).Return("success", nil)

	resp, err := env.H().DurableCall("svc", "op", "req")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "success" {
		t.Fatalf("expected %q, got %q", "success", resp)
	}
}

func TestWithRetrySimulationPerServiceOp(t *testing.T) {
	// Retry simulation should be per-service, per-operation.
	env := NewTestEnv(WithRetrySimulation(1))
	env.OnCall("svc1", "op1", nil).Return("ok1", nil)
	env.OnCall("svc2", "op2", nil).Return("ok2", nil)

	// svc1.op1 should fail once then succeed.
	_, err := env.H().DurableCall("svc1", "op1", "req1")
	if err == nil {
		t.Fatal("expected retry simulation failure for svc1.op1")
	}
	resp, err := env.H().DurableCall("svc1", "op1", "req1")
	if err != nil {
		t.Fatalf("unexpected error for svc1.op1: %v", err)
	}
	if resp != "ok1" {
		t.Fatalf("expected %q, got %q", "ok1", resp)
	}

	// svc2.op2 should fail once then succeed.
	_, err = env.H().DurableCall("svc2", "op2", "req2")
	if err == nil {
		t.Fatal("expected retry simulation failure for svc2.op2")
	}
	resp, err = env.H().DurableCall("svc2", "op2", "req2")
	if err != nil {
		t.Fatalf("unexpected error for svc2.op2: %v", err)
	}
	if resp != "ok2" {
		t.Fatalf("expected %q, got %q", "ok2", resp)
	}
}

func TestAcquireConcurrencyKeyReset(t *testing.T) {
	env := NewTestEnv()

	env.AcquireConcurrencyKey("key-1", "wf-1")
	env.AcquireConcurrencyKey("key-2", "wf-1")

	env.Reset()

	// After reset, should be able to acquire keys again.
	acquired, err := env.AcquireConcurrencyKey("key-1", "wf-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("expected acquire after reset to succeed")
	}
}

// ---------------------------------------------------------------------------
// AdvanceTimeAndDrain tests
// ---------------------------------------------------------------------------

func TestAdvanceTimeAndDrainSleeps(t *testing.T) {
	env := NewTestEnv()

	slept := make(chan struct{})
	go func() {
		env.H().DurableSleep(1 * time.Second)
		close(slept)
	}()

	// Give goroutine time to enter sleep.
	time.Sleep(5 * time.Millisecond)

	// AdvanceTimeAndDrain should block until the sleeper is drained.
	env.AdvanceTimeAndDrain(2 * time.Second)

	// The sleeper should be done without any additional sleep.
	select {
	case <-slept:
		// success
	case <-time.After(50 * time.Millisecond):
		t.Fatal("DurableSleep did not complete after AdvanceTimeAndDrain")
	}
}

func TestAdvanceTimeAndDrainPreScheduledSignal(t *testing.T) {
	env := NewTestEnv()

	done := make(chan struct{})
	go func() {
		env.H().DurableSleep(1 * time.Second)
		sr := env.H().AwaitSignals([]string{"wake"}, 500*time.Millisecond)
		if sr.TimedOut {
			t.Error("expected signal, got timeout")
		}
		close(done)
	}()

	// Pre-schedule a signal to arrive at the same time the goroutine wakes.
	env.AfterSignal(1*time.Second, "wake", `{"msg":"hello"}`)

	time.Sleep(5 * time.Millisecond)

	// Advance past the sleep; the pre-scheduled signal should be delivered.
	env.AdvanceTimeAndDrain(2 * time.Second)

	select {
	case <-done:
		// success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("goroutine did not finish after AdvanceTimeAndDrain")
	}
}

func TestAdvanceTimeAndDrainMultipleSleeps(t *testing.T) {
	env := NewTestEnv()

	slept1 := make(chan struct{})
	slept2 := make(chan struct{})

	go func() {
		env.H().DurableSleep(1 * time.Second)
		close(slept1)
		env.H().DurableSleep(1 * time.Second)
		close(slept2)
	}()

	// Wait for the sleeper to be registered rather than sleeping a fixed 5ms
	// and hoping. AdvanceTimeAndDrain returns as soon as it sees zero pending
	// sleepers, so advancing before the goroutine has registered drains
	// nothing -- while still moving the clock forward. The sleep that
	// registers afterwards then wants a deadline past the new now, and no
	// further advance is coming, so it hangs until the test's timeout.
	waitForSleeper := func(what string) {
		t.Helper()
		for i := 0; i < 500; i++ {
			env.mu.Lock()
			n := len(env.sleepRecs)
			env.mu.Unlock()
			if n > 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s to be registered", what)
	}

	// First advance: should drain first sleep.
	waitForSleeper("first DurableSleep")
	env.AdvanceTimeAndDrain(1 * time.Second)
	select {
	case <-slept1:
		// success
	case <-time.After(50 * time.Millisecond):
		t.Fatal("first DurableSleep did not complete")
	}

	// Second advance: should drain second sleep. close(slept1) happens
	// *before* the goroutine enters the second DurableSleep, so reaching here
	// says nothing about whether that sleep is registered yet -- this is the
	// window the original test left unguarded, and it is why the job failed
	// intermittently under CI load while passing locally.
	waitForSleeper("second DurableSleep")
	env.AdvanceTimeAndDrain(1 * time.Second)
	select {
	case <-slept2:
		// success
	case <-time.After(50 * time.Millisecond):
		t.Fatal("second DurableSleep did not complete")
	}
}

// ---------------------------------------------------------------------------
// Signal synchronous delivery test
// ---------------------------------------------------------------------------

func TestSignalSynchronousDelivery(t *testing.T) {
	env := NewTestEnv()

	received := make(chan struct{})
	go func() {
		sr := env.H().AwaitSignals([]string{"greeting"}, 1*time.Second)
		if sr.TimedOut {
			t.Error("expected signal, got timeout")
		}
		close(received)
	}()

	// Let goroutine reach AwaitSignals.
	time.Sleep(5 * time.Millisecond)

	// Signal should be delivered synchronously via Gosched.
	env.Signal("greeting", `{"msg":"hello"}`)

	// Signal delivery should complete quickly without additional sleep.
	select {
	case <-received:
		// success
	case <-time.After(50 * time.Millisecond):
		t.Fatal("signal was not received within expected window")
	}
}
