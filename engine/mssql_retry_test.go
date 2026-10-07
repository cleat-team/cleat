package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests targeted mssqlRetry until cleat#2758 deleted it (ConsumeSignal
// was its only production caller; see mssql_retry.go's doc comment on
// withRollbackGuaranteedRetry). Retargeted to withRollbackGuaranteedRetry,
// the surviving retry primitive with the identical loop shape, so the
// backoff/exhaustion/cancellation coverage isn't lost along with the dead
// function. Triggers that relied on mssqlRetry's wider gate (timeouts,
// connection resets) were swapped for ones isMSSQLRollbackGuaranteed also
// accepts (deadlock, snapshot conflict); TestWithRollbackGuaranteedRetry_UnknownOutcomeNotRetried
// is new and asserts the one behavior that distinguishes the two gates.

func TestWithRollbackGuaranteedRetry_SuccessFirstAttempt(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("withRollbackGuaranteedRetry: unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestWithRollbackGuaranteedRetry_SuccessAfterRollbackGuaranteedErrors(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("deadlock victim")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withRollbackGuaranteedRetry: unexpected error after %d calls: %v", calls, err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestWithRollbackGuaranteedRetry_PermanentErrorNoRetry(t *testing.T) {
	ctx := context.Background()
	permanentErr := fmt.Errorf("syntax error near SELECT")
	calls := 0
	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		calls++
		return permanentErr
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, permanentErr) {
		t.Errorf("expected permanentErr, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on permanent), got %d", calls)
	}
}

// TestWithRollbackGuaranteedRetry_UnknownOutcomeNotRetried is the test that
// did not exist for mssqlRetry, because mssqlRetry retried this case --
// that was the bug. A timeout or dropped connection leaves the outcome
// unknown, so withRollbackGuaranteedRetry must return it unretried rather
// than replay a possibly-already-applied statement.
func TestWithRollbackGuaranteedRetry_UnknownOutcomeNotRetried(t *testing.T) {
	ctx := context.Background()
	unknownOutcomeErr := fmt.Errorf("connection reset by peer")
	calls := 0
	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		calls++
		return unknownOutcomeErr
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, unknownOutcomeErr) {
		t.Errorf("expected unknownOutcomeErr, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on unknown-outcome error), got %d", calls)
	}
}

func TestWithRollbackGuaranteedRetry_ExhaustedRetries(t *testing.T) {
	ctx := context.Background()
	transientErr := fmt.Errorf("deadlock victim")
	calls := 0
	err := withRollbackGuaranteedRetry(ctx, "test-op", 2, time.Millisecond, func() error {
		calls++
		return transientErr
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	// maxRetries=2 means 3 attempts (0, 1, 2)
	if calls != 3 {
		t.Errorf("expected 3 calls (maxRetries+1), got %d", calls)
	}
	if !strings.Contains(err.Error(), "exhausted 2 retries") {
		t.Errorf("error should mention exhausted retries: %v", err)
	}
	if !errors.Is(err, transientErr) {
		t.Errorf("error should wrap transientErr: %v", err)
	}
}

func TestWithRollbackGuaranteedRetry_ContextCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	// Cancel the context after the first attempt fails.
	// Use a goroutine with a small delay so it cancels during backoff.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, 50*time.Millisecond, func() error {
		calls++
		return fmt.Errorf("deadlock victim") // rollback-guaranteed, triggers retry
	})
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call before cancellation, got %d", calls)
	}
}

func TestWithRollbackGuaranteedRetry_AlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		return fmt.Errorf("deadlock victim")
	})
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestWithRollbackGuaranteedRetry_MixedErrorsStopsOnPermanent(t *testing.T) {
	ctx := context.Background()
	calls := 0
	permanentErr := fmt.Errorf("violation of PRIMARY KEY constraint error 2627")

	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("deadlock victim") // rollback-guaranteed
		}
		return permanentErr // non-retryable
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, permanentErr) {
		t.Errorf("expected permanentErr, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestWithRollbackGuaranteedRetry_MaxRetriesZero(t *testing.T) {
	ctx := context.Background()
	transientErr := fmt.Errorf("deadlock victim")
	calls := 0
	err := withRollbackGuaranteedRetry(ctx, "test-op", 0, time.Millisecond, func() error {
		calls++
		return transientErr
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// maxRetries=0 means 1 attempt (maxRetries+1)
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
	if !errors.Is(err, transientErr) {
		t.Errorf("expected transientErr, got %v", err)
	}
}

func TestWithRollbackGuaranteedRetry_DeadlineExceededDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Millisecond))
	defer cancel()

	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, 50*time.Millisecond, func() error {
		return fmt.Errorf("snapshot isolation update conflict error 3960") // rollback-guaranteed
	})
	if err == nil {
		t.Fatal("expected deadline exceeded error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestWithRollbackGuaranteedRetry_NilErrorFromFn(t *testing.T) {
	// Verify that a nil error from the function (not just no error) is handled.
	ctx := context.Background()
	calls := 0
	err := withRollbackGuaranteedRetry(ctx, "test-op", 2, time.Millisecond, func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("deadlock victim") // rollback-guaranteed, retry
		}
		return nil // explicit nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

// TestWithRollbackGuaranteedRetry_ExponentialBackoffShape verifies that backoff increases
// exponentially by checking that the total elapsed time with a known baseDelay
// is within expected bounds. With maxRetries=3 and baseDelay=10ms:
// attempt 0: immediate, attempt 1: 10ms, attempt 2: 20ms, attempt 3: 40ms
// Total = ~70ms. We check it's >= 30ms (generous lower bound) and < 500ms.
func TestWithRollbackGuaranteedRetry_ExponentialBackoffShape(t *testing.T) {
	ctx := context.Background()
	transientErr := fmt.Errorf("deadlock victim")
	start := time.Now()
	err := withRollbackGuaranteedRetry(ctx, "test-op", 3, 10*time.Millisecond, func() error {
		return transientErr
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if elapsed < 30*time.Millisecond {
		t.Errorf("backoff too fast: %v (expected >= 30ms for 3 retries with 10ms base)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("backoff too slow: %v (expected < 500ms)", elapsed)
	}
}
