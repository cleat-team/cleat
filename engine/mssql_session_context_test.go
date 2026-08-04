package engine

// Regression test for IMPROVEMENT-PLAN.md 2.71: the per-tenant SESSION_CONTEXT
// that SQL Server RLS depends on does not survive a connection being recycled
// through the pool.
//
// tenantSessionConnector runs sp_set_session_context in Connect, and the doc
// comment on getOrCreateTenantPool says that means "RLS is enforced
// automatically". It happens once per *connection*, not once per *use*.
// database/sql calls ResetSession when handing a pooled connection back out,
// go-mssqldb issues sp_reset_connection, and that clears SESSION_CONTEXT.
//
// With the shipped schema's seven security policies in place and no session
// context, every tenant-scoped read matches nothing. It fails closed, so it is
// a correctness and availability defect rather than a leak -- but on SQL
// Server the backend does not work.
//
// Needs a real SQL Server (CLEAT_TEST_MSSQL). See
// fence_lost_integration_test.go's header for the skip policy.

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// sessionTenantID reads back what SQL Server thinks this connection's tenant
// is. NULL -- reported here as the empty string -- is the failure the security
// policies would act on.
func sessionTenantID(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT CONVERT(nvarchar(64), SESSION_CONTEXT(N'tenant_id'))`).Scan(&got); err != nil {
		t.Fatalf("read SESSION_CONTEXT: %v", err)
	}
	if !got.Valid {
		return ""
	}
	return got.String
}

// TestMSSQLSessionContext_SurvivesConnectionReuse pins the property RLS
// depends on: every statement the pool serves must run with the tenant's
// session context set, not just the first one on a fresh connection.
//
// SetMaxOpenConns(1) makes the second query provably reuse the same physical
// connection, so a passing result cannot come from a new connection being
// opened and re-initialised.
func TestMSSQLSessionContext_SurvivesConnectionReuse(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	const tenant = "11111111-1111-1111-1111-111111111111"

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()

	pool, err := factory.getOrCreateTenantPool(ctx, tenant)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool: %v", err)
	}
	pool.SetMaxOpenConns(1)

	// First use: the connection was just opened, so Connect's
	// sp_set_session_context is still in effect.
	if got := sessionTenantID(t, ctx, pool); got != tenant {
		t.Fatalf("session context on a fresh connection = %q, want %q", got, tenant)
	}

	// The query above returned its connection to the pool. With
	// SetMaxOpenConns(1) this is necessarily the same connection again.
	if got := sessionTenantID(t, ctx, pool); got != tenant {
		t.Fatalf("session context after the connection was returned to the pool and re-acquired = %q, want %q -- "+
			"database/sql called ResetSession, go-mssqldb issued sp_reset_connection, and that cleared it. "+
			"Every tenant-scoped read served by a recycled connection matches nothing under the shipped "+
			"security policies", got, tenant)
	}

	// And again after an explicit round trip through an idle period, which is
	// the shape a worker's polling loop produces.
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("pool.Conn: %v", err)
	}
	var viaConn sql.NullString
	if err := conn.QueryRowContext(ctx,
		`SELECT CONVERT(nvarchar(64), SESSION_CONTEXT(N'tenant_id'))`).Scan(&viaConn); err != nil {
		conn.Close()
		t.Fatalf("read SESSION_CONTEXT via pinned conn: %v", err)
	}
	conn.Close()
	if !viaConn.Valid || viaConn.String != tenant {
		t.Errorf("session context on a pinned connection = %v, want %q", viaConn, tenant)
	}
}

// TestMSSQLSessionContext_TenantPoolsStaySeparate checks the property the
// per-tenant pool exists for, and that the fix for the test above cannot
// break: re-applying the session context must re-apply the *right* tenant.
//
// A reset hook that cached one tenant, or that read from a shared variable,
// would pass the reuse test and still cross tenants here.
func TestMSSQLSessionContext_TenantPoolsStaySeparate(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	const (
		tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
		tenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	)

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()

	poolA, err := factory.getOrCreateTenantPool(ctx, tenantA)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool(A): %v", err)
	}
	poolB, err := factory.getOrCreateTenantPool(ctx, tenantB)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool(B): %v", err)
	}
	poolA.SetMaxOpenConns(1)
	poolB.SetMaxOpenConns(1)

	// Interleave, so each pool serves a recycled connection at least once.
	for i := 0; i < 3; i++ {
		if got := sessionTenantID(t, ctx, poolA); got != tenantA {
			t.Fatalf("round %d: pool A session context = %q, want %q", i, got, tenantA)
		}
		if got := sessionTenantID(t, ctx, poolB); got != tenantB {
			t.Fatalf("round %d: pool B session context = %q, want %q", i, got, tenantB)
		}
	}
}
