package engine

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

// cleat#4105. A guest hands the host time quantities as raw 64-bit integers, and the host multiplies them by time.Millisecond. That product
// wraps: i64::MAX ms is -1 ms (measured, 2026-11-19, on `time.Duration(math.MaxInt64) * time.Millisecond`), 1e13 ms is about -270 years.
// What each site then does with a negative duration is different and none of it is what the guest asked for. One site, the heartbeat
// ticker, panics outright: time.NewTicker refuses a non-positive interval.
//
// Every test here pairs the bad value with a CONTROL: an ordinary value at the same site that must behave normally, so that a test that
// went red because the harness could not reach the site cannot pass for the wrong reason.

const farFuture = 365 * 24 * time.Hour // anything this far ahead was not produced by a wrapped product

// badMillis are the values that wrap Duration arithmetic: past 292 years (9.22e12 ms), the largest int64, and the SDKs' own
// "wait indefinitely" sentinel, which is that same number (python-sdk INFINITE_TIMEOUT_MS = 9223372036854775807).
var badMillis = []int64{9_300_000_000_000, 1e13, math.MaxInt64 / 2, math.MaxInt64}

// guardedCaller is a ServiceCaller that is safe to read from the host's own goroutines.
type guardedCaller struct {
	mu    sync.Mutex
	calls []string
}

func (g *guardedCaller) Call(_ context.Context, service, operation, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, service+"/"+operation)
	return `{}`, nil
}

func (g *guardedCaller) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

func durationSession(opts ...EngineOption) (*execSession, *guardedCaller) {
	c := &guardedCaller{}
	opts = append([]EngineOption{WithWorkflowID("wf-4105")}, opts...)
	return &execSession{
		engine: NewEngine(nil, c, opts...), workflowID: "wf-4105", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{},
	}, c
}

func hostCtx() context.Context {
	return contextWithRawMemBuf(context.Background(), make([]byte, 65536))
}

// survives runs fn and reports a panic instead of letting it take the test binary down, which is what it does to a worker.
func survives(t *testing.T, what string, fn func()) (ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s PANICKED the host: %v", what, r)
			ok = false
		}
	}()
	fn()
	return true
}

// ---- the heartbeat ticker: the panic -----------------------------------------------------------------------------------------------

func TestHeartbeatIntervalThatIsNotPositiveIsRefusedNotPanicked(t *testing.T) {
	for _, ms := range []int64{0, -1, math.MinInt64} {
		s, c := durationSession()
		var packed int64
		if !survives(t, fmt.Sprintf("a heartbeat interval of %d ms", ms), func() {
			packed = s.DurableCallWithHeartbeat(hostCtx(), nil, "svc", "op", "{}", ms, 0, 4096)
		}) {
			continue
		}
		if packed&0xFF == 0 {
			t.Errorf("interval %d ms: the call was answered as a success (%#x), want a refusal the guest can read", ms, packed)
		}
		if n := c.count(); n != 0 {
			t.Errorf("interval %d ms: the service was called %d time(s) by a refused call", ms, n)
		}
	}
}

func TestHeartbeatIntervalPastWhatDurationCanHoldRunsTheCallInsteadOfPanicking(t *testing.T) {
	for _, ms := range badMillis {
		s, c := durationSession()
		var packed int64
		if !survives(t, fmt.Sprintf("a heartbeat interval of %d ms", ms), func() {
			packed = s.DurableCallWithHeartbeat(hostCtx(), nil, "svc", "op", "{}", ms, 0, 4096)
		}) {
			continue
		}
		if packed&0xFF != 0 || c.count() != 1 {
			t.Errorf("interval %d ms: packed=%#x calls=%d, want the call to run once and succeed (a heartbeat that never ticks is a valid request)", ms, packed, c.count())
		}
	}
}

// CONTROL: an ordinary interval must still run the call.
func TestHeartbeatOrdinaryIntervalRunsTheCall(t *testing.T) {
	s, c := durationSession()
	if packed := s.DurableCallWithHeartbeat(hostCtx(), nil, "svc", "op", "{}", 50, 0, 4096); packed&0xFF != 0 || c.count() != 1 {
		t.Fatalf("packed=%#x calls=%d, want one successful call", packed, c.count())
	}
}

// ---- scheduled invoke: a wrapped delay is delivered at once -----------------------------------------------------------------------

func TestScheduleInvokeWithADelayPastWhatDurationCanHoldIsNotDeliveredAtOnce(t *testing.T) {
	for _, ms := range badMillis {
		s, c := durationSession()
		s.DurableScheduleInvoke(hostCtx(), nil, "svc", "later", "{}", ms)
		time.Sleep(150 * time.Millisecond)
		if n := c.count(); n != 0 {
			t.Errorf("delay %d ms: the request was delivered %d time(s) within 150 ms, want it held (the wrapped delay is negative)", ms, n)
		}
	}
}

// CONTROL: this harness does see a delivery when one is due, so "0 deliveries" above means held, not unobservable.
func TestScheduleInvokeOrdinaryDelayIsDelivered(t *testing.T) {
	s, c := durationSession()
	s.DurableScheduleInvoke(hostCtx(), nil, "svc", "later", "{}", 5)
	deadline := time.Now().Add(3 * time.Second)
	for c.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.count() != 1 {
		t.Fatalf("a 5 ms delay was not delivered within 3 s (calls=%d): the test cannot tell held from lost", c.count())
	}
}

// ---- waits: a wrapped timeout puts the wake-up in the past --------------------------------------------------------------------------

func suspendedUntil(t *testing.T, s *execSession, what string) time.Time {
	t.Helper()
	if s.suspendErr == nil {
		t.Fatalf("%s: the workflow was not suspended", what)
	}
	return s.suspendErr.Until
}

func TestAwaitSignalsWithAnEnormousTimeoutSuspendsUntilFarInTheFuture(t *testing.T) {
	for _, ms := range badMillis {
		s, _ := durationSession(WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
		s.DurableAwaitSignals(hostCtx(), nil, `["a"]`, ms, 0, 200, 256, 200)
		if until := suspendedUntil(t, s, fmt.Sprintf("await_signals(%d ms)", ms)); time.Until(until) < farFuture {
			t.Errorf("await_signals(%d ms) wakes at %v (%v from now), want it far in the future: a wrapped timeout is in the past", ms, until.Format(time.RFC3339), time.Until(until).Round(time.Second))
		}
	}
}

// CONTROL: an ordinary timeout wakes about when asked.
func TestAwaitSignalsOrdinaryTimeoutWakesWhenAsked(t *testing.T) {
	s, _ := durationSession(WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
	s.DurableAwaitSignals(hostCtx(), nil, `["a"]`, 60_000, 0, 200, 256, 200)
	until := suspendedUntil(t, s, "await_signals(60s)")
	if d := time.Until(until); d < 50*time.Second || d > 70*time.Second {
		t.Errorf("await_signals(60s) wakes in %v", d)
	}
}

// The recorded timeout is what a replay measures its deadline against, so a recorded enormous one must not wrap either.
func TestReplayedAwaitSignalsWithAnEnormousRecordedTimeoutStillSuspends(t *testing.T) {
	for _, ms := range badMillis {
		s := awaitReplaySession(t, &awaitProbeStore{deliveries: map[string]SignalDelivery{}}, []EventRecord{recordedAwait(ms)})
		// The call's own return value, not callAwait's: that helper reads the stop sentinel's bit 31 as a timed-out flag.
		if packed := s.DurableAwaitSignals(contextWithRawMemBuf(context.Background(), make([]byte, 512)), nil, `["a","b"]`, 60000, 0, 200, 256, 200); packed != callSuspendSentinel {
			t.Errorf("a replayed await with a recorded timeout of %d ms returned %#x, want the stop sentinel (a timeout that had not happened is what the wrapped sum reported)", ms, packed)
		}
		if s.suspendErr == nil {
			t.Errorf("a replayed await with a recorded timeout of %d ms did not suspend", ms)
		} else if time.Until(s.suspendErr.Until) < farFuture {
			t.Errorf("recorded timeout %d ms: wakes at %v, want far in the future", ms, s.suspendErr.Until.Format(time.RFC3339))
		}
	}
}

type pendingPromises struct{ PromiseStore }

func (pendingPromises) GetPromise(context.Context, string, string) (string, string, string, error) {
	return "pending", "", "", nil
}

func TestAwaitPromiseWithAnEnormousTimeoutSuspendsUntilFarInTheFuture(t *testing.T) {
	for _, ms := range badMillis {
		s, _ := durationSession(WithPromiseStore(pendingPromises{}))
		s.AwaitPromise(hostCtx(), nil, "p-1", ms, 0, 256)
		if until := suspendedUntil(t, s, fmt.Sprintf("await_promise(%d ms)", ms)); time.Until(until) < farFuture {
			t.Errorf("await_promise(%d ms) wakes at %v (%v from now), want it far in the future", ms, until.Format(time.RFC3339), time.Until(until).Round(time.Second))
		}
	}
}

func TestSleepForAnEnormousDurationSuspendsInsteadOfCompleting(t *testing.T) {
	for _, ms := range badMillis {
		s, _ := durationSession()
		packed := s.DurableSleep(hostCtx(), nil, ms)
		if s.suspendErr == nil || time.Until(s.suspendErr.Until) < farFuture {
			t.Errorf("sleep(%d ms): packed=%#x suspend=%v, want a suspension far in the future (nowMs+duration wraps int64)", ms, packed, s.suspendErr)
		}
	}
}

// CONTROL: an ordinary sleep suspends for about that long, and zero completes at once.
func TestSleepOrdinaryAndZeroDurations(t *testing.T) {
	s, _ := durationSession()
	s.DurableSleep(hostCtx(), nil, 60_000)
	if d := time.Until(suspendedUntil(t, s, "sleep(60s)")); d < 50*time.Second || d > 70*time.Second {
		t.Errorf("sleep(60s) wakes in %v", d)
	}
	z, _ := durationSession()
	z.DurableSleep(hostCtx(), nil, 0)
	if z.suspendErr != nil {
		t.Errorf("sleep(0) suspended until %v, want it to complete at once", z.suspendErr.Until)
	}
}

func TestWasiSleepForAnEnormousDurationIsALongWaitNotAnImmediateOne(t *testing.T) {
	for _, ms := range badMillis {
		s, _ := durationSession()
		if d := s.ServeWasiSleep(hostCtx(), ms); d < farFuture {
			t.Errorf("wasi sleep(%d ms) = %v, want a very long wait", ms, d)
		}
	}
}

// A CONTROL, and a measured non-finding: WaitForEvent computes `deadline := s.nowMs + timeoutMs`, which wraps for i64::MAX, and a fix was
// written for it. Reverting that fix left this test green, because the only use of the deadline is `remaining := deadline - s.nowMs`, and the
// subtraction wraps back to the original timeout. So this site needs no clamp; the test pins that it keeps waiting rather than timing out.
func TestWaitForEventWithAnEnormousTimeoutWaitsInsteadOfTimingOutAtOnce(t *testing.T) {
	for _, ms := range badMillis {
		counter := &eventClaimCounter{}
		pr := NewPluginRegistry()
		if err := pr.Register("webhook-ingest", "await_webhook", counter.call); err != nil {
			t.Fatal(err)
		}
		base := time.Now().UnixMilli()
		eng := NewEngine(nil, &mockCaller{}, WithPluginRegistry(pr), WithWorkflowID("wf-1"), WithClock(func() int64 { return base }),
			WithSignalStore(&awaitProbeStore{deliveries: map[string]SignalDelivery{}}))
		s := &execSession{engine: eng, workflowID: "wf-1", nowMs: base, deferrals: map[string]string{}, queryState: map[string]string{}}
		packed := s.WaitForEvent(hostCtx(), nil, "webhook-ingest", "await_webhook", "{}", `["__evt:payment"]`, ms, 0, 400)
		if packed != callSuspendSentinel || s.suspendErr == nil {
			t.Errorf("wait_for_event(%d ms): packed=%#x suspend=%v, want the wait to suspend rather than report a timeout", ms, packed, s.suspendErr)
		}
	}
}

// ---- retry policy: a wrapped interval makes any policy fit the budget -------------------------------------------------------------------

func TestRetryPolicyWithAnEnormousIntervalDoesNotFitTheHostBudget(t *testing.T) {
	for _, ms := range badMillis {
		if retryPolicyFitsBudget(3, ms, 200, 0, 60*time.Second) {
			t.Errorf("a 3-attempt policy with a first interval of %d ms fits a 60 s budget: the wrapped interval is negative, so the total never grows", ms)
		}
	}
	// CONTROL: the same shape with an ordinary interval fits, and an ordinary-but-long one does not.
	if !retryPolicyFitsBudget(3, 10, 200, 0, 60*time.Second) {
		t.Error("CONTROL: a 3-attempt policy at 10 ms does not fit a 60 s budget")
	}
	if retryPolicyFitsBudget(3, 40_000, 200, 0, 60*time.Second) {
		t.Error("CONTROL: a policy whose worst case is 40 s + 80 s fits a 60 s budget")
	}
}

// ---- lock TTL --------------------------------------------------------------------------------------------------------------------------

type ttlKeyStore struct {
	mu  sync.Mutex
	ttl time.Duration
}

func (k *ttlKeyStore) AcquireConcurrencyKey(_ context.Context, _, _ string, ttl time.Duration) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ttl = ttl
	return true, nil
}
func (k *ttlKeyStore) ReleaseConcurrencyKey(context.Context, string, string) (bool, error) {
	return true, nil
}

func TestAcquireLockWithAnEnormousTTLReachesTheStoreAsALongPositiveOne(t *testing.T) {
	for _, ms := range badMillis {
		st := &ttlKeyStore{}
		s, _ := durationSession(WithConcurrencyKeyStore(st))
		s.AcquireLock(hostCtx(), nil, "k", ms)
		if st.ttl < farFuture {
			t.Errorf("lock TTL %d ms reached the store as %v, want a very long positive TTL (a negative one is a lock that is already expired)", ms, st.ttl)
		}
	}
	// CONTROL: an ordinary TTL passes through unchanged.
	st := &ttlKeyStore{}
	s, _ := durationSession(WithConcurrencyKeyStore(st))
	s.AcquireLock(hostCtx(), nil, "k", 90_000)
	if st.ttl != 90*time.Second {
		t.Errorf("CONTROL: a 90 s TTL reached the store as %v", st.ttl)
	}
}

// A promise await that was recorded with an enormous timeout and is woken early measures its deadline against the RECORD. The wrapped sum
// was negative, so the wake was read as the deadline having passed and the await reported a timeout (cleat#4105).
func TestReplayedAwaitPromiseWithAnEnormousRecordedTimeoutKeepsWaiting(t *testing.T) {
	for _, ms := range badMillis {
		s, _ := durationSession(WithPromiseStore(pendingPromises{}))
		s.isReplay = true
		s.history = []EventRecord{{Step: 0, EventType: EventTypeAwaitPromise, PromiseID: "p-1", TimeoutMs: ms, TimestampMs: time.Now().UnixMilli()}}
		packed := s.AwaitPromise(hostCtx(), nil, "p-1", 60000, 0, 256)
		if packed != callSuspendSentinel || s.suspendErr == nil || time.Until(s.suspendErr.Until) < farFuture {
			t.Errorf("recorded await_promise timeout %d ms: packed=%#x suspend=%v, want it to keep waiting until far in the future, not report a timeout", ms, packed, s.suspendErr)
		}
	}
}

// boundedBackoffMs replaced `initialIntervalMs * int64(math.Pow(coefficient, attempt-1))`. The first rows are what that expression gave for a
// set of ORDINARY inputs, so the replacement changes nothing there (a grid of eight, not a proof about every input: cleat-review's
// 222-input grid found 73 that differ, in exactly the two classes below). Then what it gave where it overflowed or converted an
// out-of-range float (implementation-defined in Go: amd64 yields MinInt64, arm64 saturates), and the two classes where the new value
// DIFFERS ON PURPOSE even though the old one was defined:
//
//   - an old product above the 100-year clamp but inside int64 (it was a wait of millennia; it is now the clamp);
//   - a negative interval times a negative factor (the old product was positive and large; a negative factor is no backoff at all, so
//     the new value is 0, which the loop floors to 1 ms).
func TestBoundedBackoffMatchesTheOldArithmeticOnTheOrdinaryRowsAndSaysWhereItDeliberatelyDiffers(t *testing.T) {
	old := func(initial int64, factor float64) int64 { return initial * int64(factor) }
	for _, tc := range []struct {
		initial int64
		factor  float64
	}{{1000, 1}, {1000, 2}, {1000, 4}, {1000, 1.5}, {1000, 0.99}, {250, 8}, {0, 3}, {1, 1e6}} {
		want := old(tc.initial, tc.factor)
		if want < 1 {
			want = 0 // the loop floors it to 1 ms; both forms feed the same floor
		}
		if got := boundedBackoffMs(tc.initial, tc.factor); got != want {
			t.Errorf("boundedBackoffMs(%d, %v) = %d, old arithmetic %d", tc.initial, tc.factor, got, want)
		}
	}
	for _, tc := range []struct {
		name    string
		initial int64
		factor  float64
	}{
		{"product overflows int64", math.MaxInt64 / 2, 8},
		{"huge initial", math.MaxInt64, 2},
		{"factor is +Inf", 1000, math.Inf(1)},
		{"factor past int64", 1000, 1e30},
	} {
		if got := boundedBackoffMs(tc.initial, tc.factor); got != maxGuestWaitMs {
			t.Errorf("%s: boundedBackoffMs = %d, want the clamp %d", tc.name, got, maxGuestWaitMs)
		}
	}
	// The two classes where the old arithmetic was defined and the new value differs by design.
	if old, got := int64(1_000_000_000_000)*int64(1000.0), boundedBackoffMs(1_000_000_000_000, 1000); old <= maxGuestWaitMs || got != maxGuestWaitMs {
		t.Errorf("old product above the clamp but inside int64: old %d (clamp %d), new %d, want the clamp", old, maxGuestWaitMs, got)
	}
	if old, got := int64(-1000)*int64(-2.0), boundedBackoffMs(-1000, -2); old != 2000 || got != 0 {
		t.Errorf("negative interval times negative factor: old %d, new %d, want 2000 and 0", old, got)
	}
	for _, f := range []float64{math.NaN(), -3, math.Inf(-1)} {
		if got := boundedBackoffMs(1000, f); got != 0 {
			t.Errorf("factor %v: boundedBackoffMs = %d, want 0 (floored to 1 ms by the loop, as before)", f, got)
		}
	}
}
