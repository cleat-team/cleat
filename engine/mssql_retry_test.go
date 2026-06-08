package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMSSQLRetry_SuccessFirstAttempt(t *testing.T) {
	ctx := context.Background()
	calls := 0
	fn := func() error {
		calls++
		return nil
	}
	err := mssqlRetry(ctx, "test", 3, time.Millisecond, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestMSSQLRetry_RetryThenSucceed(t *testing.T) {
	ctx := context.Background()
	calls := 0
	fn := func() error {
		calls++
		if calls < 3 {
			return errors.New("deadlock victim")
		}
		return nil
	}
	err := mssqlRetry(ctx, "test", 3, time.Millisecond, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestMSSQLRetry_ExhaustedRetries(t *testing.T) {
	ctx := context.Background()
	fn := func() error {
		return errors.New("deadlock")
	}
	err := mssqlRetry(ctx, "test", 2, time.Millisecond, fn)
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if !strings.Contains(err.Error(), "exhausted 2 retries") {
		t.Errorf("error = %q, want exhausted retries message", err.Error())
	}
}

func TestMSSQLRetry_NonRetryableError(t *testing.T) {
	ctx := context.Background()
	calls := 0
	fn := func() error {
		calls++
		return errors.New("permanent schema error")
	}
	err := mssqlRetry(ctx, "test", 3, time.Millisecond, fn)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retries on permanent error)", calls)
	}
}

func TestMSSQLRetry_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	fn := func() error {
		calls++
		if calls == 1 {
			cancel() // cancel after first attempt
		}
		return errors.New("deadlock")
	}
	err := mssqlRetry(ctx, "test", 3, time.Millisecond, fn)
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}
	if !strings.Contains(err.Error(), "context cancelled") {
		t.Errorf("error = %q, want context cancelled", err.Error())
	}
}

func TestMSSQLRetry_MaxRetriesZero(t *testing.T) {
	ctx := context.Background()
	calls := 0
	fn := func() error {
		calls++
		return errors.New("deadlock")
	}
	err := mssqlRetry(ctx, "test", 0, time.Millisecond, fn)
	if err == nil {
		t.Fatal("expected error when maxRetries=0 and fn always fails")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (maxRetries=0 means 1 attempt)", calls)
	}
}

func TestMSSQLRetry_ContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fn := func() error {
		return errors.New("deadlock")
	}
	err := mssqlRetry(ctx, "test", 3, time.Millisecond, fn)
	if err == nil {
		t.Fatal("expected error when context already cancelled")
	}
}
