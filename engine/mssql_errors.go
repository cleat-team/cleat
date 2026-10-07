package engine

import (
	"errors"
	"fmt"
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
	mssqlErrLockTimeout      = 1222 // lock request time out period exceeded (SET LOCK_TIMEOUT)
	mssqlErrForeignKeyRef    = 547  // the INSERT/UPDATE/DELETE statement conflicted with a constraint
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

// isMSSQLSignalsWorkflowFKViolation checks for the FK 547 conflict on
// fk_signals_workflow specifically -- the FK workflow_signals.workflow_id
// holds on workflow_instances(id) (migrations/mssql/001_schema.sql). 547 is
// SQL Server's generic "the INSERT/UPDATE/DELETE statement conflicted with a
// constraint" error, shared by every FOREIGN KEY and CHECK constraint in the
// database, so a bare error-547 check would also match an unrelated
// CHECK-constraint failure; the constraint's name is asserted in the
// message text (SQL Server always includes it) rather than trusted from the
// error number alone.
//
// deliverSignalTx's EXISTS-gated INSERT (mssql_signals_promises.go) reads
// workflow_instances as a plain, non-locking SELECT: the EXISTS predicate
// can be true when it is evaluated and false by the time the INSERT's own
// FK check runs a moment later, if the target is hard-deleted in between --
// a purge racing a signal, in the caller's own tenant. When that race is
// lost, the INSERT fails HERE instead of the EXISTS predicate simply being
// false, and without this check the raw FK error would reach the caller
// unwrapped, indistinguishable from an unrelated database failure and
// invisible to a caller checking errors.Is(err, ErrWorkflowNotFound) --
// including eventtriggers.signalAwaiters, which unregisters on that
// specifically and would otherwise treat this race as a transient failure
// and retry it forever against a workflow that is never coming back.
func isMSSQLSignalsWorkflowFKViolation(err error) bool {
	if err == nil {
		return false
	}
	if !hasNumber(err, mssqlErrForeignKeyRef) {
		return false
	}
	return containsAny(err.Error(), "fk_signals_workflow")
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

// isMSSQLLockTimeout checks for SET LOCK_TIMEOUT's own error (1222), distinct
// from a client/network wait timing out (error 258, which this package no
// longer classifies -- see cleat#2792): 1222 is the server refusing to wait
// past a session's own configured bound on a lock request. Deliberately not
// treated as rollback-guaranteed -- the caller that sets a lock timeout
// (claim Step 4, cleat#1963) wants to treat this as "no claim this round"
// and let its own poll loop retry later, not as a transaction to replay
// immediately against a lock that is probably still held.
func isMSSQLLockTimeout(err error) bool {
	if err == nil {
		return false
	}
	if hasNumber(err, mssqlErrLockTimeout) {
		return true
	}
	return containsAny(err.Error(), "lock request time out period exceeded")
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

// MSSQLConnectionString builds a SQL Server connection string.
// Format: sqlserver://user:pass@host:port?database=dbname&connection+timeout=30
func MSSQLConnectionString(host string, port int, user, password, database string) string {
	return fmt.Sprintf("sqlserver://%s:%s@%s:%d?database=%s&connection+timeout=30&encrypt=false",
		user, password, host, port, database)
}
