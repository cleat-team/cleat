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
	"database/sql/driver"
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

// sessionIdentityExpr names ONE connection. @@SPID alone does not: SQL Server hands the same session id back to
// a replacement connection, measured by cleat-review on cleat#3682 (20 of 20 reconnects on a pool of one reused
// the id). connection_id is a guid per connection. login_time is NOT used: sp_reset_connection rewrites it, so
// it differs across a reset on the very same connection (measured here, a few milliseconds apart).
const sessionIdentityExpr = `CONVERT(nvarchar(20), @@SPID) + N'/' + ISNULL(CONVERT(nvarchar(36),
	(SELECT c.connection_id FROM sys.dm_exec_connections c WHERE c.session_id = @@SPID)), N'-')`

const sessionIdentityQuery = `SELECT ` + sessionIdentityExpr

// queryRower is the one method *sql.DB, *sql.Conn and *sql.Tx share that these tests need.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func sessionIdentity(ctx context.Context, q queryRower) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, sessionIdentityQuery).Scan(&id)
	return id, err
}

// TestMSSQLSessionContext_SurvivesConnectionReuse pins the property RLS
// depends on: every statement the pool serves must run with the tenant's
// session context set, not just the first one on a fresh connection.
//
// SetMaxOpenConns(1) makes the second query reuse the same physical
// connection UNLESS database/sql discards it in between, and a session id does
// not tell those apart: SQL Server hands the same id to a replacement. Where
// that matters the test compares sessionIdentity, and checks that it can tell a
// discarded connection from a reused one.
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

	pool, err := tenantPoolDB(ctx, factory, tenant)
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

	// cleat#3647: and a connection left holding ANOTHER tenant's context -- a call that set it and was
	// cancelled, or any statement that set a different tenant on a connection from this pool -- must serve
	// its next statement as THIS pool's tenant. Two layers do that, independently: the driver's reset
	// clears the foreign tenant, and tenantSessionConn.ResetSession then applies this pool's own, whether
	// or not the driver reset. Measured with each layer removed in turn (the foreign tenant became NULL
	// with only the driver's reset, and the pool's own tenant with only the connector's), so a driver that
	// stops resetting does not hand a tenant pool another tenant's context.
	//
	// The control is the first read below: the SAME session must report the foreign tenant, or the read
	// cannot see a leftover and the assertion after it proves nothing. The pool holds one connection, and
	// the connection identity (session id and connection id) is compared so "the next statement" means
	// "the connection that held it".
	//
	// KNOWN-POSITIVE for "was it the same connection": discard the connection once between two reads and
	// confirm the identity below reports a DIFFERENT one. Without it a replaced connection would pass the
	// reuse check and the assertion after it would pass because of the replacement.
	discarded, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("pool.Conn: %v", err)
	}
	idBefore, err := sessionIdentity(ctx, discarded)
	if err != nil {
		discarded.Close()
		t.Fatalf("UNMEASURED: reading a session identity: %v", err)
	}
	_ = discarded.Raw(func(any) error { return driver.ErrBadConn }) // database/sql closes it instead of pooling it
	discarded.Close()
	idAfter, err := sessionIdentity(ctx, pool)
	if err != nil {
		t.Fatalf("UNMEASURED: reading a session identity: %v", err)
	}
	if idBefore == idAfter {
		t.Fatalf("UNMEASURED: a connection discarded between two reads kept the identity %q, so the identity cannot tell a replaced "+
			"connection from a reused one and the reuse check below proves nothing", idBefore)
	}

	const foreign = "22222222-2222-2222-2222-222222222222"
	held, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("pool.Conn: %v", err)
	}
	idHeld, err := sessionIdentity(ctx, held)
	if err != nil {
		held.Close()
		t.Fatalf("read the session identity: %v", err)
	}
	if _, err := held.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, foreign); err != nil {
		held.Close()
		t.Fatalf("set a foreign tenant by hand: %v", err)
	}
	var onSession sql.NullString
	if err := held.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(64), SESSION_CONTEXT(N'tenant_id'))`).Scan(&onSession); err != nil {
		held.Close()
		t.Fatalf("read the foreign tenant back: %v", err)
	}
	held.Close() // back to the pool, still holding the foreign tenant
	if !onSession.Valid || onSession.String != foreign {
		t.Fatalf("the control failed: the same session reads %v after a foreign tenant was set by hand, want %q, "+
			"so this read cannot see a leftover tenant", onSession, foreign)
	}
	var idNext string
	var next sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT `+sessionIdentityExpr+`, CONVERT(nvarchar(64), SESSION_CONTEXT(N'tenant_id'))`).Scan(&idNext, &next); err != nil {
		t.Fatalf("read the next statement's tenant: %v", err)
	}
	if idNext != idHeld {
		t.Fatalf("the next statement ran on connection %q, not %q, so the connection was not reused and this measured nothing", idNext, idHeld)
	}
	if next.Valid && next.String == foreign {
		t.Fatalf("a tenant pool's connection served its next statement as another tenant (%s): neither the driver's "+
			"reset nor the connector's re-apply replaced it", foreign)
	}
	if !next.Valid || next.String != tenant {
		t.Errorf("the next statement on this pool read tenant %v after a foreign tenant was left on the connection, want %q", next, tenant)
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

	poolA, err := tenantPoolDB(ctx, factory, tenantA)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool(A): %v", err)
	}
	poolB, err := tenantPoolDB(ctx, factory, tenantB)
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
