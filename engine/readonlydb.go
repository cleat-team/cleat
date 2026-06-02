package engine

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cleat-team/cleat/plugin"
)

// ReadOnlyDB wraps *sql.DB and implements plugin.PluginDB by enforcing
// read-only access. Write operations return an error.
type ReadOnlyDB struct {
	Inner *sql.DB
}

var _ plugin.PluginDB = (*ReadOnlyDB)(nil)

func (r *ReadOnlyDB) Begin(ctx context.Context) (plugin.PluginTx, error) {
	tx, err := r.Inner.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("readOnlyDB begin tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("readOnlyDB set transaction read only: %w", err)
	}
	return &readOnlyTx{tx: tx}, nil
}

func (r *ReadOnlyDB) Exec(ctx context.Context, query string, args ...interface{}) (int64, error) {
	return 0, fmt.Errorf("read-only: Exec denied")
}

func (r *ReadOnlyDB) Query(ctx context.Context, query string, args ...interface{}) (plugin.Rows, error) {
	rows, err := r.Inner.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsWrapper{rows: rows}, nil
}

func (r *ReadOnlyDB) QueryRow(ctx context.Context, query string, args ...interface{}) plugin.RowScanner {
	row := r.Inner.QueryRowContext(ctx, query, args...)
	return &rowScanner{row: row}
}

func (r *ReadOnlyDB) Ping(ctx context.Context) error {
	return r.Inner.PingContext(ctx)
}

type readOnlyTx struct {
	tx *sql.Tx
}

var _ plugin.PluginTx = (*readOnlyTx)(nil)

func (r *readOnlyTx) Exec(ctx context.Context, query string, args ...interface{}) (int64, error) {
	return 0, fmt.Errorf("read-only: Exec denied")
}

func (r *readOnlyTx) Query(ctx context.Context, query string, args ...interface{}) (plugin.Rows, error) {
	rows, err := r.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsWrapper{rows: rows}, nil
}

func (r *readOnlyTx) QueryRow(ctx context.Context, query string, args ...interface{}) plugin.RowScanner {
	row := r.tx.QueryRowContext(ctx, query, args...)
	return &rowScanner{row: row}
}

func (r *readOnlyTx) Commit() error   { return r.tx.Commit() }
func (r *readOnlyTx) Rollback() error { return r.tx.Rollback() }

// sqlRowsWrapper wraps *sql.Rows to implement plugin.Rows.
type sqlRowsWrapper struct {
	rows *sql.Rows
}

var _ plugin.Rows = (*sqlRowsWrapper)(nil)

func (w *sqlRowsWrapper) Next() bool                    { return w.rows.Next() }
func (w *sqlRowsWrapper) Scan(dest ...interface{}) error { return w.rows.Scan(dest...) }
func (w *sqlRowsWrapper) Close() error                   { return w.rows.Close() }
func (w *sqlRowsWrapper) Err() error                     { return w.rows.Err() }

// rowScanner adapts *sql.Row to plugin.RowScanner.
type rowScanner struct {
	row *sql.Row
}

var _ plugin.RowScanner = (*rowScanner)(nil)

func (r *rowScanner) Scan(dest ...interface{}) error {
	return r.row.Scan(dest...)
}
