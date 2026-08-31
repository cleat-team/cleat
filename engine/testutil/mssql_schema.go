package testutil

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
)

// SetupMSSQLMinimalSchema builds the SQL Server test schema by applying the
// real, shipped migrations (migrations/mssql/*.sql) via applyMigrations --
// the exact code path cmd/cleat-worker/main.go runs at boot. "Minimal" is a
// historical name; see SetupMSSQLFullSchema.
func SetupMSSQLMinimalSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	applyMSSQLSchemaFile(t, db)
}

// SetupMSSQLFullSchema is SetupMSSQLMinimalSchema. Kept as a separate name
// because call sites across the repo use both; there is only ever one SQL
// Server test schema now, the shipped one, so the distinction the two names
// used to draw no longer exists.
func SetupMSSQLFullSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	applyMSSQLSchemaFile(t, db)
}

// applyMSSQLSchemaFile applies the shipped SQL Server migrations
// (migrations/mssql/*.sql) via applyMigrations/migration.Runner, then checks
// that the security policies those migrations install are still standing.
//
// This file used to hand-write ~330 lines of CREATE TABLE plus half a dozen
// migrateMSSQL* helpers that re-derived individual ALTER TABLE statements
// already present in migrations/mssql/. That copy had fallen behind the
// shipped files it was meant to approximate -- it never carried
// 021_schedule_timezone.sql or 022_schedule_policies.sql as migrations
// (patching their effect in by hand instead, in
// migrateMSSQLWorkflowDefsTenantID), and it never carried
// 031_workflow_promises_security_policy.sql at all, so no test in the repo
// could observe dbo.workflow_promises' tenant policy existing or failing to
// (that migration's own header names this exact gap). Applying the real
// directory removes the maintenance burden of keeping the copy current, not
// merely the copy's past mistakes.
//
// requireMSSQLPoliciesIntact runs on every call, cheaply (one COUNT against a
// catalogue view): migration.Runner treats a migration it has already
// recorded as done regardless of what has since happened to the objects that
// migration created, so a database that lost its security policies to
// something else (an earlier version of mssql_rls_enforcement_test.go used to
// drop them on cleanup) can never heal itself through the Runner alone, and
// every tenant-scoped MSSQL test after that would silently run without the
// backstop IMPROVEMENT-PLAN 2.71 is about. Once migrations/mssql/001_schema.sql
// has been recorded as applied -- which is true after every successful call
// here, first or repeat -- the seven base policies (eight, once 031 is
// recorded) must exist; if they do not, something other than this package
// removed them, and the test database needs to be dropped and recreated
// rather than silently continuing without a backstop.
func applyMSSQLSchemaFile(t *testing.T, db *sql.DB) {
	t.Helper()
	applyMigrations(t, db, DialectMSSQL)
	requireMSSQLPoliciesIntact(t, db)
}

// requireMSSQLPoliciesIntact fails loudly, with a fix, when the migrations
// are recorded as applied but the security policies they install are not
// present. See applyMSSQLSchemaFile's doc comment for why this can happen and
// why the Runner cannot detect or repair it on its own.
func requireMSSQLPoliciesIntact(t *testing.T, db *sql.DB) {
	t.Helper()
	var policies int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sys.security_policies`).Scan(&policies); err != nil {
		t.Fatalf("check security policies: %v", err)
	}
	if policies > 0 {
		return
	}
	var dbName string
	_ = db.QueryRow(`SELECT DB_NAME()`).Scan(&dbName)
	t.Fatalf("the SQL Server test database %q has the shipped migrations recorded as applied "+
		"but no security policies, so something dropped them and the migration runner will not "+
		"reinstall them (IMPROVEMENT-PLAN 2.71).\n\n"+
		"Every tenant-scoped test in this binary would run without a backstop.\n\n"+
		"Drop it once and re-run:\n\n"+
		"    DROP DATABASE %s; CREATE DATABASE %s;\n", dbName, dbName, dbName)
}

// mssqlCleanupTables is the set of tables CleanupMSSQLTestData clears.
// Kept in the same order as postgresCleanupTables; TestCleanupTableListsAgree
// fails if the three drift apart again.
var mssqlCleanupTables = []string{
	"admin.tenant_api_keys",
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

// CleanupMSSQLTestData removes all test data from the MSSQL tables.
// Uses DELETE with table existence checks. Order respects FK constraints.
//
// Deletes through an administrative connection, because on a database built
// from the shipped migrations the tenant filter predicate applies to every
// principal -- sa included. A plain pool with no session context matches no
// rows at all, so every DELETE here removed nothing and reported no error, and
// the rows stayed to collide with the next test's fixtures. That was §2.71's
// blocker; MSSQLAdminDB returns db unchanged when the database has no policies.
func CleanupMSSQLTestData(t *testing.T, db *sql.DB) {
	t.Helper()
	db = MSSQLAdminDB(t, db)

	// Order matters due to FK constraints — delete child tables first.
	//
	// Entries may be schema-qualified, and tenant_api_keys has to be: this
	// branch drops the duplicate dbo pair, so the only such table left is
	// admin.tenant_api_keys. Unqualified, the DELETE resolved against the
	// connecting principal's default schema and failed with "Invalid object
	// name" -- while the existence check above it passed, because sys.tables is
	// keyed on name alone and happily found the admin one.

	// present collects the tables that actually exist, so the emptiness check
	// below asks only about those. A table absent from this branch is the one
	// legitimate reason a delete here does nothing.
	var present []string
	for _, table := range mssqlCleanupTables {
		// Split "admin.tenant_api_keys" so the existence check can match on
		// schema as well as name. Checking on name alone is what let the
		// unqualified entry look present and then fail to delete.
		schema, name := "dbo", table
		if i := strings.IndexByte(table, '.'); i >= 0 {
			schema, name = table[:i], table[i+1:]
		}
		var exists int
		err := db.QueryRow(`SELECT COUNT(1) FROM sys.tables t
			JOIN sys.schemas s ON t.schema_id = s.schema_id
			WHERE s.name = @p1 AND t.name = @p2`, schema, name).Scan(&exists)
		if err != nil {
			t.Fatalf("cleanup: check table %s: %v", table, err)
		}
		if exists > 0 {
			if _, err := db.Exec(fmt.Sprintf("DELETE FROM [%s].[%s]", schema, name)); err != nil {
				t.Fatalf("cleanup: delete from %s: %v\n\n"+
					"This used to be a t.Logf. See IMPROVEMENT-PLAN 2.60d.", table, err)
			}
			present = append(present, table)
		}
	}

	// SQL Server is the dialect this check exists for. Its security policy
	// applies to every principal including sysadmin, so a filtered DELETE
	// removes nothing and reports success -- §3.37, and the 141-failure
	// signature in §2.71's residual.
	assertTablesEmpty(t, db, present, func(s string) string {
		schema, name := "dbo", s
		if i := strings.IndexByte(s, '.'); i >= 0 {
			schema, name = s[:i], s[i+1:]
		}
		return fmt.Sprintf("[%s].[%s]", schema, name)
	})
}

// MSSQLTestDB opens a connection to the MSSQL test database.
// Uses CLEAT_TEST_MSSQL environment variable.
// Default: sqlserver://sa:CleatTest123!@localhost:1433?database=cleat
func MSSQLTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping MSSQL database test in short mode")
	}

	connStr := os.Getenv("CLEAT_TEST_MSSQL")
	if connStr == "" {
		connStr = "sqlserver://sa:CleatTest123!@localhost:1433?database=cleat"
		t.Logf("CLEAT_TEST_MSSQL not set, using default: %s", connStr)
	}

	db, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open MSSQL test DB: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("ping MSSQL test DB: %v\nHint: Is SQL Server running? Start with:\n  docker run -e 'ACCEPT_EULA=Y' -e 'MSSQL_SA_PASSWORD=CleatTest123!' -p 1433:1433 -d mcr.microsoft.com/mssql/server:2022-latest", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db
}
