package cleat

import (
	"math"
	"testing"
	"time"
)

// cleat#4144. await_promise's timeout reaches the host through a Duration, and two conversions on the way were wrong: a positive
// sub-millisecond wait truncated to 0 (which the host reads as "do not wait"), and AwaitPromiseMs multiplied a millisecond count by
// time.Millisecond, which wraps for a count above about 9.2e12 and produced a negative the host reads the same way.
func promiseWaitSeen(t *testing.T) (HostCalls, *[]time.Duration) {
	t.Helper()
	var seen []time.Duration
	h := NewHostCalls(HostCallsOptions{
		AwaitPromise: func(_ string, timeout time.Duration) (string, bool, error) {
			seen = append(seen, timeout)
			return "", true, nil
		},
	})
	return h, &seen
}

func TestAPositiveSubMillisecondPromiseWaitIsRoundedUpNotToZero(t *testing.T) {
	for _, d := range []time.Duration{1, 400 * time.Microsecond, 999_999} {
		h, seen := promiseWaitSeen(t)
		if _, _, err := h.AwaitPromise("p", d); err != nil {
			t.Fatal(err)
		}
		if len(*seen) != 1 || (*seen)[0].Milliseconds() < 1 {
			t.Errorf("AwaitPromise(%v) reached the host as %v: Milliseconds() truncates a positive sub-ms wait to 0", d, *seen)
		}
	}
}

func TestAnOrdinaryZeroAndNegativePromiseWaitAreNotChanged(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, 5 * time.Second} {
		h, seen := promiseWaitSeen(t)
		_, _, _ = h.AwaitPromise("p", d)
		if len(*seen) != 1 || (*seen)[0] != d {
			t.Errorf("AwaitPromise(%v) reached the host as %v, want it unchanged", d, *seen)
		}
	}
}

func TestAwaitPromiseMsSaturatesInsteadOfWrapping(t *testing.T) {
	for _, ms := range []int64{math.MaxInt64, math.MaxInt64 / 2, 9_300_000_000_000} {
		h, seen := promiseWaitSeen(t)
		_, _, _ = h.AwaitPromiseMs("p", ms)
		if len(*seen) != 1 || (*seen)[0] <= 0 {
			t.Errorf("AwaitPromiseMs(%d) reached the host as %v: time.Duration(ms)*time.Millisecond wrapped", ms, *seen)
		}
	}
	// CONTROL: an ordinary count is exact.
	h, seen := promiseWaitSeen(t)
	_, _, _ = h.AwaitPromiseMs("p", 1500)
	if len(*seen) != 1 || (*seen)[0] != 1500*time.Millisecond {
		t.Errorf("AwaitPromiseMs(1500) reached the host as %v, want 1.5s", *seen)
	}
}
