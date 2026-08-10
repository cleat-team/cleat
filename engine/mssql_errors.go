package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

// SQL Server error numbers this package classifies. Matching these as numbers
// rather than as substrings of the error text is the whole point of
// mssqlErrNumber below -- see the comment there.
const (
	mssqlErrDeadlockVictim   = 1205 // chosen as the deadlock victim
	mssqlErrDuplicateKeyObj  = 2601 // cannot insert duplicate key row (unique index)
	mssqlErrUniqueConstraint = 2627 // violation of UNIQUE/PRIMARY KEY constraint
	mssqlErrSnapshotConflict = 3960 // snapshot isolation update conflict
	mssqlErrTimeout          = 258  // wait operation timed out
)

// In-memory OLTP (Hekaton) reports its write conflicts with its own numbers
// rather than 3960. They mean the same thing for our purposes: the transaction
// was rolled back and may be retried.
var mssqlSnapshotConflictNumbers = []int32{
	mssqlErrSnapshotConflict,
	41301, // dependency failure
	41302, // updated a record that was updated since this transaction started
	41305, // repeatable read validation failure
	41325, // serializable validation failure
}

// mssqlErrNumber extracts the SQL Server error number, if the error carries one.
//
// This exists because classifying on the *text* of the error is unsound. The
// original implementation did `strings.Contains(msg, "258")` and friends, which
// matches those digits anywhere in the message -- including in workflow IDs,
// row numbers, column names, and business data that the driver interpolates
// into the error. Verified misclassifications from that approach:
//
//	permission denied for workflow "wf-2589abc"     -> "timeout"   -> RETRYABLE
//	column "col3960" does not exist                 -> "snapshot"  -> RETRYABLE
//	invalid column value at row 26270               -> "duplicate key"
//	workflow input rejected: amount 2601 exceeds…   -> "duplicate key"
//
// The first two are the dangerous ones: a permanent failure classified as
// transient is retried until the budget is exhausted, turning a clear error
// into a slow one.
func mssqlErrNumber(err error) (int32, bool) {
	var e mssql.Error
	if errors.As(err, &e) {
		return e.Number, true
	}
	return 0, false
}

// hasNumber reports whether err carries any of the given SQL Server error numbers.
func hasNumber(err error, numbers ...int32) bool {
	n, ok := mssqlErrNumber(err)
	if !ok {
		return false
	}
	for _, want := range numbers {
		if n == want {
			return true
		}
	}
	return false
}

// containsAny reports whether msg contains any of the given phrases,
// case-insensitively.
//
// Every phrase passed here must be *distinctive* -- a bare error number or a
// common word like "connection" or "duplicate" will match unrelated errors.
// These fallbacks exist only for errors that reached us as plain text, having
// lost the mssql.Error type on the way through a wrapper.
func containsAny(msg string, phrases ...string) bool {
	lower := strings.ToLower(msg)
	for _, p := range phrases {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// isMSSQLDeadlock checks for SQL Server deadlock (error 1205).
func isMSSQLDeadlock(err error) bool {
	if err == nil {
		return false
	}
	if hasNumber(err, mssqlErrDeadlockVictim) {
		return true
	}
	return containsAny(err.Error(), "deadlock victim", "was deadlocked", "deadlocked on lock")
}

// isMSSQLDuplicateKey checks for unique constraint violations (errors 2627, 2601).
func isMSSQLDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if hasNumber(err, mssqlErrUniqueConstraint, mssqlErrDuplicateKeyObj) {
		return true
	}
	return containsAny(err.Error(),
		"cannot insert duplicate key",
		"duplicate key row",
		"unique key constraint",
		"primary key constraint",
		"unique index",
	)
}

// isMSSQLSnapshotError checks for snapshot isolation write conflicts (error
// 3960, plus the in-memory OLTP equivalents).
func isMSSQLSnapshotError(err error) bool {
	if err == nil {
		return false
	}
	if hasNumber(err, mssqlSnapshotConflictNumbers...) {
		return true
	}
	return containsAny(err.Error(), "snapshot isolation", "update conflict")
}

// isMSSQLTimeout returns true if the error is a timeout or a deadline.
// Handles context deadlines, SQL Server error 258, and network timeouts.
//
// context.Canceled is deliberately excluded: cancellation is a decision, not a
// transient fault, and retrying it would ignore the caller.
func isMSSQLTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if hasNumber(err, mssqlErrTimeout) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return containsAny(err.Error(), "timeout expired", "timed out", "query timeout", "i/o timeout")
}

// isMSSQLConnectionError checks for network-level errors that may be transient.
func isMSSQLConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// driver.ErrBadConn is database/sql's own signal that the connection died.
	if errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return containsAny(err.Error(),
		"connection reset",
		"connection refused",
		"connection closed",
		"broken pipe",
		"no such host",
		"unreachable",
		"tls handshake",
		"transport endpoint is not connected",
		"server does not support",
	)
}

// isMSSQLRetryable returns true if the error is transient and the operation
// should be retried. Covers deadlocks, snapshot conflicts, timeouts, and
// connection failures.
//
// Note for callers: a *retryable* error is not automatically a *safe-to-retry*
// operation. Deadlocks and snapshot conflicts guarantee the transaction was
// rolled back, so replaying it is sound. A timeout or a dropped connection
// leaves the transaction's fate unknown -- the commit may have succeeded with
// only the acknowledgement lost -- so replaying a non-idempotent statement can
// double-apply it. See IMPROVEMENT-PLAN §2.26.
func isMSSQLRetryable(err error) bool {
	if err == nil {
		return false
	}
	return isMSSQLDeadlock(err) ||
		isMSSQLSnapshotError(err) ||
		isMSSQLTimeout(err) ||
		isMSSQLConnectionError(err)
}

// isMSSQLRollbackGuaranteed reports whether the error guarantees the server
// rolled the transaction back, which is what makes an unconditional retry safe
// even for non-idempotent work.
func isMSSQLRollbackGuaranteed(err error) bool {
	if err == nil {
		return false
	}
	return isMSSQLDeadlock(err) || isMSSQLSnapshotError(err)
}

// mapMSSQLError maps a SQL Server error to a standard CleatError.
// Deadlocks and transient errors -> ErrTransient (retryable).
// Duplicate keys -> ErrCancelled (idempotent).
// Other errors -> ErrPermanent.
func mapMSSQLError(op, workflowID string, err error) error {
	if err == nil {
		return nil
	}
	if isMSSQLRetryable(err) {
		return NewTransientError(op, workflowID, err)
	}
	if isMSSQLDuplicateKey(err) {
		return NewCancelledError(op, workflowID, err)
	}
	return NewPermanentError(op, workflowID, err)
}

// MSSQLConnectionString builds a SQL Server connection string.
// Format: sqlserver://user:pass@host:port?database=dbname&connection+timeout=30
func MSSQLConnectionString(host string, port int, user, password, database string) string {
	return fmt.Sprintf("sqlserver://%s:%s@%s:%d?database=%s&connection+timeout=30&encrypt=false",
		user, password, host, port, database)
}
