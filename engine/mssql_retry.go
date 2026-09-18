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

// mssqlTxRetries and mssqlTxRetryDelay bound a retry at a transaction
// boundary. Four attempts at 20/40/80ms cost at most 140ms of added latency
// on a path that otherwise fails outright, which is the right trade for a
// deadlock: the alternative is surfacing it to the caller as a hard error.
//
// MEASURED, not guessed -- cleat#1885, found stress-testing #1884's MySQL
// finalize fix pointed at MSSQL instead. FinalizeWorkflowSegment already
// retried a deadlock victim (1205) here; what was open was whether the
// budget was enough. Same 8-racer, disjoint-step-range contention as #1884,
// against a real `mssql/server:2022-latest` container:
//
//	budget (this const)   total attempts   rounds observed   exhausted
//	2 (the old value)      3                5 x 40 = 200      2  (~1%)
//	3 (this value)         4                4 x 40 = 160      0
//	4                      5                1 x 40             0
//	5                      6                1 x 40             0
//
// The two exhaustions at budget 2 reproduced the exact reported error
// verbatim (1205, "Rerun the transaction"), so the mechanism is real, not
// noise -- and it is real at a materially lower rate than #1884's MySQL
// deadlock (reliably 40/40 with that fix's retry bypassed), which is the
// same load pattern hitting SQL Server's lock manager rather than InnoDB's.
//
// STATED AS A MARGIN, NOT A GUARANTEE: 0 of 160 at budget 3 does not prove
// the true rate is zero -- with a true rate as high as 1% (budget 2's
// measured rate), seeing zero failures in 160 independent rounds has ~20%
// probability by chance alone. What it supports is "meaningfully lower than
// budget 2, at a cost of one more attempt (80ms) only on the already-rare
// contended path" -- which is what decided +1 over the larger jumps: budget
// 4 and 5 added no further observed improvement over budget 3 in this
// session's samples, so there is no measured argument for costing every
// caller more than this.
//
// mssqlTxRetries is SHARED across ~30 call sites (mssql_lifecycle.go,
// mssql_operations.go, mssql_schedules.go, mssql_signals_promises.go,
// mssql_defer_phase.go, store_admin.go, store_admin_rereplay.go,
// mssql_deployment.go) -- this budget is not finalize-specific, so the added
// worst-case latency is paid by every one of them, not only the path that
// motivated the measurement.
const (
	mssqlTxRetries    = 3
	mssqlTxRetryDelay = 20 * time.Millisecond
)
