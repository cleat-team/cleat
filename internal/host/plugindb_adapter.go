package host

import (
	"context"
	"database/sql"

	"github.com/rcownie/cleat/internal/plugin"
)

// SQLDBAdapter wraps *sql.DB and implements plugin.PluginDB with full
// read-write access. Used when a plugin declares DatabaseAccessReadWrite.
type SQLDBAdapter struct {
	DB *sql.DB
}

var _ plugin.PluginDB = (*SQLDBAdapter)(nil)

func (a *SQLDBAdapter) Begin(ctx context.Context) (plugin.PluginTx, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &sqlTxAdapter{tx: tx}, nil
}

func (a *SQLDBAdapter) Exec(ctx context.Context, query string, args ...interface{}) (int64, error) {
	result, err := a.DB.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (a *SQLDBAdapter) Query(ctx context.Context, query string, args ...interface{}) (plugin.Rows, error) {
	rows, err := a.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsWrapper{rows: rows}, nil
}

func (a *SQLDBAdapter) QueryRow(ctx context.Context, query string, args ...interface{}) plugin.RowScanner {
	row := a.DB.QueryRowContext(ctx, query, args...)
	return &rowScanner{row: row}
}

func (a *SQLDBAdapter) Ping(ctx context.Context) error {
	return a.DB.PingContext(ctx)
}

type sqlTxAdapter struct {
	tx *sql.Tx
}

var _ plugin.PluginTx = (*sqlTxAdapter)(nil)

func (a *sqlTxAdapter) Exec(ctx context.Context, query string, args ...interface{}) (int64, error) {
	result, err := a.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (a *sqlTxAdapter) Query(ctx context.Context, query string, args ...interface{}) (plugin.Rows, error) {
	rows, err := a.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsWrapper{rows: rows}, nil
}

func (a *sqlTxAdapter) QueryRow(ctx context.Context, query string, args ...interface{}) plugin.RowScanner {
	row := a.tx.QueryRowContext(ctx, query, args...)
	return &rowScanner{row: row}
}

func (a *sqlTxAdapter) Commit() error   { return a.tx.Commit() }
func (a *sqlTxAdapter) Rollback() error { return a.tx.Rollback() }
