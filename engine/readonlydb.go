package engine

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cleat-team/cleat/plugin"
)

// ReadOnlyDB wraps *sql.DB and implements plugin.PluginDB by enforcing
// read-only access. Write operations return an error.
//
// Dialect carries the same meaning as on SQLDBAdapter: statements are put
// through plugin.Rebind on the way to the driver. A read-only plugin has the
// identical portability problem -- a SELECT with $N placeholders fails on
// MySQL and SQL Server exactly as an UPDATE does -- so leaving this one out
// would fix the writes and quietly keep the reads broken.
//
// WHAT IS ACTUALLY ENFORCED, AND WHERE. Exec is refused in Go on every
// dialect. The DATABASE additionally refuses a write inside the transaction on
// PostgreSQL and MySQL, and cannot on SQL Server, which has no read-only
// transaction -- see readOnlyTxOptions. So do not read "Begin returned a
// transaction" as "the database is enforcing read-only": on SQL Server it is
// not, and a mutating statement sent through Query reaches the database there.
//
// That gap is cleat#1621 and is wider than SQL Server: Query and QueryRow fall
// through to the bare pool whenever beginTenantTx declines, which includes
// PostgreSQL with no tenant in context.
type ReadOnlyDB struct {
	Inner   *sql.DB
	Dialect plugin.Dialect
}

var _ plugin.PluginDB = (*ReadOnlyDB)(nil)

// readOnlyTxOptions returns the options that make the DATABASE enforce
// read-only for this dialect, or nil where it has no way to.
//
// PostgreSQL and MySQL both refuse a write inside a transaction opened with
// ReadOnly: true, with no further statement needed -- measured on PostgreSQL
// 16 and MySQL 8.4 as SQLSTATE 25006 and error 1792. SQL Server has no
// read-only transaction at all: go-mssqldb rejects the option outright
// ("read-only transactions are not supported") and T-SQL has no statement
// that makes an open transaction read-only.
//
// So the guarantee is NOT uniform, and the difference is deliberate rather
// than overlooked. On SQL Server a ReadOnlyDB transaction is enforced only by
// readOnlyTx.Exec refusing in Go. That still covers every write route
// plugin.PluginTx offers, but it is a weaker thing than the other two: a
// mutating statement sent through Query -- which denies nothing -- reaches
// the database on SQL Server and is refused on the others. cleat#1615.
func readOnlyTxOptions(d plugin.Dialect) *sql.TxOptions {
	if d == plugin.DialectMSSQL {
		return nil
	}
	return &sql.TxOptions{ReadOnly: true}
}

func (r *ReadOnlyDB) Begin(ctx context.Context) (plugin.PluginTx, error) {
	opts := readOnlyTxOptions(r.Dialect)
	tx, err := beginTenantTx(ctx, r.Inner, r.Dialect, opts)
	if err != nil {
		return nil, fmt.Errorf("readOnlyDB begin tenant-scoped tx: %w", err)
	}
	if tx == nil {
		if tx, err = r.Inner.BeginTx(ctx, opts); err != nil {
			return nil, fmt.Errorf("readOnlyDB begin tx: %w", err)
		}
	}
	// No "SET TRANSACTION READ ONLY" here. It was redundant on PostgreSQL --
	// the option above already does it -- FATAL on MySQL, which refuses to
	// change transaction characteristics once a transaction is open (error
	// 1568, SQLSTATE 25001), and a syntax error on SQL Server. So the one
	// statement that was meant to make this portable was the only thing
	// stopping Begin working on two of the three dialects.
	return &readOnlyTx{tx: tx, dialect: r.Dialect}, nil
}

func (r *ReadOnlyDB) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	return 0, fmt.Errorf("read-only: Exec denied")
}

func (r *ReadOnlyDB) Query(ctx context.Context, query string, args ...any) (plugin.Rows, error) {
	tx, err := beginTenantTx(ctx, r.Inner, r.Dialect, readOnlyTxOptions(r.Dialect))
	if err != nil {
		return nil, err
	}
	if tx == nil {
		rows, err := r.Inner.QueryContext(ctx, plugin.Rebind(query, r.Dialect), args...)
		if err != nil {
			return nil, err
		}
		return &sqlRowsWrapper{rows: rows}, nil
	}
	rows, err := tx.QueryContext(ctx, plugin.Rebind(query, r.Dialect), args...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return &sqlRowsWrapper{rows: rows, done: tx.Commit}, nil
}

func (r *ReadOnlyDB) QueryRow(ctx context.Context, query string, args ...any) plugin.RowScanner {
	tx, err := beginTenantTx(ctx, r.Inner, r.Dialect, readOnlyTxOptions(r.Dialect))
	if err != nil {
		return &rowScanner{err: err}
	}
	if tx == nil {
		row := r.Inner.QueryRowContext(ctx, plugin.Rebind(query, r.Dialect), args...)
		return &rowScanner{row: row}
	}
	row := tx.QueryRowContext(ctx, plugin.Rebind(query, r.Dialect), args...)
	return &rowScanner{row: row, done: tx.Commit}
}

func (r *ReadOnlyDB) Ping(ctx context.Context) error {
	return r.Inner.PingContext(ctx)
}

type readOnlyTx struct {
	tx      *sql.Tx
	dialect plugin.Dialect
}

var _ plugin.PluginTx = (*readOnlyTx)(nil)

func (r *readOnlyTx) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	return 0, fmt.Errorf("read-only: Exec denied")
}

func (r *readOnlyTx) Query(ctx context.Context, query string, args ...any) (plugin.Rows, error) {
	rows, err := r.tx.QueryContext(ctx, plugin.Rebind(query, r.dialect), args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsWrapper{rows: rows}, nil
}

func (r *readOnlyTx) QueryRow(ctx context.Context, query string, args ...any) plugin.RowScanner {
	row := r.tx.QueryRowContext(ctx, plugin.Rebind(query, r.dialect), args...)
	return &rowScanner{row: row}
}

func (r *readOnlyTx) Commit() error   { return r.tx.Commit() }
func (r *readOnlyTx) Rollback() error { return r.tx.Rollback() }

// sqlRowsWrapper wraps *sql.Rows to implement plugin.Rows.
type sqlRowsWrapper struct {
	rows *sql.Rows

	// done, when set, ends the transaction the rows were read on. A
	// tenant-scoped Query (see SQLDBAdapter.tenantTx) must keep its
	// transaction open until the caller has finished reading, because
	// set_config's is_local scope ends with the transaction -- committing
	// before Close would pull the policy context out from under the rows.
	done func() error
}

var _ plugin.Rows = (*sqlRowsWrapper)(nil)

// Next ends the transaction as soon as the rows are exhausted, rather than
// waiting for Close.
//
// Callers should defer Close and do; the difference matters for one that does
// not. Before this change, forgetting Close leaked a connection back to the
// pool late. With a tenant-scoped Query it would hold an open transaction
// as well, which is a worse thing to leak -- so the common exit path ends it
// without relying on the caller.
//
// Close remains correct after this: endTx clears the hook, and a second call
// does nothing.
func (w *sqlRowsWrapper) Next() bool {
	if w.rows.Next() {
		return true
	}
	_ = w.endTx()
	return false
}

// endTx runs the transaction hook at most once.
func (w *sqlRowsWrapper) endTx() error {
	if w.done == nil {
		return nil
	}
	done := w.done
	w.done = nil
	return done()
}
func (w *sqlRowsWrapper) Scan(dest ...any) error { return w.rows.Scan(dest...) }
func (w *sqlRowsWrapper) Close() error {
	err := w.rows.Close()
	if derr := w.endTx(); err == nil {
		err = derr
	}
	return err
}
func (w *sqlRowsWrapper) Err() error { return w.rows.Err() }

// rowScanner adapts *sql.Row to plugin.RowScanner.
type rowScanner struct {
	row *sql.Row

	// err, when set, is returned instead of scanning -- QueryRow has no
	// error return of its own, so a failure to open the tenant-scoped
	// transaction has to be carried to the caller's Scan.
	err error

	// done ends the transaction the row was read on. See sqlRowsWrapper.
	done func() error
}

var _ plugin.RowScanner = (*rowScanner)(nil)

func (r *rowScanner) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	err := r.row.Scan(dest...)
	if r.done != nil {
		if derr := r.done(); err == nil {
			err = derr
		}
		r.done = nil
	}
	return err
}
