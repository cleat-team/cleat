package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// DefaultFlushRetryWindow is how long a failed event flush is retried when no
// window is configured.
//
// It is the SLEEP BUDGET the batch path has always had, not a new number.
// retryBatchFlush was `maxRetries = 5` with 50ms doubling, so its four sleeps
// totalled 50+100+200+400 = 750ms, and `maxBackoff = 2s` was unreachable.
// Expressed as a deadline with the same backoff, 750ms yields the same five
// attempts -- see TestTheDefaultWindowReproducesTheHistoricalFiveAttempts,
// which measures it rather than asserting the arithmetic.
//
// WHY THE DEFAULT IS NOT LONGER. A database failover outlasts this by two
// orders of magnitude, and covering one is exactly what --flush-retry-window
// exists for. But the retry sits on the engine's hottest write path, and
// errIsRetryable returns true for errors it does not recognise -- so raising
// the default would make an unrecognised PERMANENT error stall every step by
// the new window. An operator who knows their failover budget can say so;
// nobody else pays for it. cleat#1717.
const DefaultFlushRetryWindow = 750 * time.Millisecond

// flushRetryBaseBackoff is the first sleep; each subsequent one doubles.
const flushRetryBaseBackoff = 50 * time.Millisecond

// flushRetryMaxBackoff caps the doubling, scaled to the window.
//
// At the default window the cap is 2s and unreachable, which is what the
// constant it replaces did. At a window sized for a failover it matters: a
// fixed 2s cap over a 5-minute window is ~150 attempts against a database that
// is down, where window/20 gives 15s and ~25. Measured by
// TestHowManyAttemptsAWindowBuys.
func flushRetryMaxBackoff(window time.Duration) time.Duration {
	return max(2*time.Second, window/20)
}

// retryFlushUntilDeadline calls fn until it succeeds, until fn returns an error
// that retrying cannot fix, or until window has elapsed.
//
// THE DEADLINE IS CHECKED BEFORE SLEEPING, NOT BEFORE ATTEMPTING, and that is
// deliberate rather than incidental: a check of "would this backoff carry me
// past the deadline" would make the default window buy four attempts where the
// attempt-count form it replaces always bought five. Checking first means the
// last attempt may start just inside the deadline and overshoot it by up to one
// backoff -- bounded by flushRetryMaxBackoff, so 2s at the default and 15s at a
// 5-minute window. A caller for whom that overshoot matters should pass a ctx
// with its own deadline; ctx is honoured during every sleep.
//
// window <= 0 means DefaultFlushRetryWindow. There is no way to spell "do not
// retry at all", because no caller wants one: the direct path had no retry and
// that is the defect, not the configuration.
func retryFlushUntilDeadline(ctx context.Context, window time.Duration, op string, fn func() error) error {
	if window <= 0 {
		window = DefaultFlushRetryWindow
	}
	maxBackoff := flushRetryMaxBackoff(window)
	deadline := time.Now().Add(window)

	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s failed after %d attempts over %s: %w", op, attempt, window, lastErr)
			}
			backoff := time.Duration(math.Min(
				float64(flushRetryBaseBackoff)*math.Pow(2, float64(attempt-1)),
				float64(maxBackoff)))
			slog.Warn("retrying event flush",
				"op", op,
				"attempt", attempt,
				"backoff", backoff,
				"window", window,
				"prevErr", lastErr,
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !errIsRetryable(err) {
			return err
		}
	}
}

// errFlushRetryPointless reports whether err describes a refusal that a later
// attempt would receive identically.
//
// ErrFenceLost is the case that made this necessary. It is not a database
// failure at all -- another worker holds the claim, which is the normal outcome
// of reaping -- and it is what the direct flush path returns most often when it
// returns anything. errIsRetryable's closing clause returns true for errors it
// does not recognise, so without this a routine reassignment would sleep out
// the whole window on every subsequent step of a zombie's segment.
func errFlushRetryPointless(err error) bool {
	return errors.Is(err, ErrFenceLost) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}
