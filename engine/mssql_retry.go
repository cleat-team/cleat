package engine

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

// withRollbackGuaranteedRetry retries fn while the error it returns is one SQL
// Server guarantees it rolled back.
//
// This is the distinction IMPROVEMENT-PLAN.md 2.26 says wiring requires, and
// the reason mssqlRetry itself cannot be wrapped around a transaction.
// mssqlRetry gates on isMSSQLRetryable, which includes timeouts (258) and
// dropped connections -- errors that leave the outcome *unknown*, where the
// commit may have succeeded with only the acknowledgement lost. Replaying a
// non-idempotent transaction after one of those can double-apply it, which for
// a workflow engine means a duplicated side effect.
//
// isMSSQLRollbackGuaranteed is the narrower set: deadlock victim (1205) and
// snapshot/update conflicts (3960, 41301-41325). The server has definitively
// undone the transaction, so replaying it is sound whether or not the work is
// idempotent -- which means this wrapper is safe at a transaction boundary
// without a per-transaction idempotency analysis.
//
// Unknown-outcome errors are returned unretried, exactly as today. Making
// those retryable is a separate decision, per transaction, and is not taken
// here.
func withRollbackGuaranteedRetry(ctx context.Context, op string, maxRetries int, baseDelay time.Duration, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
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
		if !isMSSQLRollbackGuaranteed(err) {
			return err
		}
	}
	return fmt.Errorf("%s: exhausted %d retries: %w", op, maxRetries, lastErr)
}

// mssqlClaimRetries and mssqlClaimRetryDelay bound the claim-path retry. The
// claim is the highest-contention transaction in the engine and the one where
// a deadlock is most likely; three attempts at 20ms/40ms costs at most 60ms of
// added latency on a path that otherwise fails outright.
const (
	mssqlClaimRetries    = 2
	mssqlClaimRetryDelay = 20 * time.Millisecond
)
