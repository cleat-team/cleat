package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// MaxInterval is an OPTIONAL field on RetryPolicy. Leaving it at its zero value
// used to clamp every backoff to 0 -- the clamp was unconditional -- which the
// 1ms floor below it then turned into a tight retry loop. Retries happened,
// immediately, and the configured InitialInterval was silently ignored.
//
// Measured before the fix: six attempts at a 2s interval completed in 384ms.
// The port suite's retry workflow carries a comment about it and sets
// MaxInterval defensively, which is a workaround written into a caller rather
// than a fix, and it only protects that one caller.
//
// retryPolicyFitsBudget, in the same file, always had the guard:
//
//	if maxInterval > 0 && interval > maxInterval {
//
// So the budget check and the executor disagreed about what the same policy
// meant. Zero is "no maximum" to one and "a maximum of zero" to the other --
// which is also why the check could reject a policy as exceeding the budget
// that the executor would have run in a millisecond per attempt.
// TestTheBudgetCheckAndTheExecutorAgreeOnZeroMaxInterval pins that they now
// read it the same way.

// countingFailCaller fails the first n calls, then succeeds.
type countingFailCaller struct {
	failures int
	calls    int
}

func (c *countingFailCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	c.calls++
	if c.calls <= c.failures {
		return "", errors.New("temporary: please retry")
	}
	return `{"ok":true}`, nil
}

func TestAZeroMaxIntervalDoesNotDisableBackoff(t *testing.T) {
	const (
		attempts   = 3
		intervalMs = 200
		failures   = 2
	)

	caller := &countingFailCaller{failures: failures}
	s := &execSession{engine: NewEngine(nil, caller)}

	start := time.Now()
	// maxIntervalMs = 0, the case a caller gets by not setting the field.
	// backoffCoefficient100x = 100 means a flat interval, so the expected wait
	// is exactly failures * intervalMs and the assertion needs no modelling of
	// exponential growth.
	s.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
		attempts, intervalMs, 100, 0, "", 0, 0)
	elapsed := time.Since(start)

	if caller.calls != failures+1 {
		t.Fatalf("the caller was reached %d times, want %d; this test cannot say "+
			"anything about backoff if the retries themselves did not happen",
			caller.calls, failures+1)
	}

	// 0.8 of the nominal wait, to absorb timer granularity without absorbing
	// the defect: the broken behaviour was 1ms per retry, two orders of
	// magnitude below this floor.
	floor := time.Duration(float64(failures*intervalMs)*0.8) * time.Millisecond
	if elapsed < floor {
		t.Errorf("%d retries at a %dms interval took %v, want at least %v.\n"+
			"A zero MaxInterval must mean 'no maximum', not 'a maximum of zero'. "+
			"The clamp in DurableCallWithRetry has to be guarded by maxIntervalMs > 0, "+
			"the way retryPolicyFitsBudget's already is -- otherwise every backoff is "+
			"clamped to 0, raised to the 1ms floor, and InitialInterval is ignored.",
			failures, intervalMs, elapsed, floor)
	}
}

// TestAMaxIntervalStillCaps is the other direction: the guard must not turn the
// clamp off for callers who do set it.
func TestAMaxIntervalStillCaps(t *testing.T) {
	const (
		attempts   = 3
		intervalMs = 5000 // would be a 10s wait across two retries
		maxMs      = 50   // capped to 50ms each
		failures   = 2
	)

	caller := &countingFailCaller{failures: failures}
	s := &execSession{engine: NewEngine(nil, caller)}

	start := time.Now()
	s.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
		attempts, intervalMs, 100, maxMs, "", 0, 0)
	elapsed := time.Since(start)

	if caller.calls != failures+1 {
		t.Fatalf("caller reached %d times, want %d", caller.calls, failures+1)
	}
	if elapsed > 2*time.Second {
		t.Errorf("%d retries with MaxInterval=%dms took %v; the cap is not being applied",
			failures, maxMs, elapsed)
	}
}

// TestTheBudgetCheckAndTheExecutorAgreeOnZeroMaxInterval states the invariant
// the fix restores, in the terms the two sites share.
//
// retryPolicyFitsBudget treats a zero maxInterval as "no maximum" -- it skips
// the clamp entirely -- so its worst case for a flat 200ms policy over three
// attempts is two full intervals. The executor must reach the same number, or a
// policy admitted by the check runs for a different length than the check
// approved.
func TestTheBudgetCheckAndTheExecutorAgreeOnZeroMaxInterval(t *testing.T) {
	// Just under the two 200ms waits the check models: the policy must NOT fit.
	if retryPolicyFitsBudget(3, 200, 100, 0, 399*time.Millisecond) {
		t.Error("retryPolicyFitsBudget says a 3-attempt, 200ms-interval policy fits in 399ms; " +
			"it models two full 200ms waits, so it should not")
	}
	// Just over: it must fit.
	if !retryPolicyFitsBudget(3, 200, 100, 0, 401*time.Millisecond) {
		t.Error("retryPolicyFitsBudget says the same policy does not fit in 401ms")
	}
	// And the executor must actually take about that long -- asserted by
	// TestAZeroMaxIntervalDoesNotDisableBackoff above. Before the fix it took
	// 2ms, so the check was reserving 400ms for something that never waited.
}
