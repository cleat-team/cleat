package migration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

func TestLockTimeoutSQL_Postgres(t *testing.T) {
	r := &Runner{dialect: engine.DialectPostgres, lockTimeout: 5 * time.Second}
	want := "SET LOCAL lock_timeout = '5000ms'"
	if got := r.lockTimeoutSQL(); got != want {
		t.Errorf("lockTimeoutSQL() = %q, want %q", got, want)
	}
}

func TestLockTimeoutSQL_MySQL(t *testing.T) {
	r := &Runner{dialect: engine.DialectMySQL, lockTimeout: 5 * time.Second}
	want := "SET SESSION innodb_lock_wait_timeout = 5"
	if got := r.lockTimeoutSQL(); got != want {
		t.Errorf("lockTimeoutSQL() = %q, want %q", got, want)
	}
}

func TestLockTimeoutSQL_MSSQL(t *testing.T) {
	r := &Runner{dialect: engine.DialectMSSQL, lockTimeout: 5 * time.Second}
	want := "SET LOCK_TIMEOUT 5000"
	if got := r.lockTimeoutSQL(); got != want {
		t.Errorf("lockTimeoutSQL() = %q, want %q", got, want)
	}
}

func TestLockTimeoutSQL_Zero(t *testing.T) {
	// With lockTimeout = 0, the SQL string is still computed per-dialect
	// but the caller checks lockTimeout > 0 before issuing it.
	r := &Runner{dialect: engine.DialectPostgres, lockTimeout: 0}
	want := "SET LOCAL lock_timeout = '0ms'"
	if got := r.lockTimeoutSQL(); got != want {
		t.Errorf("lockTimeoutSQL() = %q, want %q", got, want)
	}
}

func TestLockTimeoutSQL_UnknownDialect(t *testing.T) {
	r := &Runner{dialect: "unknown", lockTimeout: 5 * time.Second}
	if got := r.lockTimeoutSQL(); got != "" {
		t.Errorf("lockTimeoutSQL() for unknown dialect = %q, want empty", got)
	}
}

func TestRunMigrationWithRetry_Success(t *testing.T) {
	ctx := context.Background()
	calls := 0
	applyFn := func(ctx context.Context) error {
		calls++
		return nil
	}
	err := runMigrationWithRetry(ctx, "001_test.sql", applyFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRunMigrationWithRetry_SuccessAfterFailures(t *testing.T) {
	ctx := context.Background()
	calls := 0
	applyFn := func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return fmt.Errorf("transient error")
		}
		return nil
	}
	err := runMigrationWithRetry(ctx, "002_test.sql", applyFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestRunMigrationWithRetry_AllFailures(t *testing.T) {
	ctx := context.Background()
	wantErr := fmt.Errorf("permanent connection error")
	applyFn := func(ctx context.Context) error {
		return wantErr
	}
	err := runMigrationWithRetry(ctx, "003_test.sql", applyFn)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Final error must include the migration name and attempt count.
	msg := err.Error()
	if !strings.Contains(msg, "003_test.sql") {
		t.Errorf("error should include migration name, got: %s", msg)
	}
	if !strings.Contains(msg, "failed after 3 attempts") {
		t.Errorf("error should include 'failed after 3 attempts', got: %s", msg)
	}
}

func TestRunMigrationWithRetry_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	applyFn := func(ctx context.Context) error {
		return fmt.Errorf("some error")
	}
	// Cancel the context so the retry delay is interrupted.
	cancel()
	err := runMigrationWithRetry(ctx, "004_test.sql", applyFn)
	if err == nil {
		t.Fatal("expected error from context cancellation, got nil")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestNewRunner_Defaults(t *testing.T) {
	r := NewRunner(nil, engine.DialectPostgres, "migrations", 5*time.Second)
	if r.lockTimeout != 5*time.Second {
		t.Errorf("expected lockTimeout 5s, got %v", r.lockTimeout)
	}
	if r.applyMigrationFn == nil {
		t.Error("applyMigrationFn should not be nil")
	}
}
