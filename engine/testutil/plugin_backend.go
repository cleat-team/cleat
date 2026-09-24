package testutil

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// PluginTestBackend provides a real database connection for plugin behavioral
// tests. Each backend (PostgreSQL, MySQL, MSSQL) represents a database
// connection that a plugin can run migrations against and execute queries
// against. Call Cleanup when done to release the connection.
type PluginTestBackend struct {
	// Name is a human-readable backend identifier, e.g. "postgres", "mysql".
	Name string

	// Dialect identifies the SQL dialect for schema setup and migration
	// execution.
	Dialect Dialect

	// DB is the open database connection.
	DB *sql.DB

	// Cleanup releases the database connection. Must be called (typically via
	// defer) after the test completes.
	Cleanup func()
}

// NewPluginTestBackends returns all available database backends for plugin
// behavioral tests.
//
// PostgreSQL is always attempted — a default DSN of
// "postgres://localhost:5432/cleat?sslmode=disable" is used when neither
// CLEAT_TEST_POSTGRES nor CLEAT_TEST_DB is set. If the connection fails the
// calling test is skipped.
//
// MySQL is included only when CLEAT_TEST_MYSQL is set. If the variable is
// set but the connection fails the calling test is fatally terminated (the
// user explicitly requested MySQL).
//
// MSSQL is included only when CLEAT_TEST_MSSQL is set, with the same
// behaviour as MySQL on connection failure.
//
// Each backend's Cleanup function must be called (typically via defer) to
// release the database connection. Both PluginTestBackend.Cleanup and the
// test's t.Cleanup will close the connection; calling Cleanup explicitly
// lets tests control ordering (e.g. close after dropping test tables).
func NewPluginTestBackends(t *testing.T) []PluginTestBackend {
	t.Helper()

	var backends []PluginTestBackend

	// PostgreSQL is always attempted.
	pgDB := TestDB(t, DialectPostgres)
	backends = append(backends, PluginTestBackend{
		Name:    "postgres",
		Dialect: DialectPostgres,
		DB:      pgDB,
		Cleanup: func() { pgDB.Close() },
	})

	// MySQL only if CLEAT_TEST_MYSQL is set.
	if os.Getenv("CLEAT_TEST_MYSQL") != "" {
		mysqlDB := MySQLTestDB(t)
		backends = append(backends, PluginTestBackend{
			Name:    "mysql",
			Dialect: DialectMySQL,
			DB:      mysqlDB,
			Cleanup: func() { mysqlDB.Close() },
		})
	}

	// MSSQL only if CLEAT_TEST_MSSQL is set.
	if os.Getenv("CLEAT_TEST_MSSQL") != "" {
		mssqlDB := MSSQLTestDB(t)
		backends = append(backends, PluginTestBackend{
			Name:    "mssql",
			Dialect: DialectMSSQL,
			DB:      mssqlDB,
			Cleanup: func() { mssqlDB.Close() },
		})
	}

	return backends
}

// CrossTenantConn pins a connection that may reach EVERY tenant's rows, for
// fixtures that touch a tenant-scoped plugin table DIRECTLY rather than through
// the plugin's own adapter. cleat#1552.
//
// WHY A FIXTURE NEEDS THIS AT ALL, AND WHY ONLY ON SQL SERVER. A fixture
// writing `INSERT INTO task_queue (tenant_id, ...) VALUES ($1, ...)` on
// PluginTestBackend.DB is issuing correct, fully tenant-qualified SQL on a pool
// that carries no tenant context. That was fine while SQL Server had no
// policies. It is not fine now:
//
//   - a WRITE is refused by the policy's BLOCK predicate -- loudly, with
//     "the target object ... has a block predicate that conflicts with this
//     operation";
//   - a READ or a DELETE is filtered to nothing -- SILENTLY. The kvstore
//     fixture's `DELETE FROM kv_store WHERE tenant_id = $1` removed no rows and
//     reported success, and the next scenario counted one row too many and
//     blamed its own SELECT.
//
// The literal in the statement is not what the policy reads. It reads
// SESSION_CONTEXT, which engine.beginTenantTx sets at runtime and which this
// pins for a fixture.
//
// PostgreSQL fixtures never needed it, and that is not luck: its policy calls
// cleat.assert_tenant_set(), which RAISES, so an unscoped fixture there failed
// the day the policy landed and was fixed then. SQL Server cannot raise from a
// filter predicate -- an inline table-valued function has no body to raise from
// -- so the same mistake is silent there, and nothing else will say so.
//
// CROSS-TENANT RATHER THAN PER-TENANT, deliberately: a fixture invents whatever
// tenants it likes and frequently seeds several, so pinning it to one would
// just move the problem. A per-tenant variant was written first and deleted
// unused -- every fixture that needed anything needed this one.
//
// THE cross_tenant SESSION KEY IS NOT A BYPASS ON CORE (dbo.fn_tenant_filter)
// TABLES, AND THE COMMENT HERE IMPLIED OTHERWISE UNTIL cleat#2205 -- BUT IT IS
// THE REAL BYPASS ON PLUGIN TABLES, AND THAT HALF WAS ALREADY TRUE. Two
// different predicate functions read two different things. dbo.fn_tenant_filter
// (the core tables: workflow_defs, workflow_instances, tenant_secrets, ...) has
// never in its shipped history read a key called cross_tenant: 001 and 012
// check only SESSION_CONTEXT('tenant_id') and IS_ROLEMEMBER('cleat_admin'), and
// 075 made even that disjunct opt-in, off by default. Measured directly against
// a migrated CLEAT_TEST_MSSQL database, connected as sa exactly as MSSQLTestDB
// connects: `SELECT IS_ROLEMEMBER(N'cleat_admin')` returns 0 and
// admin.rls_predicate_form reads 'plain'. So on a CORE table, a plain sa
// connection setting only the cross_tenant key was never exempt from FILTER
// either.
//
// dbo.fn_plugin_tenant_filter (every plugin-owned table: workflow_blob_refs,
// kv_store, task_queue, ...) is a SEPARATE function, and it DOES read this
// key: an OR disjunct in plugin/migration.go's applyTenantScopingMSSQL casts
// SESSION_CONTEXT(N'cross_tenant') to NVARCHAR and admits any non-empty
// value. It is not a test-only convenience: plugin.markCrossTenantOnTx sets
// the identical key in production, and every plugin's AcrossAllTenants sweep
// (blobstore's stale-ref sweep among them) depends on it to see every
// tenant's rows in one pass. So the `EXEC
// sp_set_session_context @key = N'cross_tenant', ...` call below is exactly
// the bypass a fixture touching a PLUGIN table needs, and is the same
// mechanism the code under test uses -- keep it wired for anything that reads,
// writes, or deletes a plugin table through this connection.
//
// What it is NOT is a way to write a CORE table across the block predicates
// cleat#2205 (migration 103) added to dbo.fn_tenant_filter. For MSSQL this now
// ALSO routes the connection through MSSQLAdminDB: it applies
// migrations/mssql/optional/cross_tenant_claim.sql (switching
// dbo.fn_tenant_filter to the IS_ROLEMEMBER('cleat_admin') form, refcounted and
// restored per mssql_admin.go's file comment so it does not leak into a
// deployment-shaped test elsewhere in the suite) and returns a pool
// authenticated as a member of that role, which migration 103's BLOCK
// predicates admit on a core table exactly as IS_ROLEMEMBER already admitted
// FILTER reads and deletes there. It changes nothing for a plugin table:
// fn_plugin_tenant_filter has no IS_ROLEMEMBER branch at all, so a fixture's
// access to a plugin table continues to run on cross_tenant alone, whichever
// login holds the connection.
//
// It is a no-op on PostgreSQL and MySQL, and the connection is released by
// t.Cleanup. Returning it to the pool clears the session context (go-mssqldb
// answers database/sql's ResetSession with a TDS connection reset), so a pinned
// bypass cannot leak to the next borrower.
func (b PluginTestBackend) CrossTenantConn(t *testing.T, ctx context.Context, reason string) *sql.Conn {
	t.Helper()

	pool := b.DB
	if b.Dialect == DialectMSSQL {
		pool = MSSQLAdminDB(t, b.DB)
	}

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection on %s: %v", b.Name, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if b.Dialect != DialectMSSQL {
		return conn
	}
	if _, err := conn.ExecContext(ctx,
		`EXEC sp_set_session_context @key = N'cross_tenant', @value = @p1`, reason); err != nil {
		t.Fatalf("pin a cross-tenant fixture on %s: %v", b.Name, err)
	}
	return conn
}
