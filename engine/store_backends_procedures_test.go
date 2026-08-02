package engine

// Installs the finalize_workflow_status stored procedure/function (and any
// other migrations/<dialect>/003_procedures.sql + 004_*.sql content) into a
// test database. testutil.SetupFullSchema and friends only create the raw
// tables -- they do not run the server-side procedures migration -- so any
// test that exercises FinalizeWorkflowSegment against a real database
// (including the pre-existing TestFinalizeWorkflowSegment_ParentWake* tests
// and the new zombie-writer regression test in
// fence_lost_integration_test.go) needs this applied first, or the
// procedure simply won't exist and the call will fail with
// "function/procedure finalize_workflow_status does not exist".
//
// This reads the actual production migration files from disk (under
// ../migrations/<dialect>/) rather than duplicating their SQL, so tests
// exercise the real, shipped fenced logic -- not a hand-rolled substitute
// that could drift from production.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// postgresProcedureMigrations lists the migration files (in order) that
// define finalize_workflow_status and friends for PostgreSQL.
var postgresProcedureMigrations = []string{
	"003_procedures.sql",
	"004_fix_finalize_workflow_status_fence.sql",
}

var mysqlProcedureMigrations = []string{
	"003_procedures.sql",
	"004_fix_finalize_workflow_status_fence.sql",
}

var mssqlProcedureMigrations = []string{
	"003_procedures.sql",
	"004_fix_finalize_workflow_status_fence.sql",
}

// Every Postgres-backed subtest that goes through PostgresBackend.Setup
// calls applyPostgresProcedures against the same shared CLEAT_TEST_DB
// instance (there is one database, not one per test). 003_procedures.sql
// defines finalize_workflow_status as RETURNS VOID; 004 changes it to
// RETURNS BOOLEAN. Postgres allows CREATE OR REPLACE to redefine a
// function's body but not its return type, so replaying 003 a second time
// against a database where 004 has already landed the BOOLEAN signature
// fails with 42P13 "cannot change return type of existing function" --
// even though 003 and 004 each apply cleanly once, in order, against a
// fresh database. Applying the sequence exactly once per test binary run,
// rather than once per subtest, sidesteps the replay entirely: the
// functions only need to exist, not be redefined by every subtest that
// depends on them.
var (
	postgresProceduresOnce sync.Once
	postgresProceduresErr  error
	mysqlProceduresOnce    sync.Once
	mysqlProceduresErr     error
	mssqlProceduresOnce    sync.Once
	mssqlProceduresErr     error
)

// applyPostgresProcedures installs finalize_workflow_status (and friends)
// against a PostgreSQL test database. PostgreSQL's simple query protocol
// (used by lib/pq for a plain db.Exec) natively accepts a string containing
// multiple ';'-terminated statements, so each file can be sent as-is.
//
// The actual application happens at most once per test binary run (see
// postgresProceduresOnce above); every call after the first just replays
// the cached result.
func applyPostgresProcedures(t *testing.T, db *sql.DB) {
	t.Helper()
	postgresProceduresOnce.Do(func() {
		for _, f := range postgresProcedureMigrations {
			path := filepath.Join("..", "migrations", "postgres", f)
			data, err := os.ReadFile(path)
			if err != nil {
				postgresProceduresErr = fmt.Errorf("read migration %s: %v", path, err)
				return
			}
			if _, err := db.Exec(string(data)); err != nil {
				postgresProceduresErr = fmt.Errorf("apply migration %s: %v", path, err)
				return
			}
		}
	})
	if postgresProceduresErr != nil {
		t.Fatalf("%v", postgresProceduresErr)
	}
}

// applyMySQLProcedures installs finalize_workflow_status against a MySQL
// test database. The migration files use the classic `DELIMITER //` idiom
// to let a CREATE PROCEDURE body contain internal semicolons; that's a
// mysql-CLI-only convention, so it must be parsed out here and each
// resulting statement sent individually via db.Exec (the go-sql-driver/mysql
// driver parses a single query's internal semicolons fine as long as it is
// not itself split on ';').
//
// As with applyPostgresProcedures, this only actually runs once per test
// binary (see postgresProceduresOnce doc comment) since every caller shares
// one CLEAT_TEST_MYSQL database and 004 is not safe to replay atop itself.
func applyMySQLProcedures(t *testing.T, db *sql.DB) {
	t.Helper()
	mysqlProceduresOnce.Do(func() {
		for _, f := range mysqlProcedureMigrations {
			path := filepath.Join("..", "migrations", "mysql", f)
			data, err := os.ReadFile(path)
			if err != nil {
				mysqlProceduresErr = fmt.Errorf("read migration %s: %v", path, err)
				return
			}
			for _, stmt := range splitMySQLDelimited(string(data)) {
				if strings.TrimSpace(stmt) == "" {
					continue
				}
				if _, err := db.Exec(stmt); err != nil {
					mysqlProceduresErr = fmt.Errorf("apply migration %s: %v\nstatement:\n%s", path, err, stmt)
					return
				}
			}
		}
	})
	if mysqlProceduresErr != nil {
		t.Fatalf("%v", mysqlProceduresErr)
	}
}

// applyMSSQLProcedures installs finalize_workflow_status against a SQL
// Server test database. The migration files contain no `GO` batch
// separators, so each file is a single T-SQL batch that can be sent as-is.
//
// As with applyPostgresProcedures, this only actually runs once per test
// binary (see postgresProceduresOnce doc comment) since every caller shares
// one CLEAT_TEST_MSSQL database and 004 is not safe to replay atop itself.
func applyMSSQLProcedures(t *testing.T, db *sql.DB) {
	t.Helper()
	mssqlProceduresOnce.Do(func() {
		for _, f := range mssqlProcedureMigrations {
			path := filepath.Join("..", "migrations", "mssql", f)
			data, err := os.ReadFile(path)
			if err != nil {
				mssqlProceduresErr = fmt.Errorf("read migration %s: %v", path, err)
				return
			}
			if _, err := db.Exec(string(data)); err != nil {
				mssqlProceduresErr = fmt.Errorf("apply migration %s: %v", path, err)
				return
			}
		}
	})
	if mssqlProceduresErr != nil {
		t.Fatalf("%v", mssqlProceduresErr)
	}
}

// splitMySQLDelimited splits MySQL SQL source that may contain `DELIMITER`
// directives (used to define stored procedures/functions whose body
// contains semicolons) into individual statements suitable for db.Exec.
// This mirrors what the `mysql` CLI does internally with DELIMITER, since
// the go-sql-driver/mysql driver has no equivalent client-side concept --
// each Exec call must receive exactly one complete statement.
func splitMySQLDelimited(src string) []string {
	delim := ";"
	var stmts []string
	var buf strings.Builder

	flush := func() {
		s := strings.TrimSpace(buf.String())
		s = strings.TrimSuffix(s, delim)
		s = strings.TrimSpace(s)
		if s != "" {
			stmts = append(stmts, s)
		}
		buf.Reset()
	}

	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "DELIMITER ") {
			flush()
			delim = strings.TrimSpace(trimmed[len("DELIMITER "):])
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		if strings.HasSuffix(strings.TrimSpace(buf.String()), delim) {
			flush()
		}
	}
	flush()
	return stmts
}
