package host

import (
	"context"
	"database/sql"
	"fmt"
)

// ReadOnlyDB wraps *sql.DB and ensures all transactions are READ ONLY.
// This provides a defense-in-depth mechanism for plugins that are only
// granted DatabaseAccessReadOnly.
type ReadOnlyDB struct {
	Inner *sql.DB
}

func (r *ReadOnlyDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	if opts == nil {
		opts = &sql.TxOptions{}
	}
	opts.ReadOnly = true
	tx, err := r.Inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("readOnlyDB begin tx: %w", err)
	}
	// Also set the session-level read_only to catch any statement that
	// bypasses the TxOptions (e.g., multi-statement queries).
	if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("readOnlyDB set transaction read only: %w", err)
	}
	return tx, nil
}

func (r *ReadOnlyDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	// For single-statement writes, wrap in a read-only transaction.
	tx, err := r.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

func (r *ReadOnlyDB) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return r.Inner.QueryContext(ctx, query, args...)
}

func (r *ReadOnlyDB) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return r.Inner.QueryRowContext(ctx, query, args...)
}

func (r *ReadOnlyDB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return r.Inner.PrepareContext(ctx, query)
}

func (r *ReadOnlyDB) Close() error {
	return r.Inner.Close()
}

func (r *ReadOnlyDB) PingContext(ctx context.Context) error {
	return r.Inner.PingContext(ctx)
}

// Exec implements the deprecated sql.DB.Exec by routing through ExecContext
// so that the read-only enforcement applies.
func (r *ReadOnlyDB) Exec(query string, args ...interface{}) (sql.Result, error) {
	return r.ExecContext(context.Background(), query, args...)
}

// Query implements the deprecated sql.DB.Query by routing through QueryContext.
func (r *ReadOnlyDB) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return r.QueryContext(context.Background(), query, args...)
}

// QueryRow implements the deprecated sql.DB.QueryRow by routing through QueryRowContext.
func (r *ReadOnlyDB) QueryRow(query string, args ...interface{}) *sql.Row {
	return r.QueryRowContext(context.Background(), query, args...)
}
