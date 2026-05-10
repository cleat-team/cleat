package host

import (
	"context"
	"fmt"
	"time"
)

// mssqlRetry executes fn with retry logic for transient SQL Server errors.
// It retries up to maxRetries times with exponential backoff starting at baseDelay.
// Only retries on transient errors (deadlocks, snapshot conflicts, connection issues).
func mssqlRetry(ctx context.Context, op string, maxRetries int, baseDelay time.Duration, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// exponential backoff: 1x, 2x, 4x, 8x, ...
			delay := baseDelay * (1 << (attempt - 1))
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: context cancelled during retry: %w", op, ctx.Err())
			case <-time.After(delay):
			}
		}

		err := fn()
		if err == nil {
			return nil
		}

		lastErr = err
		if !isMSSQLRetryable(err) {
			return err // don't retry permanent errors
		}
	}
	return fmt.Errorf("%s: exhausted %d retries: %w", op, maxRetries, lastErr)
}
