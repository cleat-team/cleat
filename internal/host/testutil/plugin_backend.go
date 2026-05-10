package testutil

import (
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
