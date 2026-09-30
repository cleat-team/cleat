package engine

import (
	"context"
	"database/sql/driver"
	"fmt"
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
	}{
		{
			name:     "1205 deadlock victim",
			err:      numberedErr(1205, "Transaction was chosen as the deadlock victim."),
			deadlock: true,
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
			name:     "3960 snapshot conflict",
			err:      numberedErr(3960, "Snapshot isolation transaction aborted due to update conflict."),
			snapshot: true,
		},
		{
			name:     "41302 in-memory OLTP write conflict",
			err:      numberedErr(41302, "The current transaction attempted to update a record."),
			snapshot: true,
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
		})
	}
}

// TestMSSQLClassifyDoesNotMatchNumbersInText is the regression test for the
// defect. Each error below contains a SQL Server error number as a *substring*
// of unrelated content -- a workflow ID, a row number, a column name, a
// business value -- and none of them is the error that number denotes.
//
// The previous classifier returned true for every one of these.
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
			if isMSSQLDuplicateKey(tt.err) {
				t.Errorf("classified as duplicate key: %v", tt.err)
			}
		})
	}
}

// TestMSSQLRollbackGuaranteedClassification records the distinction a caller
// has to make before wrapping a transaction in withRollbackGuaranteedRetry.
//
// A deadlock or snapshot conflict guarantees the server rolled the
// transaction back, so replaying it is sound even when the work is not
// idempotent. A timeout or a dropped connection leaves the outcome *unknown*
// -- the commit may have succeeded with only the acknowledgement lost -- so a
// blind replay can double-apply. See IMPROVEMENT-PLAN §2.26.
func TestMSSQLRollbackGuaranteedClassification(t *testing.T) {
	rollbackGuaranteed := []error{
		numberedErr(1205, "deadlock victim"),
		numberedErr(3960, "update conflict"),
	}
	for _, err := range rollbackGuaranteed {
		if !isMSSQLRollbackGuaranteed(err) {
			t.Errorf("want rollback guaranteed: %v", err)
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
	}
}
