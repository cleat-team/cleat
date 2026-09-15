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
// unused -- every fixture that needed anything needed this one. The reason
// string is recorded in the session for the same reason a real sweep's is: a
// connection holding a bypass should be able to say why.
//
// It is a no-op on PostgreSQL and MySQL, and the connection is released by
// t.Cleanup. Returning it to the pool clears the session context (go-mssqldb
// answers database/sql's ResetSession with a TDS connection reset), so a pinned
// bypass cannot leak to the next borrower.
func (b PluginTestBackend) CrossTenantConn(t *testing.T, ctx context.Context, reason string) *sql.Conn {
	t.Helper()
	conn, err := b.DB.Conn(ctx)
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
