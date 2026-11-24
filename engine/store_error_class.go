package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/lib/pq"
)

// StoreErrorIsTransient reports whether a failed store call is one a client
// can reasonably send again a moment later: a deadlock or serialization
// failure the server rolled back, a lock wait that timed out, a connection the
// pool dropped or could not make, or a context that ran out of time.
//
// The HTTP layer uses it to choose between 503 (retry) and 500 (do not) for an
// error it will not describe to the caller (cleat#3878). It is deliberately the
// narrow answer: an error it does not recognise is NOT transient. That is the
// opposite default from errIsRetryable, whose closing clause retries anything
// it cannot name, because that function retries INSIDE the engine where a
// wrong guess costs a few milliseconds; a wrong 503 here tells every client to
// hammer an endpoint that is failing for a reason a retry will not change.
//
// Deadlock and lock-timeout recognition reuses the per-dialect classifiers the
// stores already have rather than matching text a second time. A closed pool
// is not transient: nothing will reopen it for the next attempt.
func StoreErrorIsTransient(err error) bool {
	if err == nil || errPoolClosed(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		code := string(pqErr.Code)
		switch {
		case code == "40001", code == "40P01", code == "55P03":
			// serialization failure, deadlock detected, lock not available
			return true
		case strings.HasPrefix(code, "08"), strings.HasPrefix(code, "53"), strings.HasPrefix(code, "57P"):
			// connection exception, insufficient resources (too many connections),
			// operator intervention (shutdown, crash recovery)
			return true
		}
		return false
	}
	return isDeadlockError(err) || isLockWaitTimeout(err) || isMSSQLDeadlock(err) || isMSSQLLockTimeout(err) || isMSSQLSnapshotError(err)
}
