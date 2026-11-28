package main

// cleat#3678 (owner decision D70). auth.TenantStore.scopedRead on SQL Server folds the clear of the session
// tenant into the read's own batch: `<read>; EXEC sp_set_session_context ... NULL`. That is only safe if an
// error from the TRAILING statement reaches the caller, because a refused clear that went unreported would
// pool a connection still holding the tenant. database/sql's Row.Scan returns the first result set's error
// and discards the Close error, so whether the trailing statement's error surfaces is a property of
// go-mssqldb that has to be measured, not assumed. This pins it, on the row path and on the no-row path (where
// Scan would otherwise answer sql.ErrNoRows), and pins that the trailing statement RUNS when no row comes back.
//
// Known-positive: a trailing RAISERROR must surface. Controls: a trailing statement that succeeds leaves the
// first statement's answer alone (nil with a row, sql.ErrNoRows without).

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestATrailingStatementsErrorSurfacesThroughScan(t *testing.T) {
	admin := testutil.TestDB(t, testutil.DialectMSSQL)
	u := mssqlDatabaseURL(t, admin)

	cn, err := mssql.NewConnector(u.String())
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	for _, drv := range []struct {
		name string
		db   *sql.DB
	}{
		{"shipped driver", sql.OpenDB(cn)},
		{"driver without a session reset", sql.OpenDB(noResetConnector{cn})},
	} {
		db := drv.db
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(1)

		// scan runs one QueryRow inside a transaction on the pinned connection, as scopedRead does.
		scan := func(t *testing.T, q string) error {
			t.Helper()
			ctx := context.Background()
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			defer conn.Close()
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			var n int
			err = tx.QueryRowContext(ctx, q).Scan(&n)
			_ = tx.Rollback()
			return err
		}

		t.Run(drv.name, func(t *testing.T) {
			// Controls first: if these do not hold, the cases below say nothing.
			if err := scan(t, `SELECT 1; SELECT 2`); err != nil {
				t.Fatalf("UNMEASURED: control (row, trailing success) returned %v", err)
			}
			if err := scan(t, `SELECT 1 WHERE 1 = 0; SELECT 2`); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("UNMEASURED: control (no row, trailing success) returned %v, want sql.ErrNoRows", err)
			}

			for _, tc := range []struct{ name, q string }{
				{"after a row", `SELECT 1; RAISERROR(N'trailing statement refused', 16, 1)`},
				{"after no row", `SELECT 1 WHERE 1 = 0; RAISERROR(N'trailing statement refused', 16, 1)`},
			} {
				t.Run("a trailing error surfaces "+tc.name, func(t *testing.T) {
					err := scan(t, tc.q)
					if err == nil || errors.Is(err, sql.ErrNoRows) || !strings.Contains(err.Error(), "trailing statement refused") {
						t.Errorf("the trailing statement's error did not reach Scan: got %v", err)
					}
				})
			}

			t.Run("the trailing statement runs when no row comes back", func(t *testing.T) {
				ctx := context.Background()
				conn, err := db.Conn(ctx)
				if err != nil {
					t.Fatalf("UNMEASURED: %v", err)
				}
				defer conn.Close()
				var n int
				// Control: the key is not set on this connection yet.
				var before sql.NullString
				if err := conn.QueryRowContext(ctx, `SELECT CAST(SESSION_CONTEXT(N'pin_3678') AS NVARCHAR(10))`).Scan(&before); err != nil || before.Valid {
					t.Fatalf("UNMEASURED: the key is already set on the connection: %v %v", before, err)
				}
				tx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("UNMEASURED: %v", err)
				}
				err = tx.QueryRowContext(ctx, `SELECT 1 WHERE 1 = 0; EXEC sp_set_session_context @key = N'pin_3678', @value = N'ran'`).Scan(&n)
				_ = tx.Rollback()
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("UNMEASURED: want sql.ErrNoRows, got %v", err)
				}
				var after sql.NullString
				if err := conn.QueryRowContext(ctx, `SELECT CAST(SESSION_CONTEXT(N'pin_3678') AS NVARCHAR(10))`).Scan(&after); err != nil {
					t.Fatalf("UNMEASURED: %v", err)
				}
				if after.String != "ran" {
					t.Errorf("the trailing statement did not run after a no-row first statement: session context reads %q", after.String)
				}
				// Leave no trace on a connection that goes back to the pool.
				_, _ = conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'pin_3678', @value = NULL`)
			})
		})
	}
}
