package engine

// Tests for the 2.26 wiring decision: which errors a transaction boundary may
// replay.
//
// These construct mssql.Error values directly, which is the practice 2.26
// blames for the original classifier being wrong in both directions. It is
// sound here only because the classification itself is validated against a
// real server elsewhere: TestMSSQLDeadlock_ClassifiedFromTheRealDriverError
// provokes an actual deadlock and asserts the driver returns Number=1205 and
// that isMSSQLRollbackGuaranteed says so. What is under test *here* is the
// retry policy built on top of that, not the classification.

import (
	"context"
	"errors"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestWithRollbackGuaranteedRetry(t *testing.T) {
	// Numbers, not text. mssql.Error{Number: 258} renders as "Wait operation
	// timed out." with the digits appearing nowhere in the message, which is
	// how the original substring matcher missed real timeouts.
	deadlock := mssql.Error{Number: mssqlErrDeadlockVictim, Message: "Transaction ... deadlock victim. Rerun the transaction."}
	timeout := mssql.Error{Number: 258, Message: "Wait operation timed out."}
	permanent := mssql.Error{Number: 208, Message: "Invalid object name 'workflow_instances'."}

	for _, tc := range []struct {
		name         string
		err          error
		wantAttempts int
		wantErr      bool
	}{
		{
			// A deadlock victim's transaction is definitively rolled back,
			// so replaying it is sound whether or not the work is idempotent.
			name: "deadlock is replayed", err: deadlock, wantAttempts: 3, wantErr: true,
		},
		{
			// The outcome is unknown: the commit may have succeeded with only
			// the acknowledgement lost. Replaying could double-apply.
			name: "timeout is not replayed", err: timeout, wantAttempts: 1, wantErr: true,
		},
		{
			name: "permanent error is not replayed", err: permanent, wantAttempts: 1, wantErr: true,
		},
		{
			name: "success does not retry", err: nil, wantAttempts: 1, wantErr: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			err := withRollbackGuaranteedRetry(context.Background(), "op", 2, time.Millisecond, func() error {
				attempts++
				return tc.err
			})
			if attempts != tc.wantAttempts {
				t.Errorf("fn ran %d time(s), want %d", attempts, tc.wantAttempts)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestWithRollbackGuaranteedRetry_SucceedsOnReplay is the point of the whole
// exercise: a deadlock that clears on the second attempt stops being a hard
// error. Before this wiring, IMPROVEMENT-PLAN.md 2.8's support position held —
// "on SQL Server, a deadlock is a hard error today".
func TestWithRollbackGuaranteedRetry_SucceedsOnReplay(t *testing.T) {
	attempts := 0
	err := withRollbackGuaranteedRetry(context.Background(), "claim workflows", 2, time.Millisecond, func() error {
		attempts++
		if attempts == 1 {
			return mssql.Error{Number: mssqlErrDeadlockVictim, Message: "deadlock victim"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil once the replay succeeds", err)
	}
	if attempts != 2 {
		t.Errorf("fn ran %d time(s), want 2", attempts)
	}
}

// TestWithRollbackGuaranteedRetry_HonoursContext checks that a cancelled
// context stops the loop rather than sleeping out the full budget.
// Cancellation is a decision, not a fault -- the same reasoning that made
// context.Canceled non-retryable in the classifier.
func TestWithRollbackGuaranteedRetry_HonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	err := withRollbackGuaranteedRetry(ctx, "op", 5, time.Hour, func() error {
		attempts++
		cancel()
		return mssql.Error{Number: mssqlErrDeadlockVictim, Message: "deadlock victim"}
	})
	if err == nil {
		t.Fatal("err = nil, want a context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("fn ran %d time(s), want 1 -- a cancelled context must not be slept through", attempts)
	}
}

// TestWithRollbackGuaranteedRetry_DoesNotRetryFenceLost guards the claim made
// where the terminal writes are wrapped: wrapping them must not disturb the
// fence.
//
// ErrFenceLost is returned before the commit and is not an mssql.Error, so it
// falls through the rollback-guarantee check and is returned on the first
// attempt. Retrying it would be actively wrong -- the fence is lost because
// another worker legitimately owns the workflow, and that does not change on
// a second attempt.
func TestWithRollbackGuaranteedRetry_DoesNotRetryFenceLost(t *testing.T) {
	attempts := 0
	err := withRollbackGuaranteedRetry(context.Background(), "complete workflow", 2, time.Millisecond, func() error {
		attempts++
		return ErrFenceLost
	})
	if !errors.Is(err, ErrFenceLost) {
		t.Fatalf("err = %v, want ErrFenceLost returned unchanged", err)
	}
	if attempts != 1 {
		t.Errorf("fn ran %d time(s), want 1 -- a lost fence is not a transient fault", attempts)
	}
}
