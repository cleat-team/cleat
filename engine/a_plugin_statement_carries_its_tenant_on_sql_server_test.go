package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
)

// The SQL Server half of cleat#1277's tenant scoping: a plugin statement now
// carries its tenant in SESSION_CONTEXT, the way it carries it in a GUC on
// PostgreSQL.
//
// WHY THESE TESTS BUILD THEIR OWN POLICY instead of using the shipped one.
// applyTenantScoping still emits nothing on SQL Server -- that is cleat#1552's
// next step, deliberately separate, because policies landing before anything
// sets the key would fail every plugin statement closed on contact. So there is
// no shipped plugin policy to point these at yet, and a test that asserted only
// "the key is set" would be measuring the instrument. Each test below installs
// a filter predicate of the same shape migrations/mssql/001_schema.sql uses and
// asks what a statement can SEE, which is the question the key exists to
// answer.

// mssqlTenantTestPool opens a pool of exactly one connection.
//
// One connection, so every borrow after the first is a REUSE -- which is the
// only condition under which the pool-reset property below can be observed at
// all. It is a pool of its own rather than testutil.MSSQLTestDB's, because
// SetMaxOpenConns(1) on a shared handle would serialise every other test in the
// package that happened to be running.
func mssqlTenantTestPool(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}
	connStr := os.Getenv("CLEAT_TEST_MSSQL")
	if connStr == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	// Set but unreachable is a FAILURE, not a skip: somebody asked for SQL
	// Server and did not get it. scripts/check-skips.sh case (b).

	db, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open MSSQL test DB: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		t.Fatalf("ping MSSQL test DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mssqlScratchScopedTable creates a plugin-shaped table behind a filter
// predicate of the shipped shape, returns its name, and tears it down.
//
// The predicate is a copy rather than dbo.fn_tenant_filter itself: these tests
// must run against a database whether or not the engine schema is installed,
// and the point being made is about the SESSION_CONTEXT key, which both shapes
// read identically.
func mssqlScratchScopedTable(t *testing.T, db *sql.DB, tenantA, tenantB uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	const table = "cleat_test_1552_rows"
	const fn = "fn_tenant_filter_test_1552"
	const policy = "TenantFilter_test_1552"

	drop := []string{
		fmt.Sprintf(`IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'%s') DROP SECURITY POLICY dbo.%s`, policy, policy),
		fmt.Sprintf(`IF OBJECT_ID('dbo.%s','U') IS NOT NULL DROP TABLE dbo.%s`, table, table),
		fmt.Sprintf(`IF OBJECT_ID('dbo.%s','IF') IS NOT NULL DROP FUNCTION dbo.%s`, fn, fn),
	}
	create := []string{
		fmt.Sprintf(`CREATE TABLE dbo.%s (tenant_id UNIQUEIDENTIFIER NOT NULL, k NVARCHAR(50) NOT NULL, PRIMARY KEY (tenant_id, k))`, table),
		fmt.Sprintf(`INSERT INTO dbo.%s (tenant_id, k) VALUES ('%s','a1'),('%s','a2'),('%s','b1')`,
			table, tenantA, tenantA, tenantB),
		fmt.Sprintf(`CREATE FUNCTION dbo.%s(@tenant_id UNIQUEIDENTIFIER) RETURNS TABLE WITH SCHEMABINDING
			AS RETURN SELECT 1 AS access
			   WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)`, fn),
		fmt.Sprintf(`CREATE SECURITY POLICY dbo.%s ADD FILTER PREDICATE dbo.%s(tenant_id) ON dbo.%s WITH (STATE = ON)`,
			policy, fn, table),
	}
	for _, s := range append(append([]string{}, drop...), create...) {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	t.Cleanup(func() {
		for _, s := range drop {
			if _, err := db.ExecContext(context.Background(), s); err != nil {
				t.Logf("fixture teardown %q: %v", s, err)
			}
		}
	})
	return "dbo." + table
}

func mssqlSessionKey(t *testing.T, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) string {
	t.Helper()
	var v sql.NullString
	if err := q.QueryRowContext(context.Background(),
		`SELECT CAST(SESSION_CONTEXT(@p1) AS NVARCHAR(200))`, key).Scan(&v); err != nil {
		t.Fatalf("read SESSION_CONTEXT(%q): %v", key, err)
	}
	if !v.Valid {
		return ""
	}
	return v.String
}

// TestAPluginStatementIsScopedToItsTenantOnSQLServer is the behaviour the change
// exists for: the same adapter, the same statement, three different answers
// depending only on what the context carries.
func TestAPluginStatementIsScopedToItsTenantOnSQLServer(t *testing.T) {
	db := mssqlTenantTestPool(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	table := mssqlScratchScopedTable(t, db, tenantA, tenantB)

	adapter := &SQLDBAdapter{DB: db, Dialect: plugin.DialectMSSQL}
	countFor := func(ctx context.Context) int {
		var n int
		if err := adapter.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"tenant A sees only its own", tenantctx.With(context.Background(), tenantA), 2},
		{"tenant B sees only its own", tenantctx.With(context.Background(), tenantB), 1},
		// No tenant is not an error on this path -- beginTenantTx returns
		// (nil, nil) and the statement runs on the bare pool, where the policy
		// matches nothing. SQL Server has no counterpart to PostgreSQL's
		// cleat.assert_tenant_set(), which RAISEs: a filter predicate must be
		// an inline table-valued function, and an inline TVF has no procedural
		// body to raise from. So this row reads 0 rather than failing, and
		// that asymmetry is a known cost of the SQL Server arm rather than a
		// gap in this test. cleat#1552.
		{"no tenant sees nothing, silently", context.Background(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := countFor(tc.ctx); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestATenantKeyDoesNotSurviveTheSQLServerConnectionPool guards the one property
// this whole design rests on, and it is a property of the DRIVER rather than of
// cleat: sp_set_session_context is session-scoped, and what makes it behave
// transaction-scoped is database/sql calling ResetSession on a recycled
// connection and go-mssqldb answering with a TDS connection reset.
//
// If that ever stops happening the symptom is not an error. It is one tenant's
// key still set when the next borrower runs a statement -- a cross-tenant read
// with nothing anywhere to say so. That is why this is a test and not a
// sentence in a comment.
func TestATenantKeyDoesNotSurviveTheSQLServerConnectionPool(t *testing.T) {
	db := mssqlTenantTestPool(t)
	tenant := uuid.New()
	ctx := tenantctx.With(context.Background(), tenant)

	tx, err := beginTenantTx(ctx, db, plugin.DialectMSSQL, nil)
	if err != nil {
		t.Fatalf("beginTenantTx: %v", err)
	}
	if tx == nil {
		t.Fatal("beginTenantTx returned no transaction for a context carrying a tenant")
	}
	// The control. Without it a reader that can never see a value at all would
	// make the assertion below pass for the wrong reason.
	if got := mssqlSessionKey(t, tx, "tenant_id"); got != tenant.String() {
		t.Fatalf("inside the transaction SESSION_CONTEXT('tenant_id') = %q, want %q", got, tenant)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Same physical connection -- the pool holds exactly one.
	if got := mssqlSessionKey(t, db, "tenant_id"); got != "" {
		t.Fatalf("after the transaction the pooled connection still carries tenant_id = %q; "+
			"go-mssqldb is no longer clearing SESSION_CONTEXT on ResetSession, and every "+
			"tenant-scoped plugin statement is now a potential cross-tenant read", got)
	}
}

// TestACrossTenantSweepSetsNoTenantOnSQLServer pins the claim that the sweep key
// cannot widen anything the engine's own policies guard.
//
// dbo.fn_tenant_filter reads `tenant_id` and nothing else, so a transaction
// marked AcrossAllTenants must leave that key unset -- otherwise the sweep key
// would be a back door into the thirteen engine tables rather than an opt-in
// for the plugin tables cleat#1552 will add.
func TestACrossTenantSweepSetsNoTenantOnSQLServer(t *testing.T) {
	db := mssqlTenantTestPool(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	table := mssqlScratchScopedTable(t, db, tenantA, tenantB)

	const reason = "test: a sweep that names itself"
	ctx := plugin.AcrossAllTenants(context.Background(), reason)

	tx, err := beginTenantTx(ctx, db, plugin.DialectMSSQL, nil)
	if err != nil {
		t.Fatalf("beginTenantTx: %v", err)
	}
	if tx == nil {
		t.Fatal("beginTenantTx returned no transaction for a cross-tenant context")
	}
	defer tx.Rollback()

	if got := mssqlSessionKey(t, tx, "cross_tenant"); got != reason {
		t.Fatalf("SESSION_CONTEXT('cross_tenant') = %q, want %q", got, reason)
	}
	if got := mssqlSessionKey(t, tx, "tenant_id"); got != "" {
		t.Fatalf("a cross-tenant sweep set tenant_id = %q; it must set no tenant at all", got)
	}
	// And the consequence, measured rather than argued: a predicate that reads
	// only tenant_id is unaffected by the sweep key.
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a tenant_id-only predicate admitted %d rows to a cross-tenant sweep; "+
			"the sweep key must not widen the engine's own policies", n)
	}
}

// TestAReadOnlyPluginReadIsTenantScopedOnSQLServer covers the adapter cleat#1285
// added and cleat#1280 missed, on the dialect neither of them reached.
//
// It also covered the half that was broken, so that cleat#1615 being fixed
// would be visible here rather than silently changing behaviour. It was: that
// assertion required Begin to FAIL and said to remove it once the issue was
// fixed. cleat#1615 removed the `SET TRANSACTION READ ONLY` that made Begin
// impossible here, so the assertion now asserts what should be true instead --
// the transaction opens AND carries the tenant into it.
func TestAReadOnlyPluginReadIsTenantScopedOnSQLServer(t *testing.T) {
	db := mssqlTenantTestPool(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	table := mssqlScratchScopedTable(t, db, tenantA, tenantB)

	ro := &ReadOnlyDB{Inner: db, Dialect: plugin.DialectMSSQL}
	ctx := tenantctx.With(context.Background(), tenantA)

	var n int
	if err := ro.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("read-only QueryRow: %v", err)
	}
	if n != 2 {
		t.Fatalf("read-only QueryRow saw %d rows, want 2 (tenant A's own)", n)
	}

	tx, err := ro.Begin(ctx)
	if err != nil {
		t.Fatalf("ReadOnlyDB.Begin on SQL Server: %v", err)
	}
	// Roll back explicitly, and note WHY, because the failure it prevents does
	// not look like a leak. The assertion this replaced ended the test with
	// Begin's transaction still open; on SQL Server that holds locks on the
	// scratch table, the cleanup DROP blocks on them, and the package dies at
	// the suite timeout -- a package-level failure with ZERO failing tests,
	// which reads as a build break rather than as a test that forgot to close
	// something.
	defer tx.Rollback()

	var m int
	if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&m); err != nil {
		t.Fatalf("read-only Begin+QueryRow: %v", err)
	}
	if m != 2 {
		t.Errorf("a read-only transaction saw %d rows, want 2 (tenant A's own): "+
			"the tenant scoping must survive into the Begin path, not only QueryRow", m)
	}
}
