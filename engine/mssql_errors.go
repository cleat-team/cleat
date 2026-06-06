package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	// Register the "sqlserver" driver with database/sql.
	_ "github.com/microsoft/go-mssqldb"
)

// isMSSQLDeadlock checks for SQL Server deadlock error (error 1205).
func isMSSQLDeadlock(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "deadlock") ||
		strings.Contains(msg, "was chosen as the deadlock victim")
}

// isMSSQLDuplicateKey checks for unique constraint violations (errors 2627, 2601).
func isMSSQLDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "2627") ||
		strings.Contains(msg, "2601") ||
		strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "UNIQUE KEY constraint") ||
		strings.Contains(msg, "PRIMARY KEY constraint") ||
		strings.Contains(msg, "Cannot insert duplicate key")
}

// isMSSQLSnapshotError checks for snapshot isolation failures (error 3960).
func isMSSQLSnapshotError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "3960") ||
		strings.Contains(msg, "snapshot isolation") ||
		strings.Contains(msg, "update conflict")
}

// isMSSQLTimeout returns true if the error is a timeout or connection deadline.
// Handles context deadlines, SQL Server timeouts (error 258), and network timeouts.
func isMSSQLTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "258") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection refused")
}

// isMSSQLRetryable returns true if the error is transient and the operation
// should be retried. Covers deadlocks, snapshot conflicts, timeouts, and
// connection failures.
func isMSSQLRetryable(err error) bool {
	if err == nil {
		return false
	}
	return isMSSQLDeadlock(err) ||
		isMSSQLSnapshotError(err) ||
		isMSSQLTimeout(err) ||
		isMSSQLConnectionError(err)
}

// isMSSQLConnectionError checks for network-level errors that may be transient.
func isMSSQLConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection") ||
		strings.Contains(msg, "TLS") ||
		strings.Contains(msg, "transport") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unreachable")
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
