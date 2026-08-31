package testutil

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// SetupMySQLFullSchema builds the MySQL test schema by applying the real,
// shipped migrations (migrations/mysql/*.sql) via applyMigrations --
// the exact code path cmd/cleat-worker/main.go runs at boot.
//
// This file used to hand-write ~330 lines of CREATE TABLE, a second,
// independent definition of a schema that already existed in
// migrations/mysql/. It had drifted from the shipped one in ways that hid
// real defects rather than being merely untidy: it was missing whole tables
// production has (tenants, tenant_roles, workflow_tags, workflow_routing,
// plugin_tables), and it declared event_history.service/operation/request
// NOT NULL where the shipped schema does not (migration 030 fixed that at
// the schema level; this file is the mechanism that let the two disagree in
// the first place -- IMPROVEMENT-PLAN, "the tests do not run against the
// schema that ships"). migration/mysql_bootstrap_test.go already proves the
// Runner applies these files correctly, DELIMITER handling included.
func SetupMySQLFullSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	applyMigrations(t, db, DialectMySQL)
}

// mysqlCleanupTables is the set of tables CleanupMySQLTestData clears.
// Order respects FK constraints — child tables first.
// Kept in the same order as postgresCleanupTables; TestCleanupTableListsAgree
// fails if the three drift apart again.
var mysqlCleanupTables = []string{
	"tenant_api_keys",
	"workflow_tags",
	"workflow_routing",
	"workflow_update_requests",
	"workflow_promises",
	"workflow_signals",
	"concurrency_keys",
	"idempotency_keys",
	"event_history",
	"workflow_memory_samples",
	"workflow_memory_stats",
	"workflow_schedules",
	"workflow_instances",
	"workflow_defs",
	"plugin_defs",
}

// CleanupMySQLTestData removes all test data from MySQL tables.
func CleanupMySQLTestData(t *testing.T, db *sql.DB) {
	t.Helper()

	// Same existence check as PostgreSQL and SQL Server: a table absent from
	// this schema variant is the one legitimate reason a delete here does
	// nothing, and it must not be confused with a delete that was filtered.
	present := existingTables(t, db, DialectMySQL, mysqlCleanupTables)

	for _, table := range present {
		if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s", table)); err != nil {
			t.Fatalf("cleanup: delete from %s: %v\n\n"+
				"This used to be a t.Logf. See IMPROVEMENT-PLAN 2.60d.", table, err)
		}
	}
	assertTablesEmpty(t, db, present, func(s string) string { return s })
}

// MySQLTestDB opens a connection to the MySQL test database.
// Uses CLEAT_TEST_MYSQL environment variable.
// Default: root:cleat@tcp(127.0.0.1:3306)/cleat
func MySQLTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping MySQL database test in short mode")
	}

	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		dsn = "root:cleat@tcp(127.0.0.1:3306)/cleat?tls=false&parseTime=true&multiStatements=true"
		t.Logf("CLEAT_TEST_MYSQL not set, using default: %s", dsn)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL test DB: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("ping MySQL test DB: %v\nHint: Is MySQL running? Start with:\n  docker run -e MYSQL_ROOT_PASSWORD=cleat -e MYSQL_DATABASE=cleat -p 3306:3306 -d mysql:8.4", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db
}
