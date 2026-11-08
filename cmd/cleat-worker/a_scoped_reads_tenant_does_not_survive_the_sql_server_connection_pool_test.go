package main

// cleat#3678. auth.TenantStore.scopedRead reads a policy-protected table for one tenant. On SQL Server it
// sets the tenant with sp_set_session_context inside a transaction, but session context belongs to the
// CONNECTION and not to the transaction, so the connection returns to the pool still holding that tenant.
// Nothing in scopedRead clears it. What clears it is go-mssqldb answering database/sql's ResetSession with
// sp_reset_connection.
//
// That is a deliberate reliance on the driver, the same one engine/plugindb_tenant.go documents and pins
// with TestATenantKeyDoesNotSurviveTheSQLServerConnectionPool, and it holds because nothing on this pool
// consumes an ambient tenant. Until now this path had neither the sentence nor the test. This is the test:
// it fails if the shipped driver stops clearing the session, in which case every scoped read would leave a
// tenant on a connection that the next caller inherits.
//
// It is a pin and not a fix. A cancel before the read does not clear the context either, and what making
// this driver-independent would cost is measured on cleat#3678.
//
// THE CONTROL IS A DRIVER THAT DOES NOT RESET (noResetConnector, from the drop test): the same call on
// it must leave the tenant behind, or "the shipped driver leaves none" proves nothing about the read.
//
// If scopedRead is ever changed to clear the tenant itself (option iii on cleat#3678), the control case goes
// red BY DESIGN: the call would no longer leave a tenant on a driver that does not reset. That is the moment
// to replace this pin with one that asserts the no-reset case is clean too.

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"net/url"
	"os"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

func TestAScopedReadsTenantDoesNotSurviveTheSQLServerConnectionPool(t *testing.T) {
	ctx := context.Background()
	admin := testutil.TestDB(t, testutil.DialectMSSQL)
	var dbName string
	if err := admin.QueryRowContext(ctx, `SELECT DB_NAME()`).Scan(&dbName); err != nil {
		t.Fatalf("UNMEASURED: reading the database name: %v", err)
	}
	u, err := url.Parse(os.Getenv("CLEAT_TEST_MSSQL"))
	if err != nil {
		t.Fatalf("UNMEASURED: parsing the DSN: %v", err)
	}
	qv := u.Query()
	qv.Set("database", dbName)
	u.RawQuery = qv.Encode()

	// At least one tenant, so that scopedRead runs at least once. CountTenantDomains reads every tenant in
	// admin.tenants, one scopedRead each.
	tsAdmin, err := auth.NewTenantStoreForDialect(admin, auth.DialectMSSQL)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	tenant := uuid.New()
	orgName := "scopedread-3678-" + uuid.NewString()
	if _, err := tsAdmin.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: tenant, OrgName: orgName, Creator: "scopedread-test"}); err != nil {
		t.Fatalf("UNMEASURED: provisioning a tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DELETE FROM admin.tenant_api_keys WHERE tenant_id = @p1`, tenant.String())
		_, _ = admin.ExecContext(context.Background(), `DELETE FROM admin.tenants WHERE tenant_id = @p1`, tenant.String())
		_, _ = admin.ExecContext(context.Background(), `DELETE FROM admin.orgs WHERE name = @p1`, orgName)
	})

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
		if _, err := ts.CountTenantDomains(ctx); err != nil {
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

	t.Run("the control: a driver that does not reset leaves the tenant behind", func(t *testing.T) {
		cn, err := mssql.NewConnector(u.String())
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		db := sql.OpenDB(noResetConnector{cn})
		t.Cleanup(func() { db.Close() })
		before, after, next := scopedThenNext(t, db)
		if before != after {
			t.Fatalf("UNMEASURED: the next statement ran on connection %q, not %q, so the connection was not reused", after, before)
		}
		if next == "" {
			t.Fatal("UNMEASURED: a scoped read on a driver that does not reset left no tenant, so this read cannot see " +
				"a leftover tenant and the assertion below would pass for the wrong reason")
		}
	})

	t.Run("the shipped driver clears it on reuse", func(t *testing.T) {
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
			t.Fatalf("the pooled connection still carries tenant_id = %q after a scoped read; go-mssqldb is no longer "+
				"clearing SESSION_CONTEXT on ResetSession, and the next caller on auth.TenantStore's pool inherits "+
				"the last tenant it read (cleat#3678)", next)
		}
	})
}
