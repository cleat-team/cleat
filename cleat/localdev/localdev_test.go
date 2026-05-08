package localdev

import (
	"io"
	"testing"
)

func TestNewLocalRunner_CreatesNonNil(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	if r == nil {
		t.Fatal("NewLocalRunner returned nil")
	}
}

func TestNewLocalRunner_HEqualsNonNil(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	h := r.H()
	if h == nil {
		t.Fatal("H() returned nil")
	}
}

func TestNewLocalRunner_EventsStartEmpty(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	events := r.Events()
	if len(events) != 0 {
		t.Errorf("expected empty events, got %d", len(events))
	}
}

func TestNewLocalRunner_ContinueAsNewInputInitiallyEmpty(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	input, ok := r.ContinueAsNewInput()
	if ok {
		t.Errorf("expected ok=false initially, got ok=%v with input=%q", ok, input)
	}
	if input != "" {
		t.Errorf("expected empty input, got %q", input)
	}
}

func TestNewLocalRunner_WithWorkflowID(t *testing.T) {
	r := NewLocalRunner(
		WithLogWriter(io.Discard),
		WithWorkflowID("test-wf-123"),
	)
	if r == nil {
		t.Fatal("NewLocalRunner returned nil")
	}
	// WorkflowID isn't exported on LocalRunner, but we can verify
	// that the runner is functional.
	h := r.H()
	if h == nil {
		t.Fatal("H() returned nil")
	}
}

func TestNewLocalRunner_AcquireConcurrencyKey(t *testing.T) {
	r := NewLocalRunner(
		WithLogWriter(io.Discard),
		WithWorkflowID("wf-1"),
	)
	acquired, err := r.AcquireConcurrencyKey("key-1", "wf-1", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire concurrency key")
	}

	// Second acquisition by same workflow should succeed.
	reAcquired, err := r.AcquireConcurrencyKey("key-1", "wf-1", 0)
	if err != nil {
		t.Fatalf("re-acquire returned error: %v", err)
	}
	if !reAcquired {
		t.Fatal("expected re-acquire by same workflow to succeed")
	}

	// Different workflow should fail.
	blocked, err := r.AcquireConcurrencyKey("key-1", "wf-2", 0)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey by different workflow returned error: %v", err)
	}
	if blocked {
		t.Fatal("expected different workflow to be blocked")
	}
}

func TestNewLocalRunner_ReleaseConcurrencyKeys(t *testing.T) {
	r := NewLocalRunner(
		WithLogWriter(io.Discard),
		WithWorkflowID("wf-release"),
	)

	r.AcquireConcurrencyKey("key-a", "wf-release", 0)
	r.AcquireConcurrencyKey("key-b", "wf-release", 0)

	r.ReleaseConcurrencyKeys("wf-release")

	// Another workflow should now be able to acquire.
	acquired, err := r.AcquireConcurrencyKey("key-a", "wf-other", 0)
	if err != nil {
		t.Fatalf("acquire after release returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected to acquire key after release")
	}
}

// ---------------------------------------------------------------------------
// sideEffect — returns computed result as-is
// ---------------------------------------------------------------------------

func TestSideEffect_ReturnsComputedResult(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	h := r.H()
	result, err := h.SideEffect(func() (string, error) {
		return "computed-value", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "computed-value" {
		t.Errorf("expected %q, got %q", "computed-value", result)
	}
}

// ---------------------------------------------------------------------------
// joinStrings — multiple elements
// ---------------------------------------------------------------------------

func TestJoinStrings_Empty(t *testing.T) {
	result := joinStrings(nil)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
	result = joinStrings([]string{})
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestJoinStrings_SingleElement(t *testing.T) {
	result := joinStrings([]string{"only"})
	if result != "only" {
		t.Errorf("expected 'only', got %q", result)
	}
}

func TestJoinStrings_MultipleElements(t *testing.T) {
	result := joinStrings([]string{"a", "b", "c"})
	if result != "a,b,c" {
		t.Errorf("expected 'a,b,c', got %q", result)
	}
}

func TestJoinStrings_TwoElements(t *testing.T) {
	result := joinStrings([]string{"x", "y"})
	if result != "x,y" {
		t.Errorf("expected 'x,y', got %q", result)
	}
}

// ---------------------------------------------------------------------------
// pow — edge cases for the loop body
// ---------------------------------------------------------------------------

func TestPow_ZeroExponent(t *testing.T) {
	result := pow(5.0, 0)
	if result != 1.0 {
		t.Errorf("expected 1.0, got %f", result)
	}
}

func TestPow_PositiveExponent(t *testing.T) {
	result := pow(2.0, 3)
	if result != 8.0 {
		t.Errorf("expected 8.0, got %f", result)
	}
}

func TestPow_ExponentOne(t *testing.T) {
	result := pow(10.0, 1)
	if result != 10.0 {
		t.Errorf("expected 10.0, got %f", result)
	}
}

func TestPow_BaseZero(t *testing.T) {
	result := pow(0.0, 5)
	if result != 0.0 {
		t.Errorf("expected 0.0, got %f", result)
	}
}

// ---------------------------------------------------------------------------
// matchesAny — coverage for both paths
// ---------------------------------------------------------------------------

func TestMatchesAny_Match(t *testing.T) {
	if !matchesAny("foo", []string{"bar", "foo", "baz"}) {
		t.Error("expected match for 'foo'")
	}
}

func TestMatchesAny_NoMatch(t *testing.T) {
	if matchesAny("foo", []string{"bar", "baz"}) {
		t.Error("expected no match for 'foo'")
	}
}

func TestMatchesAny_EmptyList(t *testing.T) {
	if matchesAny("foo", []string{}) {
		t.Error("expected no match for empty list")
	}
}

func TestMatchesAny_NilList(t *testing.T) {
	if matchesAny("foo", nil) {
		t.Error("expected no match for nil list")
	}
}

// ---------------------------------------------------------------------------
// truncate — coverage for both paths
// ---------------------------------------------------------------------------

func TestTruncate_ShorterThanMax(t *testing.T) {
	result := truncate("short", 10)
	if result != "short" {
		t.Errorf("expected 'short', got %q", result)
	}
}

func TestTruncate_LongerThanMax(t *testing.T) {
	result := truncate("this is a long string", 10)
	if result != "this is a ..." {
		t.Errorf("expected 'this is a ...', got %q", result)
	}
}

func TestTruncate_ExactlyMax(t *testing.T) {
	// When exactly at max, it returns as-is.
	result := truncate("exact", 5)
	if result != "exact" {
		t.Errorf("expected 'exact', got %q", result)
	}
}

// ---------------------------------------------------------------------------
// SendSignal — direct and buffered paths
// ---------------------------------------------------------------------------

func TestSendSignal_DeliversToChannel(t *testing.T) {
	signalCh := make(chan Signal, 1)
	r := NewLocalRunner(WithSignalChannel(signalCh))
	go r.SendSignal("test-sig", `{"key":"val"}`)
	sig := <-signalCh
	if sig.Name != "test-sig" {
		t.Errorf("expected Name 'test-sig', got %q", sig.Name)
	}
	if sig.Payload != `{"key":"val"}` {
		t.Errorf("expected Payload, got %q", sig.Payload)
	}
}

func TestSendSignal_BufferedWhenChannelFull(t *testing.T) {
	signalCh := make(chan Signal) // unbuffered, no receiver
	r := NewLocalRunner(WithSignalChannel(signalCh))
	// This should buffer the signal since no one is receiving.
	r.SendSignal("buffered-sig", `{"data":"buffered"}`)

	// Verify it was stored in pendingSigs.
	r.mu.Lock()
	pending := len(r.pendingSigs)
	r.mu.Unlock()
	if pending != 1 {
		t.Errorf("expected 1 pending signal, got %d", pending)
	}
}

func TestSendSignal_BufferedMultiple(t *testing.T) {
	signalCh := make(chan Signal) // unbuffered, no receiver
	r := NewLocalRunner(WithSignalChannel(signalCh))

	r.SendSignal("sig1", "p1")
	r.SendSignal("sig2", "p2")

	r.mu.Lock()
	pending := len(r.pendingSigs)
	r.mu.Unlock()
	if pending != 2 {
		t.Errorf("expected 2 pending signals, got %d", pending)
	}
}

// ---------------------------------------------------------------------------
// durableAwaitSignals — additional path coverage
// ---------------------------------------------------------------------------

func TestDurableAwaitSignals_ReturnsFirstMatchFromPending(t *testing.T) {
	signalCh := make(chan Signal) // unbuffered
	r := NewLocalRunner(WithSignalChannel(signalCh), WithLogWriter(io.Discard))
	// Use SendSignal to queue a signal into the channel.
	go func() {
		signalCh <- Signal{Name: "evt", Payload: `{"x":1}`}
	}()

	name, payload, timedOut, err := r.durableAwaitSignals([]string{"evt"}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("expected signal, not timeout")
	}
	if name != "evt" {
		t.Errorf("expected 'evt', got %q", name)
	}
	if payload != `{"x":1}` {
		t.Errorf("expected payload, got %q", payload)
	}
}

func TestPollSignal_NonMatchingSignalIsRequeued(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	r.signalCh <- Signal{Name: "other", Payload: "val"}

	payload, ok, err := r.pollSignal("target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected no match for non-matching signal")
	}
	if payload != "" {
		t.Errorf("expected empty payload, got %q", payload)
	}

	r.mu.Lock()
	pending := len(r.pendingSigs)
	r.mu.Unlock()
	if pending != 1 {
		t.Errorf("expected 1 pending signal, got %d", pending)
	}
}

func TestPollSignal_EmptyChannel(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))

	payload, ok, err := r.pollSignal("anything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected no match for empty channel")
	}
	if payload != "" {
		t.Errorf("expected empty payload, got %q", payload)
	}
}

// ---------------------------------------------------------------------------
// durableAwaitSignals — poll mode (timeout=0)
// ---------------------------------------------------------------------------

func TestDurableAwaitSignals_PollMode_Matching(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	r.signalCh <- Signal{Name: "evt", Payload: `{"x":1}`}

	name, payload, timedOut, err := r.durableAwaitSignals([]string{"evt"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("expected signal, not timeout")
	}
	if name != "evt" {
		t.Errorf("expected 'evt', got %q", name)
	}
	if payload != `{"x":1}` {
		t.Errorf("expected payload, got %q", payload)
	}
}

func TestDurableAwaitSignals_PollMode_NegativeTimeout(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	r.signalCh <- Signal{Name: "evt", Payload: `{"x":1}`}

	// Negative timeout also triggers poll mode.
	name, payload, timedOut, err := r.durableAwaitSignals([]string{"evt"}, -1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Fatal("expected signal, not timeout")
	}
	if name != "evt" {
		t.Errorf("expected 'evt', got %q", name)
	}
	if payload != `{"x":1}` {
		t.Errorf("expected payload, got %q", payload)
	}
}

func TestDurableAwaitSignals_PollMode_EmptyChannel(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))

	name, payload, timedOut, err := r.durableAwaitSignals([]string{"evt"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !timedOut {
		t.Fatal("expected timeout, got signal")
	}
	if name != "" {
		t.Errorf("expected empty name, got %q", name)
	}
	if payload != "" {
		t.Errorf("expected empty payload, got %q", payload)
	}
}

func TestDurableAwaitSignals_PollMode_NonMatchingDiscarded(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	r.signalCh <- Signal{Name: "wrong", Payload: "x"}

	// Poll with a different name — "wrong" should be discarded.
	name, payload, timedOut, err := r.durableAwaitSignals([]string{"target"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !timedOut {
		t.Fatal("expected timeout, got signal")
	}
	if name != "" {
		t.Errorf("expected empty name, got %q", name)
	}
	if payload != "" {
		t.Errorf("expected empty payload, got %q", payload)
	}
}

// ---------------------------------------------------------------------------
// durableAwaitSignals — non-matching signal during timer wait
// ---------------------------------------------------------------------------

func TestDurableAwaitSignals_TimerWaitDiscardNonMatching(t *testing.T) {
	r := NewLocalRunner(WithLogWriter(io.Discard))
	// Put a non-matching signal in the channel.
	r.signalCh <- Signal{Name: "wrong", Payload: "x"}
	// Signal channel is buffered (cap=100) so the non-matching signal
	// will be read by the timer loop first, then discarded, and eventually
	// the timer fires and we get a timeout.
	name, payload, timedOut, err := r.durableAwaitSignals([]string{"target"}, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !timedOut {
		t.Fatal("expected timeout after discarding non-matching signal")
	}
	if name != "" {
		t.Errorf("expected empty name, got %q", name)
	}
	if payload != "" {
		t.Errorf("expected empty payload, got %q", payload)
	}
}
