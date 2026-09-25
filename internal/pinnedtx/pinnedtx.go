// Package pinnedtx starts a transaction on a session that may be one pinned
// *sql.Conn, without handing that connection to a goroutine the caller cannot
// see. cleat#2215.
//
// # The hazard
//
// database/sql starts a goroutine (Tx.awaitDone) for every transaction begun
// with a context that can be cancelled. When the context ends it rolls the
// transaction back and, unless the driver can reset a session, DISCARDS the
// connection: for a *sql.Conn that means Conn.close, which runs on that
// goroutine, concurrently with whatever the caller does next.
//
// A caller that pins a connection for a run -- the migration runners take their
// lock on one and RESET its session settings on the way out -- goes on
// using the *sql.Conn after the transaction failed. Conn.grabConn checks its
// done flag and only then takes the read lock, so a close that lands between
// the two leaves it holding a nil driver connection, and the next ExecContext
// dereferences it (database/sql.(*DB).execDC). It is a window of nanoseconds
// and needs the closer to run at that moment, which is why it appeared as an
// occasional panic under CI load and not on a workstation. The developer
// measurement is in the pull request for cleat#2215.
//
// # The rule
//
// On a pinned connection the transaction must not be ended by the run's
// context. Statements inside it still take that context, so a blocked
// statement is still cancelled client-side (lib/pq sends a cancel request) and
// the caller's own Rollback, which is synchronous, ends the transaction. What
// goes away is only the second, asynchronous, closer.
package pinnedtx

import (
	"context"
	"database/sql"
)

// Beginner is what Begin needs of a session: *sql.DB and *sql.Conn both
// satisfy it.
type Beginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// Begin starts a transaction on s.
//
// On a *sql.Conn the transaction is begun on a context that ctx cannot cancel,
// so database/sql has no reason to close the connection from another
// goroutine; see the package comment. A context that is already done still
// refuses the call, as it always did.
//
// On anything else (a *sql.DB) the transaction returns its connection to the
// pool when discarded, there is no Conn to close underneath the caller, and ctx
// is passed through unchanged. Every migration session is a pinned *sql.Conn on
// all three dialects now (the PostgreSQL advisory lock, and the MySQL and SQL
// Server named locks), so in the runners this is the path a test double or an
// out-of-tree caller takes, not the production one.
func Begin(ctx context.Context, s Beginner, opts *sql.TxOptions) (*sql.Tx, error) {
	if _, pinned := s.(*sql.Conn); !pinned {
		return s.BeginTx(ctx, opts)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.BeginTx(context.WithoutCancel(ctx), opts)
}
