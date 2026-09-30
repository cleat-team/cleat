package engine

// cleat#2805: no dialect ever wrapped a DB-originated FinalizeWorkflowSegment
// failure into a *CleatError, so cmd/cleat-worker's errors.As(err, &ce) --
// the only thing that ever populates error_code away from its zero value --
// always missed, and every DB-originated terminal workflow failure recorded
// error_code=ErrUnknown regardless of dialect or actual cause.
//
// Measured rather than assumed: before this change, engine/store_lifecycle.go,
// engine/mysql_lifecycle.go and engine/mssql_lifecycle.go's FinalizeWorkflowSegment
// methods returned wrapRejectedResult's result UNCHANGED -- wrapRejectedResult
// itself only classifies a store's JSON-value refusal (ErrResultRejected); a
// genuine driver error (deadlock, lock timeout, snapshot conflict, or anything
// else) passed through as a plain wrapped error, which is exactly what
// `var ce *CleatError; errors.As(err, &ce)` at the three cmd/cleat-worker call
// sites cannot see.
//
// wrapPostgresFinalizeDBError / wrapMySQLFinalizeDBError / wrapMSSQLFinalizeDBError
// (store_lifecycle.go, mysql_lifecycle.go, mssql_lifecycle.go) are the fix, each
// now called from its dialect's outer FinalizeWorkflowSegment. These tests
// exercise the classifiers directly with the REAL typed driver errors each one
// switches on -- *pq.Error.Code, *mysql.MySQLError.Number, mssql.Error.Number --
// not text, so a wording change in a driver's message cannot silently break the
// classification the way engine/mssql_errors.go's own doc comment describes
// substring matching having done historically.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	mssql "github.com/microsoft/go-mssqldb"
)

func TestWrapPostgresFinalizeDBError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode ErrorCode
		wantNil  bool
	}{
		{name: "nil passes through", err: nil, wantNil: true},
		{name: "ErrFenceLost passes through unclassified", err: ErrFenceLost, wantCode: ErrUnknown},
		{
			name:     "already a *CleatError is not re-wrapped",
			err:      &CleatError{Code: ErrResultRejected, Op: "finalize workflow", WorkflowID: "wf-1", Err: fmt.Errorf("refused")},
			wantCode: ErrResultRejected,
		},
		{name: "40P01 deadlock_detected -> transient", err: &pq.Error{Code: "40P01", Message: "deadlock detected"}, wantCode: ErrTransient},
		{name: "40001 serialization_failure -> transient", err: &pq.Error{Code: "40001", Message: "could not serialize access"}, wantCode: ErrTransient},
		{name: "context deadline exceeded -> transient", err: fmt.Errorf("query: %w", context.DeadlineExceeded), wantCode: ErrTransient},
		{name: "context canceled -> transient", err: fmt.Errorf("query: %w", context.Canceled), wantCode: ErrTransient},
		{name: "23505 unique_violation -> permanent", err: &pq.Error{Code: "23505", Message: "duplicate key"}, wantCode: ErrPermanent},
		{name: "unrecognized error -> permanent", err: fmt.Errorf("syntax error at or near \"SELCT\""), wantCode: ErrPermanent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapPostgresFinalizeDBError(tc.err, "wf-1")
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if tc.name == "ErrFenceLost passes through unclassified" {
				if !errors.Is(got, ErrFenceLost) {
					t.Fatalf("got %v, want ErrFenceLost unchanged", got)
				}
				var ce *CleatError
				if errors.As(got, &ce) {
					t.Fatalf("ErrFenceLost was wrapped into a *CleatError, want passthrough")
				}
				return
			}
			var ce *CleatError
			if !errors.As(got, &ce) {
				t.Fatalf("got %v (%T), want a *CleatError", got, got)
			}
			if ce.Code != tc.wantCode {
				t.Errorf("Code = %v, want %v", ce.Code, tc.wantCode)
			}
		})
	}
}

func TestWrapMySQLFinalizeDBError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode ErrorCode
		wantNil  bool
	}{
		{name: "nil passes through", err: nil, wantNil: true},
		{name: "1213 deadlock -> transient", err: &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}, wantCode: ErrTransient},
		{name: "1205 lock wait timeout -> transient", err: &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}, wantCode: ErrTransient},
		{
			name: "1213 still transient after finalizeWorkflowSegmentRetrying exhausts its own retries",
			err: fmt.Errorf("finalize workflow segment: %d attempts all lost a lock conflict: %w",
				8, &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}),
			wantCode: ErrTransient,
		},
		{name: "context deadline exceeded -> transient", err: fmt.Errorf("query: %w", context.DeadlineExceeded), wantCode: ErrTransient},
		{name: "1062 duplicate entry -> permanent", err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}, wantCode: ErrPermanent},
		{name: "unrecognized error -> permanent", err: fmt.Errorf("you have an error in your SQL syntax"), wantCode: ErrPermanent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapMySQLFinalizeDBError(tc.err, "wf-1")
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			var ce *CleatError
			if !errors.As(got, &ce) {
				t.Fatalf("got %v (%T), want a *CleatError", got, got)
			}
			if ce.Code != tc.wantCode {
				t.Errorf("Code = %v, want %v", ce.Code, tc.wantCode)
			}
		})
	}
}

func TestWrapMSSQLFinalizeDBError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode ErrorCode
		wantNil  bool
	}{
		{name: "nil passes through", err: nil, wantNil: true},
		{name: "1205 deadlock victim -> transient", err: mssql.Error{Number: mssqlErrDeadlockVictim, Message: "Transaction (Process ID 52) was deadlocked ... deadlock victim."}, wantCode: ErrTransient},
		{name: "3960 snapshot conflict -> transient", err: mssql.Error{Number: mssqlErrSnapshotConflict, Message: "Snapshot isolation transaction aborted due to update conflict"}, wantCode: ErrTransient},
		{name: "1222 lock request timeout -> transient", err: mssql.Error{Number: mssqlErrLockTimeout, Message: "Lock request time out period exceeded"}, wantCode: ErrTransient},
		{name: "context deadline exceeded -> transient", err: fmt.Errorf("query: %w", context.DeadlineExceeded), wantCode: ErrTransient},
		{name: "2627 unique constraint -> permanent", err: mssql.Error{Number: mssqlErrUniqueConstraint, Message: "Violation of PRIMARY KEY constraint"}, wantCode: ErrPermanent},
		{name: "unrecognized error -> permanent", err: mssql.Error{Number: 208, Message: "Invalid object name 'workflow_instances'."}, wantCode: ErrPermanent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapMSSQLFinalizeDBError(tc.err, "wf-1")
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			var ce *CleatError
			if !errors.As(got, &ce) {
				t.Fatalf("got %v (%T), want a *CleatError", got, got)
			}
			if ce.Code != tc.wantCode {
				t.Errorf("Code = %v, want %v", ce.Code, tc.wantCode)
			}
		})
	}
}
