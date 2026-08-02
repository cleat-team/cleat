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
	"os"
	"path/filepath"
	"strings"
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

// applyPostgresProcedures installs finalize_workflow_status (and friends)
// against a PostgreSQL test database. PostgreSQL's simple query protocol
// (used by lib/pq for a plain db.Exec) natively accepts a string containing
// multiple ';'-terminated statements, so each file can be sent as-is.
func applyPostgresProcedures(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, f := range postgresProcedureMigrations {
		applyProcedureFile(t, db, filepath.Join("..", "migrations", "postgres", f))
	}
}

// applyMySQLProcedures installs finalize_workflow_status against a MySQL
// test database. The migration files use the classic `DELIMITER //` idiom
// to let a CREATE PROCEDURE body contain internal semicolons; that's a
// mysql-CLI-only convention, so it must be parsed out here and each
// resulting statement sent individually via db.Exec (the go-sql-driver/mysql
// driver parses a single query's internal semicolons fine as long as it is
// not itself split on ';').
func applyMySQLProcedures(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, f := range mysqlProcedureMigrations {
		path := filepath.Join("..", "migrations", "mysql", f)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		for _, stmt := range splitMySQLDelimited(string(data)) {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("apply migration %s: %v\nstatement:\n%s", path, err, stmt)
			}
		}
	}
}

// applyMSSQLProcedures installs finalize_workflow_status against a SQL
// Server test database. The migration files contain no `GO` batch
// separators, so each file is a single T-SQL batch that can be sent as-is.
func applyMSSQLProcedures(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, f := range mssqlProcedureMigrations {
		applyProcedureFile(t, db, filepath.Join("..", "migrations", "mssql", f))
	}
}

func applyProcedureFile(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %s: %v", path, err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply migration %s: %v", path, err)
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
