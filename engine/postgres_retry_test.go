package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

// Same coverage shape as mssql_retry_test.go's
// TestWithRollbackGuaranteedRetry_* suite, retargeted at the PostgreSQL
// predicate and its *pq.Error trigger instead of MSSQL's string-matched one
// -- cleat#3245 Phase 3 step 3 piece B, the production fix for the deadlock
// coordinator and cleat-review independently reproduced against
// TestClaimWorkflows_RespectsLimitUnderConcurrency.

func deadlockErr() error {
	return &pq.Error{Code: "40P01", Message: "deadlock detected"}
}

func TestWithPostgresRollbackGuaranteedRetry_SuccessFirstAttempt(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := withPostgresRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("withPostgresRollbackGuaranteedRetry: unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestWithPostgresRollbackGuaranteedRetry_SuccessAfterDeadlocks(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := withPostgresRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return deadlockErr()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withPostgresRollbackGuaranteedRetry: unexpected error after %d calls: %v", calls, err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestWithPostgresRollbackGuaranteedRetry_SerializationFailureRetried(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := withPostgresRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
		calls++
		if calls < 2 {
			return &pq.Error{Code: "40001", Message: "could not serialize access"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withPostgresRollbackGuaranteedRetry: unexpected error after %d calls: %v", calls, err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestWithPostgresRollbackGuaranteedRetry_PermanentErrorNoRetry(t *testing.T) {
	ctx := context.Background()
	permanentErr := &pq.Error{Code: "23505", Message: "duplicate key"}
	calls := 0
	err := withPostgresRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
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

// TestWithPostgresRollbackGuaranteedRetry_UnknownOutcomeNotRetried: a
// connection drop or context deadline leaves the outcome unknown -- the
// server has not guaranteed a rollback -- so it must not be retried, exactly
// isPostgresRollbackGuaranteed's narrowing versus a broader "is this
// retryable at all" gate.
func TestWithPostgresRollbackGuaranteedRetry_UnknownOutcomeNotRetried(t *testing.T) {
	ctx := context.Background()
	unknownOutcomeErr := fmt.Errorf("connection reset by peer")
	calls := 0
	err := withPostgresRollbackGuaranteedRetry(ctx, "test-op", 3, time.Millisecond, func() error {
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

func TestWithPostgresRollbackGuaranteedRetry_ExhaustedRetries(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := withPostgresRollbackGuaranteedRetry(ctx, "test-op", 2, time.Millisecond, func() error {
		calls++
		return deadlockErr()
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
}

func TestWithPostgresRollbackGuaranteedRetry_ContextCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	err := withPostgresRollbackGuaranteedRetry(ctx, "test-op", 3, 50*time.Millisecond, func() error {
		calls++
		return deadlockErr()
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

func TestIsPostgresRollbackGuaranteed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"40P01 deadlock_detected", &pq.Error{Code: "40P01"}, true},
		{"40001 serialization_failure", &pq.Error{Code: "40001"}, true},
		{"23505 unique_violation", &pq.Error{Code: "23505"}, false},
		{"non-pq error", fmt.Errorf("connection reset"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPostgresRollbackGuaranteed(tc.err); got != tc.want {
				t.Errorf("isPostgresRollbackGuaranteed(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
