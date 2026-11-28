package main

// cleat#3678. auth.TenantStore.scopedRead reads a policy-protected table for one tenant. On SQL Server it
// sets the tenant with sp_set_session_context inside a transaction, but session context belongs to the
// CONNECTION and not to the transaction, so the connection returns to the pool still holding that tenant.
// Nothing in scopedRead clears it. What clears it is go-mssqldb answering database/sql's ResetSession with
// sp_reset_connection.
//
// Until cleat#3678 that was a deliberate reliance on the driver, the same one engine/plugindb_tenant.go
// documents and pins with TestATenantKeyDoesNotSurviveTheSQLServerConnectionPool. The owner decided
// (D65, option iii) that scopedRead pins a connection and clears the tenant itself, discarding the
// connection when the clear does not succeed, so the result does not depend on the driver. This test is
// the regression guard for that, on both kinds of driver:
//
//   - the shipped driver, which resets the session on reuse (go-mssqldb v1.11.2): no tenant afterwards;
//   - a driver that does NOT reset (noResetConnector, from the drop test): no tenant afterwards either.
//     This is the case that is red against the version that relied on the driver.
//
// THE CONTROL: on that same non-resetting driver a marker set by hand on a pinned connection must SURVIVE
// reuse, or "no tenant" above could come from a wrapper that failed to hide the reset.
// TestAScopedReadWhoseClearIsRefusedDiscardsItsConnection pins the discard.

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

func TestAScopedReadsTenantDoesNotSurviveTheSQLServerConnectionPool(t *testing.T) {
	ctx := context.Background()
	admin := testutil.TestDB(t, testutil.DialectMSSQL)
	u := mssqlDatabaseURL(t, admin)
	mssqlAppLoginURL(t, admin, u, true) // the application login; VIEW SERVER STATE only so the probe can name a connection

	// identityExpr names ONE connection. @@SPID alone does not: SQL Server hands the same session id back to a
	// replacement connection (cleat-review measured 20 of 20 on a pool of one), so "same @@SPID" would call a
	// replaced connection a reused one. connection_id is a guid per connection. login_time is NOT used:
	// sp_reset_connection rewrites it, so it differs across a reset on the very same connection.
	const identityExpr = `CONVERT(nvarchar(20), @@SPID) + N'/' + ISNULL(CONVERT(nvarchar(36),
		(SELECT c.connection_id FROM sys.dm_exec_connections c WHERE c.session_id = @@SPID)), N'-')`
	const identityQuery = `SELECT ` + identityExpr

	// scopedThenNext runs one scoped read on a pool of ONE connection and returns the connection identity on
	// both sides of it and what the NEXT statement on that pool reads as its tenant. The same identity on both
	// sides is what makes "the next statement" mean "the connection the scoped read used".
	scopedThenNext := func(t *testing.T, db *sql.DB) (idBefore, idAfter, next string) {
		t.Helper()
		db.SetMaxOpenConns(1)
		if err := db.QueryRowContext(ctx, identityQuery).Scan(&idBefore); err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		ts, err := auth.NewTenantStoreForDialect(db, auth.DialectMSSQL)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		if _, err := ts.TenantForHost(ctx, "pin-3678.invalid", uuid.New()); err != nil {
			t.Fatalf("UNMEASURED: the scoped read failed: %v", err)
		}
		var v sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT `+identityExpr+`, CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(100))`).Scan(&idAfter, &v); err != nil {
			t.Fatalf("UNMEASURED: reading the session context: %v", err)
		}
		return idBefore, idAfter, v.String
	}

	// KNOWN-POSITIVE for "was it the same connection": discard the connection once between two reads and the
	// identity must change, or the reuse checks below could not tell a replaced connection from a reused one.
	t.Run("the identity tells a replaced connection from a reused one", func(t *testing.T) {
		db, err := sql.Open("sqlserver", u.String())
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(1)
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		var before, after string
		if err := c.QueryRowContext(ctx, identityQuery).Scan(&before); err != nil {
			c.Close()
			t.Fatalf("UNMEASURED: %v", err)
		}
		_ = c.Raw(func(any) error { return sqldriver.ErrBadConn }) // database/sql closes it instead of pooling it
		c.Close()
		if err := db.QueryRowContext(ctx, identityQuery).Scan(&after); err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		if before == after {
			t.Fatalf("UNMEASURED: a connection discarded between two reads kept the identity %q, so the identity cannot tell a "+
				"replaced connection from a reused one", before)
		}
	})

	t.Run("the control: a driver that does not reset keeps a marker set by hand", func(t *testing.T) {
		cn, err := mssql.NewConnector(u.String())
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		db := sql.OpenDB(noResetConnector{cn})
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(1)
		pin, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		if _, err := pin.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = N'CONTROL-MARKER'`); err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		_ = pin.Close()
		if got := sessionTenant(t, db); got != "CONTROL-MARKER" {
			t.Fatalf("UNMEASURED: a marker set by hand reads %q after reuse on a driver that does not reset, so this "+
				"wrapper does not hide the reset and the case below measures nothing", got)
		}
	})

	t.Run("a driver that does not reset: a scoped read leaves no tenant", func(t *testing.T) {
		cn, err := mssql.NewConnector(u.String())
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		db := sql.OpenDB(noResetConnector{cn})
		t.Cleanup(func() { db.Close() })
		before, after, next := scopedThenNext(t, db)
		if before != after {
			t.Fatalf("UNMEASURED: the next statement ran on connection %q, not %q, so the connection was not reused and "+
				"a clean read says nothing about the clear", after, before)
		}
		if next != "" {
			t.Fatalf("the next statement on a pool whose driver does not reset runs as tenant %q after a scoped read; "+
				"scopedRead depends on the driver again (cleat#3678, D65)", next)
		}
	})

	t.Run("the shipped driver: a scoped read leaves no tenant", func(t *testing.T) {
		db, err := sql.Open("sqlserver", u.String())
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		before, after, next := scopedThenNext(t, db)
		if before != after {
			t.Fatalf("UNMEASURED: the next statement ran on connection %q, not %q, so the connection was not reused", after, before)
		}
		if next != "" {
			t.Fatalf("the next statement on auth.TenantStore's pool runs as tenant %q after a scoped read (cleat#3678)", next)
		}
	})
}
