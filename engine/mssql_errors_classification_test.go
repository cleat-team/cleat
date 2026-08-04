package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// The existing tests in mssql_errors_test.go feed only fmt.Errorf strings --
// never a real mssql.Error. That is why they all passed against a classifier
// that matched bare error numbers as substrings: the tests and the
// implementation shared the same wrong model, that a SQL Server error is text.
//
// These tests drive the type the driver actually returns, and pin the
// misclassifications the substring approach produced.

// numberedErr builds the error the go-mssqldb driver returns for a server-side
// error, so classification is exercised against the real shape.
func numberedErr(number int32, msg string) error {
	return mssql.Error{Number: number, Message: msg}
}

func TestMSSQLClassifyByErrorNumber(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		deadlock  bool
		duplicate bool
		snapshot  bool
		timeout   bool
		retryable bool
	}{
		{
			name:      "1205 deadlock victim",
			err:       numberedErr(1205, "Transaction was chosen as the deadlock victim."),
			deadlock:  true,
			retryable: true,
		},
		{
			name:      "2627 unique constraint",
			err:       numberedErr(2627, "Violation of UNIQUE KEY constraint."),
			duplicate: true,
		},
		{
			name:      "2601 duplicate key row",
			err:       numberedErr(2601, "Cannot insert duplicate key row in object."),
			duplicate: true,
		},
		{
			name:      "3960 snapshot conflict",
			err:       numberedErr(3960, "Snapshot isolation transaction aborted due to update conflict."),
			snapshot:  true,
			retryable: true,
		},
		{
			name:      "41302 in-memory OLTP write conflict",
			err:       numberedErr(41302, "The current transaction attempted to update a record."),
			snapshot:  true,
			retryable: true,
		},
		{
			name:      "258 wait operation timed out",
			err:       numberedErr(258, "Wait operation timed out."),
			timeout:   true,
			retryable: true,
		},
		{
			// A permanent, non-retryable server error must not be swept up.
			name: "8134 divide by zero is permanent",
			err:  numberedErr(8134, "Divide by zero error encountered."),
		},
		{
			name: "207 invalid column name is permanent",
			err:  numberedErr(207, "Invalid column name 'nope'."),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMSSQLDeadlock(tt.err); got != tt.deadlock {
				t.Errorf("isMSSQLDeadlock = %v, want %v", got, tt.deadlock)
			}
			if got := isMSSQLDuplicateKey(tt.err); got != tt.duplicate {
				t.Errorf("isMSSQLDuplicateKey = %v, want %v", got, tt.duplicate)
			}
			if got := isMSSQLSnapshotError(tt.err); got != tt.snapshot {
				t.Errorf("isMSSQLSnapshotError = %v, want %v", got, tt.snapshot)
			}
			if got := isMSSQLTimeout(tt.err); got != tt.timeout {
				t.Errorf("isMSSQLTimeout = %v, want %v", got, tt.timeout)
			}
			if got := isMSSQLRetryable(tt.err); got != tt.retryable {
				t.Errorf("isMSSQLRetryable = %v, want %v", got, tt.retryable)
			}
		})
	}
}

// TestMSSQLClassifyDoesNotMatchNumbersInText is the regression test for the
// defect. Each error below contains a SQL Server error number as a *substring*
// of unrelated content -- a workflow ID, a row number, a column name, a
// business value -- and none of them is the error that number denotes.
//
// The previous classifier returned true for every one of these. The first two
// are the damaging direction: a permanent failure classified as transient is
// retried until the budget is exhausted, turning a clear error into a slow one
// and burning the retry budget that a real deadlock needed.
func TestMSSQLClassifyDoesNotMatchNumbersInText(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"workflow id containing 258", fmt.Errorf(`permission denied for workflow "wf-2589abc"`)},
		{"column name containing 3960", fmt.Errorf(`Invalid column name 'col3960'.`)},
		{"row number containing 2627", fmt.Errorf(`invalid column value at row 26270`)},
		{"business value containing 2601", fmt.Errorf(`workflow input rejected: amount 2601 exceeds limit`)},
		{"id containing 1205", fmt.Errorf(`no such workflow: run-1205e4`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if isMSSQLRetryable(tt.err) {
				t.Errorf("classified as retryable: %v", tt.err)
			}
			if isMSSQLDuplicateKey(tt.err) {
				t.Errorf("classified as duplicate key: %v", tt.err)
			}
		})
	}
}

// TestMSSQLConnectionErrorIsNotAnyMentionOfConnection pins the other overly
// broad match: the classifier used strings.Contains(msg, "connection"), so a
// malformed connection string -- a permanent configuration error that will fail
// identically on every attempt -- was retryable.
func TestMSSQLConnectionErrorIsNotAnyMentionOfConnection(t *testing.T) {
	permanent := []error{
		fmt.Errorf("invalid connection string: missing database"),
		fmt.Errorf("unsupported connection option 'foo'"),
	}
	for _, err := range permanent {
		if isMSSQLConnectionError(err) {
			t.Errorf("classified as a connection error: %v", err)
		}
		if isMSSQLRetryable(err) {
			t.Errorf("classified as retryable: %v", err)
		}
	}

	// Genuine transport failures still classify, via the error type rather
	// than the wording.
	transient := []error{
		driver.ErrBadConn,
		io.ErrUnexpectedEOF,
		&net.OpError{Op: "read", Err: errors.New("reset")},
		fmt.Errorf("wrapped: %w", driver.ErrBadConn),
		fmt.Errorf("write tcp: broken pipe"),
	}
	for _, err := range transient {
		if !isMSSQLConnectionError(err) {
			t.Errorf("not classified as a connection error: %v", err)
		}
	}
}

// TestMSSQLCancelledIsNotRetryable: cancellation is a decision, not a fault.
// Retrying it works against the caller that asked to stop.
func TestMSSQLCancelledIsNotRetryable(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		fmt.Errorf("query failed: %w", context.Canceled),
	} {
		if isMSSQLRetryable(err) {
			t.Errorf("context.Canceled classified as retryable: %v", err)
		}
	}
	// A deadline, by contrast, is a timeout and stays retryable.
	if !isMSSQLRetryable(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded should be retryable")
	}
}

// TestMSSQLRollbackGuaranteedIsNarrowerThanRetryable records the distinction a
// caller has to make before wrapping a transaction in mssqlRetry.
//
// A deadlock or snapshot conflict guarantees the server rolled the transaction
// back, so replaying it is sound even when the work is not idempotent. A
// timeout or a dropped connection leaves the outcome *unknown* -- the commit
// may have succeeded with only the acknowledgement lost -- so a blind replay
// can double-apply. See IMPROVEMENT-PLAN §2.26.
func TestMSSQLRollbackGuaranteedIsNarrowerThanRetryable(t *testing.T) {
	rollbackGuaranteed := []error{
		numberedErr(1205, "deadlock victim"),
		numberedErr(3960, "update conflict"),
	}
	for _, err := range rollbackGuaranteed {
		if !isMSSQLRollbackGuaranteed(err) {
			t.Errorf("want rollback guaranteed: %v", err)
		}
		if !isMSSQLRetryable(err) {
			t.Errorf("want retryable: %v", err)
		}
	}

	outcomeUnknown := []error{
		numberedErr(258, "Wait operation timed out."),
		driver.ErrBadConn,
		context.DeadlineExceeded,
	}
	for _, err := range outcomeUnknown {
		if isMSSQLRollbackGuaranteed(err) {
			t.Errorf("rollback is NOT guaranteed for this error: %v", err)
		}
		if !isMSSQLRetryable(err) {
			t.Errorf("want retryable: %v", err)
		}
	}
}
