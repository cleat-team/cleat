package engine

// Installs the finalize_workflow_status stored procedure/function (and any
// other migrations/<dialect>/003_procedures.sql + 004_*.sql content) into a
// test database.
//
// engine/testutil now applies every file under migrations/<dialect>/,
// 003/004 included, via migration.Runner -- so on a db built through
// testutil.TestDB/SetupFullSchema (which is every current caller here) the
// procedure already exists by the time a test reaches this function, and the
// re-apply below is a no-op in effect, not merely in intent: 003's
// `DROP FUNCTION IF EXISTS` / MySQL's `DROP PROCEDURE IF EXISTS` / MSSQL's
// `CREATE OR ALTER PROCEDURE` all make replaying 003 then 004 safe against a
// database where 004 is already installed, which is what makes reapplying
// here harmless rather than merely convenient. Left in place (rather than
// deleted) because it is still what makes finalize_workflow_status exist for
// any caller that builds its db some other way; if you're tracing why the
// procedure exists at all on a testutil-built db, the answer is
// migration.Runner, not this file.
//
// This reads the actual production migration files from disk (under
// ../migrations/<dialect>/) rather than duplicating their SQL, so tests
// exercise the real, shipped fenced logic -- not a hand-rolled substitute
// that could drift from production.

import (
	"context"
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
//
// Hand-maintained, and therefore checked: TestProcedureMigrationListsAreComplete
// fails when a migration defines the routine and is not listed here. A missing
// entry is silent in the worst way -- every test that goes through
// PostgresBackend.Setup keeps running against the LAST listed version of the
// procedure, so a change to it is not merely untested, it is actively
// contradicted by a suite that still passes. That happened: the query_state
// fix below was written, applied to a real database, verified over HTTP, and
// its own engine test still failed, because the harness was running 004.
var postgresProcedureMigrations = []string{
	"003_procedures.sql",
	"004_fix_finalize_workflow_status_fence.sql",
	"043_query_state_on_suspension.sql",
	"044_child_does_not_rewrite_parent_event.sql",
	"047_a_signal_delivered_mid_segment_wakes_the_workflow.sql",
	"049_a_burst_wakes_finalize_on_progress.sql",
	"050_the_idempotency_write_needs_the_tenant.sql",
	"053_the_finalize_procedure_stops_writing_the_result_column.sql",
}

var mysqlProcedureMigrations = []string{
	"003_procedures.sql",
	"004_fix_finalize_workflow_status_fence.sql",
	"042_query_state_on_suspension.sql",
	"043_child_does_not_rewrite_parent_event.sql",
	"046_a_signal_delivered_mid_segment_wakes_the_workflow.sql",
	"048_a_burst_wakes_finalize_on_progress.sql",
	"049_the_idempotency_write_needs_the_tenant.sql",
	"053_the_finalize_procedure_stops_writing_the_result_column.sql",
}

var mssqlProcedureMigrations = []string{
	"003_procedures.sql",
	"004_fix_finalize_workflow_status_fence.sql",
	"046_query_state_on_suspension.sql",
	"047_child_does_not_rewrite_parent_event.sql",
	"050_a_signal_delivered_mid_segment_wakes_the_workflow.sql",
	"052_a_burst_wakes_finalize_on_progress.sql",
	"053_the_idempotency_write_needs_the_tenant.sql",
	"056_the_finalize_procedure_stops_writing_the_result_column.sql",
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
		// On ONE connection, with search_path set, because these files no
		// longer say which schema they build into. Since cleat#1287 they ask
		// -- the runner answers from --schema, the initdb script answers with
		// PGOPTIONS, and this is the third applier in the tree and has to
		// answer too.
		//
		// It matters here more than anywhere, because 003 opens with
		// `DROP FUNCTION IF EXISTS finalize_workflow_status(...)`. DROP
		// resolves through search_path and finds the real function in public;
		// the CREATE that follows lands in the FIRST schema of search_path.
		// Under the default `"$user", public` as role "cleat" -- which is what
		// docker-compose.cluster.yml connects as, against a database where 001
		// has created a schema of that name -- those are two different
		// schemas, so replaying these files MOVES the function out of public.
		// Nothing errors. The next test that looks for it in public reports
		// that the migrations and the database disagree about what exists,
		// which is true and says nothing about the cause.
		//
		// Measured in CI on cleat#1287: this was order-dependent, so the
		// routine-drift test failed in a full run and passed alone.
		//
		// One connection rather than a pool Exec, for the reason
		// migration.Runner.session gives: a bare db.Exec takes whatever
		// connection is free, so the SET would apply to one and the files to
		// whichever others the pool hands out.
		conn, err := db.Conn(context.Background())
		if err != nil {
			postgresProceduresErr = fmt.Errorf("pin a connection: %v", err)
			return
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(),
			`SET search_path = public, pg_temp`); err != nil {
			postgresProceduresErr = fmt.Errorf("pin search_path: %v", err)
			return
		}
		for _, f := range postgresProcedureMigrations {
			path := filepath.Join("..", "migrations", "postgres", f)
			data, err := os.ReadFile(path)
			if err != nil {
				postgresProceduresErr = fmt.Errorf("read migration %s: %v", path, err)
				return
			}
			if _, err := conn.ExecContext(context.Background(), string(data)); err != nil {
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
// SQL Server replays only the LAST entry, where PostgreSQL and MySQL replay
// the whole list.
//
// SQL Server binds column names when it compiles a procedure body; PostgreSQL
// and MySQL do not. So a superseded definition stops being replayable the
// moment a column it names is dropped, even though it was correct against the
// schema of its own day. cleat#1049 dropped idempotency_keys.result and
// 003_procedures.sql went from redundant to fatal:
//
//	apply migration ../migrations/mssql/003_procedures.sql:
//	mssql: Invalid column name 'result'.
//
// Applying only the last entry is not a shortcut around that error, it is what
// this function was always for: the list is ordered, every entry redefines
// finalize_workflow_status in full, and only the last one decides what the
// database ends up with. On SQL Server every listed file defines that one
// routine and nothing else, so nothing is lost by skipping the rest -- which is
// NOT true of PostgreSQL, whose 003 also defines flush_event_step and
// batch_flush_events. That asymmetry is why this is a per-dialect change rather
// than the same edit in three places.
//
// TestProcedureMigrationListsAreComplete still guards the list itself, so a new
// procedure migration that is not listed is still caught -- and it is now the
// only thing standing between a new definition and a suite that silently tests
// the previous one.
func applyMSSQLProcedures(t *testing.T, db *sql.DB) {
	t.Helper()
	mssqlProceduresOnce.Do(func() {
		if len(mssqlProcedureMigrations) == 0 {
			mssqlProceduresErr = fmt.Errorf("mssqlProcedureMigrations is empty")
			return
		}
		for _, f := range mssqlProcedureMigrations[len(mssqlProcedureMigrations)-1:] {
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
