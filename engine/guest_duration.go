package engine

import (
	"math"
	"time"
)

// maxGuestWaitMs is the longest time quantity the host will act on when a guest asks for it: 100 years, in milliseconds.
//
// cleat#4105. A guest hands the host time as a raw 64-bit integer, and the host multiplies it by time.Millisecond. That product wraps
// past 9.22e12 ms (about 292 years): i64::MAX ms is -1 ms, which is also the python SDK's own "wait indefinitely" sentinel
// (INFINITE_TIMEOUT_MS). A wrapped duration is negative, and what a negative duration does is different at every site and is
// never what the guest asked for: a timer fires at once, a wake-up is placed in the past, a lock is born expired, and
// time.NewTicker panics the process.
//
// So every guest-supplied wait is clamped here, once, before it is multiplied. 100 years is far past anything a workflow can
// mean and far inside int64 (3.15e18 ns of 9.22e18), and it keeps "a very long time" meaning a very long time: the clamp is applied
// to the VALUE USED, never to what is recorded, so a history keeps what the guest asked for.
//
// A NON-positive value is not touched here. Whether zero or a negative means "do not wait", "timed out" or "refused" is each call's
// own contract and several tests pin it; only the heartbeat interval, which cannot be zero, refuses (see freshCallWithHeartbeat).
const maxGuestWaitMs = int64(100 * 365 * 24 * time.Hour / time.Millisecond)

// clampGuestMs bounds a guest-supplied millisecond count from above. See maxGuestWaitMs.
func clampGuestMs(ms int64) int64 {
	if ms > maxGuestWaitMs {
		return maxGuestWaitMs
	}
	return ms
}

// guestMsDuration is clampGuestMs as a Duration, which is the only form in which a guest's milliseconds may meet time.Millisecond.
func guestMsDuration(ms int64) time.Duration {
	return time.Duration(clampGuestMs(ms)) * time.Millisecond
}

// boundedBackoffMs is initialMs times the integer part of factor, never past maxGuestWaitMs. A product that is not positive is 0, which the
// retry loop already floors to one millisecond (the same outcome the unbounded arithmetic gave a non-positive interval or factor).
func boundedBackoffMs(initialMs int64, factor float64) int64 {
	initialMs = clampGuestMs(initialMs)
	if math.IsNaN(factor) || initialMs <= 0 || factor < 1 {
		return 0
	}
	if factor >= float64(maxGuestWaitMs) {
		return maxGuestWaitMs
	}
	f := int64(factor)
	if initialMs > maxGuestWaitMs/f {
		return maxGuestWaitMs
	}
	return initialMs * f
}
