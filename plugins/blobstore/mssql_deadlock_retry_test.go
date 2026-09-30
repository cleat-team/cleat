package blobstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// These pin isMSSQLDeadlock and withMSSQLDeadlockRetry, added for cleat#2303:
// TestStaleWorkflowRefs_SparesAnInFlightWorkflow_MultiBackend/mssql deadlocked
// once during a concurrent run, in sweepStaleWorkflowRefsMSSQL's own queries.
// Reproduced deliberately -- six `go test` processes against one MSSQL
// container, CLEAT_TEST_ALLOW_FOREIGN_SESSIONS=1 to get past cleat#982's
// guard -- and confirmed real, with the driver's own verbatim text:
//
//	mssql: Transaction (Process ID 77) was deadlocked on lock resources with
//	another process and has been chosen as the deadlock victim. Rerun the
//	transaction. (1205)
//
// The shape mirrors engine/mssql_retry_test.go's coverage of
// withRollbackGuaranteedRetry -- same loop, same class of error, restated
// here because plugins/blobstore cannot import engine's unexported version.

func TestIsMSSQLDeadlock_RealDriverText(t *testing.T) {
	// The exact string measured live, reproducing cleat#2303.
	err := errors.New("mssql: Transaction (Process ID 77) was deadlocked on " +
		"lock resources with another process and has been chosen as the " +
		"deadlock victim. Rerun the transaction. (1205)")
	if !isMSSQLDeadlock(err) {
		t.Errorf("isMSSQLDeadlock(%q) = false, want true -- this is the verbatim "+
			"error cleat#2303's reproduction captured", err)
	}
}

func TestIsMSSQLDeadlock_NotAPermanentError(t *testing.T) {
	// A known-negative, per this file's own "could this check have
	// disagreed?" obligation: a syntax error must not be read as a deadlock,
	// or every permanent MSSQL error becomes silently retried three times
	// before it is ever reported.
	for _, msg := range []string{
		"mssql: Invalid column name 'foo'.",
		"mssql: Incorrect syntax near 'SELECT'.",
		"mssql: Violation of PRIMARY KEY constraint",
		"context deadline exceeded",
	} {
		if isMSSQLDeadlock(errors.New(msg)) {
			t.Errorf("isMSSQLDeadlock(%q) = true, want false", msg)
		}
	}
}

func TestIsMSSQLDeadlock_Nil(t *testing.T) {
	if isMSSQLDeadlock(nil) {
		t.Error("isMSSQLDeadlock(nil) = true, want false")
	}
}

func TestWithMSSQLDeadlockRetry_SuccessFirstAttempt(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := withMSSQLDeadlockRetry(ctx, "test-op", func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("withMSSQLDeadlockRetry: unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestWithMSSQLDeadlockRetry_SuccessAfterDeadlocks(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := withMSSQLDeadlockRetry(ctx, "test-op", func() error {
		calls++
		if calls < mssqlDeadlockRetries+1 {
			return errors.New("mssql: ... was deadlocked on lock resources ... (1205)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withMSSQLDeadlockRetry: unexpected error after %d calls: %v", calls, err)
	}
	if calls != mssqlDeadlockRetries+1 {
		t.Errorf("expected %d calls, got %d", mssqlDeadlockRetries+1, calls)
	}
}

func TestWithMSSQLDeadlockRetry_PermanentErrorNoRetry(t *testing.T) {
	ctx := context.Background()
	permanentErr := errors.New("mssql: Invalid column name 'foo'.")
	calls := 0
	err := withMSSQLDeadlockRetry(ctx, "test-op", func() error {
		calls++
		return permanentErr
	})
	if !errors.Is(err, permanentErr) {
		t.Errorf("expected permanentErr, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on a non-deadlock error), got %d", calls)
	}
}

func TestWithMSSQLDeadlockRetry_ExhaustedRetries(t *testing.T) {
	ctx := context.Background()
	transientErr := errors.New("mssql: ... deadlock victim ... (1205)")
	calls := 0
	err := withMSSQLDeadlockRetry(ctx, "test-op", func() error {
		calls++
		return transientErr
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if calls != mssqlDeadlockRetries+1 {
		t.Errorf("expected %d calls (mssqlDeadlockRetries+1), got %d", mssqlDeadlockRetries+1, calls)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("exhausted %d retries", mssqlDeadlockRetries)) {
		t.Errorf("error should mention exhausted retries: %v", err)
	}
	if !errors.Is(err, transientErr) {
		t.Errorf("error should wrap transientErr: %v", err)
	}
}

func TestWithMSSQLDeadlockRetry_ContextCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	err := withMSSQLDeadlockRetry(ctx, "test-op", func() error {
		calls++
		return errors.New("mssql: ... deadlock victim ... (1205)")
	})
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	// At least the first attempt ran before cancellation could land; this
	// is a timing-tolerant lower bound, not an exact count.
	if calls < 1 {
		t.Errorf("expected at least 1 call, got %d", calls)
	}
}
