package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// ---------------------------------------------------------------------------
// isDuplicateKeyError
// ---------------------------------------------------------------------------

func TestIsDuplicateKeyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"MySQLError 1062", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}, true},
		{"MySQLError different code", &mysql.MySQLError{Number: 1064, Message: "Parse error"}, false},
		{"non-MySQL error", fmt.Errorf("something went wrong"), false},
		{"wrapped MySQLError 1062", fmt.Errorf("wrap: %w", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}), true},
		{"wrapped MySQLError different code", fmt.Errorf("wrap: %w", &mysql.MySQLError{Number: 1064, Message: "Parse error"}), false},
		{"empty error message", fmt.Errorf(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDuplicateKeyError(tt.err)
			if got != tt.want {
				t.Errorf("isDuplicateKeyError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isLockWaitTimeout
// ---------------------------------------------------------------------------

func TestIsLockWaitTimeout(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"MySQLError 1205", &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout"}, true},
		{"MySQLError different code", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}, false},
		{"non-MySQL error", fmt.Errorf("something went wrong"), false},
		{"wrapped MySQLError 1205", fmt.Errorf("wrap: %w", &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout"}), true},
		{"wrapped MySQLError different code", fmt.Errorf("wrap: %w", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}), false},
		{"empty error message", fmt.Errorf(""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLockWaitTimeout(tt.err)
			if got != tt.want {
				t.Errorf("isLockWaitTimeout(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// errors.As behavior — verify the production code's use of errors.As matches
// our test harness assumptions.
// ---------------------------------------------------------------------------

func TestIsDuplicateKeyErrorErrorsAsBehavior(t *testing.T) {
	// Verify that errors.As on a *mysql.MySQLError works as the production code expects.
	mysqlErr := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'foo' for key 'idx'"}
	var extracted *mysql.MySQLError
	if !errors.As(mysqlErr, &extracted) {
		t.Fatal("errors.As should succeed for *mysql.MySQLError")
	}
	if extracted.Number != 1062 {
		t.Fatalf("extracted.Number = %d, want 1062", extracted.Number)
	}

	// Verify wrapped extraction.
	wrapped := fmt.Errorf("outer: %w", mysqlErr)
	var extractedWrapped *mysql.MySQLError
	if !errors.As(wrapped, &extractedWrapped) {
		t.Fatal("errors.As should succeed for wrapped *mysql.MySQLError")
	}
	if extractedWrapped.Number != 1062 {
		t.Fatalf("extractedWrapped.Number = %d, want 1062", extractedWrapped.Number)
	}

	// Verify non-MySQLError does NOT match.
	plainErr := fmt.Errorf("plain error")
	var extractedPlain *mysql.MySQLError
	if errors.As(plainErr, &extractedPlain) {
		t.Fatal("errors.As should NOT match a non-MySQLError")
	}
}
