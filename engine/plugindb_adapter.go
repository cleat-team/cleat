package engine

import (
	"context"
	"database/sql"

	"github.com/cleat-team/cleat/plugin"
)

// SQLDBAdapter wraps *sql.DB and implements plugin.PluginDB with full
// read-write access. Used when a plugin declares DatabaseAccessReadWrite.
//
// Dialect is the backend this adapter talks to. Every statement passing through
// is put through plugin.Rebind before it reaches the driver, so a plugin writes
// the primary dialect once and does not have to remember to translate it.
//
// WHY THE TRANSLATION MOVED HERE (cleat#1133). Rebind was opt-in at the call
// site, and 166 sites called it while 40 did not -- so those 40 sent
// PostgreSQL $N placeholders to MySQL and SQL Server, where they are not
// placeholders at all. That is not 40 mistakes; it is one requirement that
// authors meet most of the time, and the miss rate does not improve on its
// own. The dialect was available at 19 of 21 plugins, so this was never a
// plumbing problem -- it was a remembering problem, and the fix for a
// remembering problem is to stop requiring the memory.
//
// Rebind is idempotent (asserted by TestRebindIsIdempotent), which is what
// makes doing it here safe for the 166 sites that already do it themselves:
// @pN contains no $N, SYSUTCDATETIME() does not match now(), 1 does not match
// TRUE.
//
// This handles the frequent, mechanical differences only -- placeholders,
// now(), boolean literals. It deliberately does NOT attempt LIMIT/TOP,
// ON CONFLICT/MERGE or RETURNING/OUTPUT: those change the shape of the
// statement rather than a token in it, and a rewrite that ambitious inside an
// adapter would be the kind of thing that is impossible to review. Plugins
// handle those with conditional code on Environment.Dialect.
type SQLDBAdapter struct {
	DB      *sql.DB
	Dialect plugin.Dialect
}

var _ plugin.PluginDB = (*SQLDBAdapter)(nil)

func (a *SQLDBAdapter) Begin(ctx context.Context) (plugin.PluginTx, error) {
	tx, err := a.tenantTx(ctx)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		if tx, err = a.DB.BeginTx(ctx, nil); err != nil {
			return nil, err
		}
	}
	return &sqlTxAdapter{tx: tx, dialect: a.Dialect}, nil
}

func (a *SQLDBAdapter) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	tx, err := a.tenantTx(ctx)
	if err != nil {
		return 0, err
	}
	if tx == nil {
		result, err := a.DB.ExecContext(ctx, plugin.Rebind(query, a.Dialect), args...)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
	result, err := tx.ExecContext(ctx, plugin.Rebind(query, a.Dialect), args...)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func (a *SQLDBAdapter) Query(ctx context.Context, query string, args ...any) (plugin.Rows, error) {
	tx, err := a.tenantTx(ctx)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		rows, err := a.DB.QueryContext(ctx, plugin.Rebind(query, a.Dialect), args...)
		if err != nil {
			return nil, err
		}
		return &sqlRowsWrapper{rows: rows}, nil
	}
	rows, err := tx.QueryContext(ctx, plugin.Rebind(query, a.Dialect), args...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return &sqlRowsWrapper{rows: rows, done: tx.Commit}, nil
}

func (a *SQLDBAdapter) QueryRow(ctx context.Context, query string, args ...any) plugin.RowScanner {
	tx, err := a.tenantTx(ctx)
	if err != nil {
		return &rowScanner{err: err}
	}
	if tx == nil {
		row := a.DB.QueryRowContext(ctx, plugin.Rebind(query, a.Dialect), args...)
		return &rowScanner{row: row}
	}
	row := tx.QueryRowContext(ctx, plugin.Rebind(query, a.Dialect), args...)
	return &rowScanner{row: row, done: tx.Commit}
}

func (a *SQLDBAdapter) Ping(ctx context.Context) error {
	return a.DB.PingContext(ctx)
}

type sqlTxAdapter struct {
	tx      *sql.Tx
	dialect plugin.Dialect
}

var _ plugin.PluginTx = (*sqlTxAdapter)(nil)

func (a *sqlTxAdapter) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	result, err := a.tx.ExecContext(ctx, plugin.Rebind(query, a.dialect), args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (a *sqlTxAdapter) Query(ctx context.Context, query string, args ...any) (plugin.Rows, error) {
	rows, err := a.tx.QueryContext(ctx, plugin.Rebind(query, a.dialect), args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsWrapper{rows: rows}, nil
}

func (a *sqlTxAdapter) QueryRow(ctx context.Context, query string, args ...any) plugin.RowScanner {
	row := a.tx.QueryRowContext(ctx, plugin.Rebind(query, a.dialect), args...)
	return &rowScanner{row: row}
}

func (a *sqlTxAdapter) Commit() error   { return a.tx.Commit() }
func (a *sqlTxAdapter) Rollback() error { return a.tx.Rollback() }
