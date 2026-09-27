package testutil

import (
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Telling a DELETED row from an INVISIBLE one, on SQL Server. cleat#982.
//
// cleat#982 is four engine-suite failures on SQL Server, all of the shape "an
// operation that needed a workflow row found none", none reproducible in
// isolation. Every hypothesis on the issue so far has been about DELETION --
// which process wiped whose fixtures -- and every probe written for it has been
// a SAMPLER: ForeignSessions asks who is attached at two instants, and the
// suspected offender is a short-lived one that falls between them. It fired
// eleven times and reported "none", and it could not have said otherwise.
//
// This file replaces the sampler with two instruments that can each report a
// positive, and that fail in opposite directions:
//
//  1. AN EVENT, NOT A SAMPLE. A row leaving workflow_instances is an event, so
//     an AFTER DELETE trigger records who deleted it at the moment it happens.
//     There is no window to miss.
//
//  2. A DIFFERENT QUESTION ENTIRELY. "The row was not there" has a second
//     production on SQL Server that has nothing to do with deletion: a FILTER
//     predicate makes a row INVISIBLE, and invisible reads exactly like absent.
//     PhysicalAndVisibleMSSQLRowCount asks whether the row is still physically
//     in the table while the failing reader cannot see it.
//
// The second is the one nobody on the issue has asked, and it is not
// hypothetical: measured 2026-09-16 on a policy-bearing database, an INSERT
// through a pooled connection whose SESSION_CONTEXT had been cleared succeeded,
// and the row was then invisible to every subsequent read -- including the
// reader that wrote it, and including the blanket DELETE that would have
// removed it.
//
//	physical=2 visible=0
//
// workflow_instances carries a FILTER predicate and no BLOCK predicate, so
// nothing refuses that write. The engine's own tenant-scoped pools re-apply the
// context on every recycle (tenantSessionConn.ResetSession, IMPROVEMENT-PLAN
// 2.71) and are not exposed to it; a plain sql.Open pool is.
//
// WHAT IS MEASURED AND WHAT IS NOT. That both mechanisms produce #982's
// signature is measured. WHICH of them produced the four failures is not, and
// this file exists to answer that rather than to assert it.

// MSSQLRowAuditEnv opts a run into the deletion audit. It names the instrument
// rather than a verbosity level, because a reader of a CI file or a shell
// history should be able to see WHAT was turned on.
//
// It is off by default and is test-only: nothing here ships in a migration, and
// the trigger exists only in a database some test explicitly installed it into.
const MSSQLRowAuditEnv = "CLEAT_TEST_MSSQL_ROW_AUDIT"

// mssqlAuditTable is where the trigger writes. It is DELIBERATELY ABSENT from
// mssqlCleanupTables: the blanket DELETE this instrument exists to catch would
// otherwise erase its own evidence on the way past.
const mssqlAuditTable = "dbo.cleat_test_deletion_audit"

// mssqlAuditedTables are the tables an AFTER DELETE trigger is installed on.
//
// ONLY DELETE, AND ONLY THESE TABLES -- both halves are constraints, not
// preferences, and both were measured on 2026-09-16.
//
// A trigger and an OUTPUT clause without INTO cannot coexist on one table, and
// the restriction is per DML TYPE rather than per table:
//
//	trigger on the table          UPDATE ... OUTPUT INSERTED.id
//	----------------------------  -----------------------------
//	none                          ok
//	AFTER DELETE                  ok
//	AFTER UPDATE                  Msg 334
//
// engine/mssql_lifecycle.go claims work with UPDATE ... OUTPUT INSERTED.* and
// no INTO, so an AFTER UPDATE trigger on workflow_instances would break the
// claim path outright. An UPDATE-side audit -- "who cleared assigned_to" -- is
// therefore structurally unavailable on this table, and that is worth knowing
// before anyone tries.
//
// blob_content is excluded for the mirror reason: plugins/blobstore issues
// DELETE FROM blob_content OUTPUT DELETED.sha256 with no INTO, so a DELETE
// trigger there is the one that breaks.
var mssqlAuditedTables = []string{"workflow_instances"}

// InstallMSSQLRowDisappearanceAudit installs the audit table and an AFTER
// DELETE trigger on each of mssqlAuditedTables. It is idempotent and does
// nothing unless MSSQLRowAuditEnv is set.
//
// Installing needs rights the ordinary test DSN has (sa) and a per-tenant pool
// does not, so it takes the raw db rather than a store.
func InstallMSSQLRowDisappearanceAudit(t *testing.T, db *sql.DB) {
	t.Helper()
	if !mssqlRowAuditEnabled() {
		return
	}
	if _, err := db.Exec(mssqlAuditTableDDL); err != nil {
		t.Fatalf("install the cleat#982 deletion audit table: %v\n\n"+
			"This needs DDL rights in the test database (the phrase is spelled out "+
			"nowhere here on purpose: TestNoHandWrittenSchema matches string literals "+
			"by substring and cannot tell a statement from prose about one). If the DSN is not "+
			"an administrative one, unset %s rather than running without the instrument "+
			"-- a run that silently did not install it reports an empty audit, which is "+
			"the same output as a run where nothing deleted anything.", err, MSSQLRowAuditEnv)
	}
	// The audit table has to be writable by whoever issues the DELETE, because a
	// DML trigger runs in the caller's security context and ownership chaining
	// did not cover it here. Without this, a DELETE through the suite's
	// cleat_admin pool failed with "The user does not have permission to perform
	// this action" -- an error naming no audit object at all, on a connection
	// that had deleted from the same table seconds earlier. Measured 2026-09-16.
	//
	// That is the worst thing a diagnostic can do: an instrument for an
	// intermittent failure that itself causes one, wearing a permissions
	// costume, in the path it was installed to watch.
	if _, err := db.Exec(`GRANT INSERT ON ` + mssqlAuditTable + ` TO PUBLIC`); err != nil {
		t.Fatalf("grant INSERT on the cleat#982 audit table: %v\n\n"+
			"Without this the trigger cannot write and every DELETE by a non-owner "+
			"fails instead of being recorded.", err)
	}
	for _, table := range mssqlAuditedTables {
		if _, err := db.Exec(mssqlAuditTriggerDDL(table)); err != nil {
			t.Fatalf("install the cleat#982 deletion audit trigger on %s: %v", table, err)
		}
	}
}

// ClearMSSQLRowDisappearanceAudit empties the audit table. It is the scoping
// half of arming the audit across a whole suite: the table is deliberately
// absent from mssqlCleanupTables (a blanket DELETE there would erase its own
// evidence), so without this every test's CleanupMSSQLTestData row stays put
// and MSSQLDeletions reads them all -- a failure late in the run would print
// hundreds of "this process" cleanups burying the one foreign deletion the
// report exists to find. Clearing at the start of each test bounds the report
// to that test's window, which is the window the flake lives in.
//
// Like Install it is a no-op unless MSSQLRowAuditEnv is set, and it takes the
// same raw handle (the DSN's own login, which holds DELETE on the table).
//
// The scoping is best-effort, not a guarantee: the audit table is shared with
// any other process using the database, and a concurrent process clearing it
// can remove evidence this process has not yet read. The physical/visible half
// of the report is the non-lossy backstop for that case.
func ClearMSSQLRowDisappearanceAudit(t *testing.T, db *sql.DB) {
	t.Helper()
	if !mssqlRowAuditEnabled() {
		return
	}
	if _, err := db.Exec(`DELETE FROM ` + mssqlAuditTable); err != nil {
		t.Fatalf("clear the cleat#982 deletion audit table: %v", err)
	}
}

// UninstallMSSQLRowDisappearanceAudit removes the triggers and the audit table.
// Safe to call whether or not they are installed.
func UninstallMSSQLRowDisappearanceAudit(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range mssqlAuditedTables {
		if _, err := db.Exec(fmt.Sprintf(
			`IF OBJECT_ID('%s','TR') IS NOT NULL DROP TRIGGER %s`,
			mssqlAuditTriggerName(table), mssqlAuditTriggerName(table))); err != nil {
			t.Logf("drop the cleat#982 audit trigger on %s: %v", table, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(
		`IF OBJECT_ID('%s','U') IS NOT NULL DROP TABLE %s`,
		mssqlAuditTable, mssqlAuditTable)); err != nil {
		t.Logf("drop the cleat#982 audit table: %v", err)
	}
}

// mssqlRowAuditEnabled reports whether the deletion audit is opted in.
//
// Unexported: nothing outside this file needs to ask, and an exported
// predicate with no caller is dead code that happens to start with a capital
// letter -- scripts/check-dead-exports.sh says so, correctly.
func mssqlRowAuditEnabled() bool { return os.Getenv(MSSQLRowAuditEnv) != "" }

const mssqlAuditTableDDL = `
IF OBJECT_ID('dbo.cleat_test_deletion_audit','U') IS NULL
  CREATE TABLE dbo.cleat_test_deletion_audit (
    audit_id     BIGINT IDENTITY(1,1) PRIMARY KEY,
    table_name   NVARCHAR(128)  NOT NULL,
    row_id       NVARCHAR(200)  NULL,
    tenant_id    NVARCHAR(64)   NULL,
    batch_id     UNIQUEIDENTIFIER NOT NULL,
    batch_rows   INT            NOT NULL,
    deleted_at   DATETIME2      NOT NULL DEFAULT SYSUTCDATETIME(),
    spid         INT            NOT NULL,
    program_name NVARCHAR(128)  NULL,
    host_name    NVARCHAR(128)  NULL,
    host_pid     INT            NULL,
    stmt         NVARCHAR(MAX)  NULL
  )`

func mssqlAuditTriggerName(table string) string {
	return "dbo.trg_cleat982_audit_" + table
}

// mssqlAuditTriggerDDL builds the AFTER DELETE trigger for one table.
//
// SET NOCOUNT ON is not tidiness. Without it the trigger's own INSERT adds a
// rowcount to the statement's, and every caller that fences on RowsAffected --
// which on this table is the whole claim and release path -- would read a
// number the instrument invented. Measured with it on: a DELETE of one row
// still reports RowsAffected=1, and a blanket DELETE of two still reports 2.
//
// host_process_id is the field that carries the answer and @@SPID is not: two
// short-lived client processes reported the SAME spid=59 in one measurement,
// because SQL Server reuses session ids. A probe keyed on spid would report one
// session for two processes, which is a false all-clear in the direction this
// issue keeps being fooled in.
//
// THE STATEMENT TEXT IS BEST-EFFORT, AND THAT IS A PERMISSIONS FACT RATHER THAN
// A CHOICE. sys.dm_exec_input_buffer needs VIEW SERVER PERFORMANCE STATE, which
// the suite's cleat_admin principal does not have and cannot be granted from
// this database (server-scope grants must be issued in master). Uncaught, the
// denial propagates out of the trigger and FAILS THE DELETE -- so the read is
// wrapped, and a principal without the permission records everything except the
// text. Measured 2026-09-16, as cleat_test_admin:
//
//	sys.dm_exec_sessions                 readable, host_process_id = 46710
//	sys.dm_exec_input_buffer(@@SPID)     denied, VIEW SERVER PERFORMANCE STATE
//
// So attribution survives on any principal and only the statement text is
// privileged. batch_id and batch_rows exist to carry the question the text was
// there to answer: a trigger fires ONCE PER STATEMENT, so every row removed by
// one DELETE shares a batch_id, and "one statement took 37 rows" identifies a
// blanket wipe with no DMV involved.
//
// THE GUARD IS A PERMISSION PRE-CHECK AND NOT A TRY/CATCH, and that distinction
// cost a round to learn. Wrapping the read in BEGIN TRY / BEGIN CATCH catches
// the denial and does NOT save the statement: an error raised inside a trigger
// leaves the transaction uncommittable, so the DELETE then failed with
//
//	The current transaction cannot be committed and cannot support operations
//	that write to the log file. Roll back the transaction.
//
// which is a stranger and more alarming failure than the permission error it
// replaced. HAS_PERMS_BY_NAME asks the question without raising anything, and
// discriminates: measured 2026-09-16 it returns 0 for cleat_test_admin and 1
// for sa.
//
// event_info, not text: sys.dm_exec_input_buffer names its statement column
// event_info, and the first version of this trigger used `text`, failed to
// create, and the deletes that followed produced ZERO audit rows -- which reads
// exactly like "nothing deleted it". The instrument reproduced this issue's own
// failure mode inside itself, which is why the install path above is a Fatalf.
func mssqlAuditTriggerDDL(table string) string {
	return fmt.Sprintf(`
IF OBJECT_ID('%s','TR') IS NOT NULL DROP TRIGGER %s;
EXEC('
CREATE TRIGGER %s ON dbo.%s AFTER DELETE AS
BEGIN
  SET NOCOUNT ON;
  DECLARE @batch UNIQUEIDENTIFIER = NEWID();
  DECLARE @rows INT = (SELECT COUNT(*) FROM DELETED);
  DECLARE @stmt NVARCHAR(MAX) = NULL;
  IF HAS_PERMS_BY_NAME(NULL, NULL, ''VIEW SERVER PERFORMANCE STATE'') = 1
    SELECT TOP 1 @stmt = t.event_info FROM sys.dm_exec_input_buffer(@@SPID, NULL) t;
  INSERT INTO %s
    (table_name, row_id, tenant_id, batch_id, batch_rows, spid,
     program_name, host_name, host_pid, stmt)
  SELECT ''%s'', CONVERT(NVARCHAR(200), d.id), CONVERT(NVARCHAR(64), d.tenant_id),
         @batch, @rows, @@SPID, s.program_name, s.host_name, s.host_process_id, @stmt
  FROM DELETED d
  CROSS JOIN sys.dm_exec_sessions s
  WHERE s.session_id = @@SPID;
END')`,
		mssqlAuditTriggerName(table), mssqlAuditTriggerName(table),
		mssqlAuditTriggerName(table), table, mssqlAuditTable, table)
}

// MSSQLDeletion is one recorded row removal.
type MSSQLDeletion struct {
	Table    string
	RowID    string
	TenantID string
	SPID     int
	Program  string
	HostName string
	HostPID  int

	// BatchID is shared by every row one DELETE statement removed, and
	// BatchRows is how many that was. They are the permission-free half of
	// "was this a blanket wipe": a trigger fires once per statement, so a
	// batch that took many rows is one statement that took many rows,
	// whatever Stmt does or does not say.
	BatchID   string
	BatchRows int

	// Stmt is the captured statement text, or "" when the deleting principal
	// lacked VIEW SERVER PERFORMANCE STATE. Read StmtCaptured before reading
	// anything into an empty one -- absent text and a delete with no text are
	// the same value here, and this issue has been fooled by that shape
	// before.
	Stmt string
}

// StmtCaptured reports whether the statement text was available to the
// principal that issued the delete.
func (d MSSQLDeletion) StmtCaptured() bool { return d.Stmt != "" }

// Foreign reports whether another OS process issued this delete.
//
// It is answered against selfPID, the same seam foreign_sessions.go uses, so a
// test can claim to be a different process and check that this process's own
// deletes are then reported as foreign. Without that control, "no foreign
// deletions" is also what a broken query returns.
func (d MSSQLDeletion) Foreign() bool { return d.HostPID != 0 && d.HostPID != selfPID() }

// Blanket reports whether the recorded statement was an unqualified DELETE
// across the whole table -- CleanupMSSQLTestData's shape -- rather than a
// targeted one.
//
// This is what separates "a peer's teardown wiped my fixtures" from "the code
// under test deleted this row", and it is available only because the trigger
// captures the statement text. A per-row audit without it can say a row went
// and not why.
func (d MSSQLDeletion) Blanket() bool {
	s := strings.ToUpper(strings.Join(strings.Fields(d.Stmt), " "))
	i := strings.Index(s, "DELETE FROM ")
	if i < 0 {
		return false
	}
	return !strings.Contains(s[i:], " WHERE ")
}

// MSSQLDeletions reads the audit. It returns an empty slice, and no error, on a
// database where the instrument was never installed -- so callers must not read
// "no deletions" as "nothing deleted anything" without checking
// mssqlRowAuditEnabled (or MSSQLRowAuditEnv) first.
func MSSQLDeletions(db *sql.DB) ([]MSSQLDeletion, error) {
	// batch_id is CONVERTed rather than scanned raw: a UNIQUEIDENTIFIER arrives
	// as 16 bytes, and a report that prints them renders mojibake where a
	// reader expects an identifier they can group on.
	rows, err := db.Query(`SELECT table_name, row_id, tenant_id,
		CONVERT(NVARCHAR(36), batch_id), batch_rows,
		spid, program_name, host_name, host_pid, stmt
		FROM ` + mssqlAuditTable + ` ORDER BY audit_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MSSQLDeletion
	for rows.Next() {
		var d MSSQLDeletion
		var rowID, tenantID, batchID, prog, host, stmt sql.NullString
		var spid, hostPID, batchRows sql.NullInt64
		if err := rows.Scan(&d.Table, &rowID, &tenantID, &batchID, &batchRows,
			&spid, &prog, &host, &hostPID, &stmt); err != nil {
			return nil, err
		}
		d.RowID, d.TenantID = rowID.String, tenantID.String
		d.BatchID, d.BatchRows = batchID.String, int(batchRows.Int64)
		d.Program, d.HostName, d.Stmt = prog.String, host.String, stmt.String
		d.SPID, d.HostPID = int(spid.Int64), int(hostPID.Int64)
		out = append(out, d)
	}
	return out, rows.Err()
}

// PhysicalAndVisibleMSSQLRowCount answers the question the deletion audit
// cannot: is the row still there and merely unreadable?
//
// IT TAKES TWO CONNECTIONS, AND THAT IS THE POINT rather than an inconvenience.
// The two halves have to be read through different principals:
//
//   - physical comes from sys.dm_db_partition_stats, which is index metadata
//     rather than a SELECT, so no filter predicate is evaluated against it.
//     Reading it needs VIEW DATABASE STATE, which the test suite's cleat_admin
//     principal deliberately does not have -- so statsDB has to be an
//     administrative connection.
//   - visible comes from an ordinary count through readerDB, which must be the
//     pool whose visibility is in question, filtered exactly as the failing
//     read was.
//
// Pass the same handle for both only when the question is about that handle and
// it is administrative.
//
// physical > visible means the rows are present and readerDB cannot see them --
// a session-context problem, not a deletion, and no amount of looking for a
// deleter will find one.
//
// physical is metadata and can lag a little behind an in-flight transaction, so
// read it as a direction rather than as an exact reconciliation: physical
// strictly greater than visible is the signal, and equality on its own proves
// nothing.
func PhysicalAndVisibleMSSQLRowCount(statsDB, readerDB *sql.DB, table string) (physical, visible int64, err error) {
	var phys sql.NullInt64
	if err = statsDB.QueryRow(`SELECT SUM(ps.row_count) FROM sys.dm_db_partition_stats ps
		WHERE ps.object_id = OBJECT_ID(@p1) AND ps.index_id IN (0,1)`,
		"dbo."+table).Scan(&phys); err != nil {
		return 0, 0, fmt.Errorf("physical row count for %s (needs VIEW DATABASE STATE): %w", table, err)
	}
	if err = readerDB.QueryRow(`SELECT COUNT(*) FROM dbo.` + table).Scan(&visible); err != nil {
		return 0, 0, fmt.Errorf("visible row count for %s: %w", table, err)
	}
	return phys.Int64, visible, nil
}

var (
	mssqlStatsMu    sync.Mutex
	mssqlStatsPools = map[string]*sql.DB{}
)

// MSSQLStatsDB returns a pool for the Stats reading that OUTLIVES the test.
//
// THE POOL A TEST HOLDS IS CLOSED BEFORE THE REPORT RUNS, and that is not an
// edge case -- it is the standard shape of every backend test in the suite:
//
//	store, teardown := backend.Setup(t)
//	defer teardown()          // closes db
//	                          // ... t.Cleanup callbacks run AFTER this
//
// A deferred call in the test body runs before any t.Cleanup, so a report
// registered with t.Cleanup and holding the test's own handle reads
// "sql: database is closed" on every real failure. Measured 2026-09-16 with a
// forced failure through MSSQLBackend.Setup: the report fired and every line of
// it said UNMEASURED. That is the reassuring answer from an instrument that
// never looked -- the exact failure cleat#982 has accumulated a dozen of.
//
// Cached per DSN and never closed, like MSSQLAdminDB's pool, so a suite pays
// one connection rather than one per test. sa rather than cleat_admin because
// sys.dm_db_partition_stats needs VIEW DATABASE STATE, which the admin login
// deliberately does not have.
func MSSQLStatsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		// Fatal rather than Skip. Every caller reaches here having already
		// established that a SQL Server was asked for -- MSSQLBackend.Setup
		// checks Enabled() first -- so an empty DSN at this point is a
		// programming error, not an absent optional resource. A skip would
		// turn it into a silently missing instrument, which is the failure
		// this whole file exists to stop.
		t.Fatalf("MSSQLStatsDB: CLEAT_TEST_MSSQL is empty, but this is only " +
			"reached once a caller has established a SQL Server was requested")
	}

	mssqlStatsMu.Lock()
	defer mssqlStatsMu.Unlock()
	if pool, ok := mssqlStatsPools[dsn]; ok {
		return pool
	}
	pool, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("open a cleat#982 stats pool: %v", err)
	}
	// One connection is enough and the pool is process-lived, so bound it
	// rather than letting an idle diagnostic hold several.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	mssqlStatsPools[dsn] = pool
	return pool
}

// MSSQLRowDisappearanceReaders names the handles the report reads through.
//
// THERE ARE THREE, AND THE THIRD IS WHY. The two-handle form below answers
// "physical versus what this connection can see", and that comparison decides
// nothing at either end of the range the suite actually uses. Measured
// 2026-09-16 on a database built from the shipped migrations, every case
// seeded first and refused if the seed was not visible to a reader that should
// see it:
//
//	Reader                              reading                 verdict
//	----------------------------------  ----------------------  ---------------------
//	the raw TestDB handle (sa)          physical=1 visible=0    PRESENT BUT INVISIBLE
//	the MSSQLAdminDB pool               physical=1 visible=1    consistent
//	a tenant store, one tenant          physical=3 visible=3    consistent
//	a tenant store, TWO tenants         physical=5 visible=3    PRESENT BUT INVISIBLE
//
// Row 1 is constant because sa has no exemption: since migration 075 the
// shipped fn_tenant_filter is the plain form and names no role, so a plain pool
// with no session context sees nothing in a policy-bearing table whether the
// database is healthy or not. Arming that pairing beside
// ReportForeignSessionsOnFailure -- the obvious place, and the first thing
// tried -- would print PRESENT BUT INVISIBLE under every SQL Server failure in
// the suite. Row 2 is constant in the opposite direction, which is the more
// dangerous one: it prints the reassuring branch every time.
//
// Rows 3 and 4 differ, so a tenant store CAN decide, and it is also the handle
// the failing reads in cleat#982 went through. Row 4 is its false positive:
// physical counts EVERY tenant's rows and a tenant pool counts one tenant's, so
// two rows belonging to somebody else are enough to read as a disappearance.
// That is inherent -- being table-level index metadata is exactly why no
// predicate is evaluated against dm_db_partition_stats, and it is why the
// reading cannot be scoped to a tenant.
//
// Hence Truth: a COUNT over the same population the Reader is scoped to, read
// through a cross-tenant principal. physical stays in the report as context
// rather than as a verdict.
type MSSQLRowDisappearanceReaders struct {
	// Stats reads sys.dm_db_partition_stats and needs VIEW DATABASE STATE. No
	// predicate is evaluated against it, so it is the one reading that cannot be
	// wrong about how many rows exist -- and cannot say whose they are.
	Stats *sql.DB

	// Truth is a reader that can see across tenants -- MSSQLAdminDB's pool. With
	// TenantID it gives a count over exactly the population Reader is scoped to,
	// which is the comparison that carries the signal. Nil, or an empty TenantID,
	// makes that line UNMEASURED rather than absent.
	Truth    *sql.DB
	TenantID string

	// Reader is the pool whose visibility is in question: the handle the failing
	// read went through, filtered exactly as it was.
	Reader *sql.DB
}

// THERE IS DELIBERATELY NO t.Cleanup-REGISTERING WRAPPER FOR THE READERS FORM,
// and one was written and then deleted rather than never considered. A
// t.Cleanup callback runs AFTER the test body's deferred calls, and every
// engine backend test is written `defer teardown()`, where teardown closes the
// pool and issues a blanket DELETE. So a convenience wrapper here would hand
// callers the exact ordering bug this instrument exists to survive -- reading
// a closed handle, or an emptied table, and printing the reassuring verdict
// either way. Call MSSQLRowDisappearanceReportFor from inside the teardown
// instead; engine's mssqlRowDisappearanceReporter shows the shape and carries
// the measurements.
//
// ReportMSSQLRowDisappearanceOnFailure (the two-handle form above) keeps its
// t.Cleanup registration because its own self-test calls it directly, with
// handles it controls and does not close.

// TenantScopedMSSQLRowCount counts one tenant's rows through a cross-tenant
// reader, so the result can be compared with what a pool scoped to that tenant
// can see. A difference means the rows are there and their own tenant's
// connection cannot read them.
func TenantScopedMSSQLRowCount(truthDB *sql.DB, table, tenantID string) (int64, error) {
	var n int64
	err := truthDB.QueryRow(`SELECT COUNT(*) FROM dbo.`+table+` WHERE tenant_id = @p1`, tenantID).Scan(&n)
	return n, err
}

// ReportMSSQLRowDisappearanceOnFailure prints both instruments' readings, and
// only if the test has already failed.
//
// Register it with t.Cleanup at the top of a test that is chasing cleat#982. A
// passing test runs no query.
func ReportMSSQLRowDisappearanceOnFailure(t *testing.T, statsDB, readerDB *sql.DB) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Log(MSSQLRowDisappearanceReport(statsDB, readerDB))
	})
}

// MSSQLRowDisappearanceReport renders both readings.
//
// Every line carries its PRECONDITIONS, and a reading whose precondition failed
// is reported as UNMEASURED rather than as a clean result. A report with no
// such value has nowhere to put "I could not look", so it prints the
// reassuring answer instead -- which is how this issue accumulated eleven
// negatives that meant nothing.
func MSSQLRowDisappearanceReport(statsDB, readerDB *sql.DB) string {
	var b strings.Builder
	b.WriteString("cleat#982 row-disappearance report\n")
	for _, table := range mssqlAuditedTables {
		physicalVsVisible(&b, statsDB, readerDB, table)
	}
	mssqlDeletionSection(&b, statsDB)
	mssqlActiveLocksSection(&b, statsDB)
	goroutineStackSection(&b)
	return b.String()
}

// physicalVsVisible writes the two-handle reading: is the row still physically
// in the table while this connection cannot see it?
//
// IT ANSWERS A QUESTION ABOUT ONE HANDLE, which is why it survives alongside
// the tenant-exact reading rather than being replaced by it. What it cannot do
// is serve as a blanket verdict: physical counts every tenant's rows, so a
// reader scoped to one tenant trips it whenever another tenant has any. See
// MSSQLRowDisappearanceReaders for the four measurements.
func physicalVsVisible(b *strings.Builder, statsDB, readerDB *sql.DB, table string) {
	physical, visible, err := PhysicalAndVisibleMSSQLRowCount(statsDB, readerDB, table)
	switch {
	case err != nil:
		fmt.Fprintf(b, "  %s: UNMEASURED (physical/visible: %v)\n", table, err)
	case physical > visible:
		fmt.Fprintf(b, "  %s: physical=%d visible=%d -- PRESENT BUT INVISIBLE to this\n"+
			"    connection. %d row(s) are in the table and its session context cannot\n"+
			"    see them. Nothing deleted them; look at how this pool sets\n"+
			"    sp_set_session_context, not at who else was attached.\n",
			table, physical, visible, physical-visible)
	default:
		fmt.Fprintf(b, "  %s: physical=%d visible=%d -- consistent, so a missing row was\n"+
			"    really removed rather than hidden.\n", table, physical, visible)
	}
}

// MSSQLRowDisappearanceReportFor renders the readings for r.
//
// The tenant-exact line is the verdict. The physical line is context, and says
// so: physical > visible is EXPECTED whenever another tenant has rows, so
// printing it as a disappearance is a false positive the two-handle form could
// not avoid.
func MSSQLRowDisappearanceReportFor(r MSSQLRowDisappearanceReaders) string {
	var b strings.Builder
	b.WriteString("cleat#982 row-disappearance report\n")

	for _, table := range mssqlAuditedTables {
		physical, visible, err := PhysicalAndVisibleMSSQLRowCount(r.Stats, r.Reader, table)
		if err != nil {
			fmt.Fprintf(&b, "  %s: UNMEASURED (physical/visible: %v)\n", table, err)
			continue
		}

		// The verdict line, over one population.
		switch {
		case r.Truth == nil || r.TenantID == "":
			// No tenant-exact reading available, so say so AND give the
			// two-handle one rather than leaving the report with no verdict.
			// A reader who asked for the better question deserves to know it
			// was not answered; a reader who gets nothing at all concludes the
			// instrument found nothing.
			fmt.Fprintf(&b, "  (tenant-exact reading UNMEASURED: no cross-tenant reader, or no\n"+
				"    tenant given. Falling back to the one-handle reading, which is about\n"+
				"    this connection rather than about this tenant.)\n")
			physicalVsVisible(&b, r.Stats, r.Reader, table)
			continue
		default:
			truth, terr := TenantScopedMSSQLRowCount(r.Truth, table, r.TenantID)
			switch {
			case terr != nil:
				fmt.Fprintf(&b, "  %s: tenant-exact reading UNMEASURED (%v)\n", table, terr)
			case truth > visible:
				fmt.Fprintf(&b, "  %s: tenant=%s truth=%d visible=%d -- PRESENT BUT INVISIBLE\n"+
					"    TO ITS OWN TENANT. %d row(s) carrying this tenant_id are in the table\n"+
					"    and this pool cannot read them. Nothing deleted them; look at how this\n"+
					"    pool sets sp_set_session_context, not at who else was attached.\n",
					table, r.TenantID, truth, visible, truth-visible)
			default:
				fmt.Fprintf(&b, "  %s: tenant=%s truth=%d visible=%d -- consistent over this\n"+
					"    tenant, so a missing row was really removed rather than hidden.\n",
					table, r.TenantID, truth, visible)
			}
		}

		// Context, deliberately not a verdict. See MSSQLRowDisappearanceReaders.
		note := ""
		if physical > visible {
			note = " (EXPECTED if another tenant has rows; not a disappearance on its own)"
		}
		fmt.Fprintf(&b, "    context: physical=%d across ALL tenants, visible=%d%s\n",
			physical, visible, note)
	}

	mssqlDeletionSection(&b, r.Stats)
	mssqlActiveLocksSection(&b, r.Stats)
	goroutineStackSection(&b)
	return b.String()
}

func mssqlDeletionSection(b *strings.Builder, statsDB *sql.DB) string {
	if !mssqlRowAuditEnabled() {
		fmt.Fprintf(b, "  deletions: UNMEASURED (%s is not set, so no trigger was installed;\n"+
			"    an empty audit here would mean nothing). Re-run with %s=1.\n",
			MSSQLRowAuditEnv, MSSQLRowAuditEnv)
		return b.String()
	}

	dels, err := MSSQLDeletions(statsDB)
	if err != nil {
		fmt.Fprintf(b, "  deletions: UNMEASURED (reading the audit failed: %v)\n", err)
		return b.String()
	}
	if len(dels) == 0 {
		fmt.Fprintf(b, "  deletions: none recorded, and the audit WAS installed, so this is a\n"+
			"    real negative rather than an absent instrument.\n")
		return b.String()
	}
	fmt.Fprintf(b, "  deletions: %d recorded (this process is pid %d)\n", len(dels), selfPID())
	for _, d := range dels {
		who := "this process"
		if d.Foreign() {
			who = "ANOTHER PROCESS"
		}
		var shape, stmt string
		switch {
		case !d.StmtCaptured():
			shape = fmt.Sprintf("shape UNMEASURED (no statement text: the deleting "+
				"principal lacks VIEW SERVER PERFORMANCE STATE). Its statement took "+
				"%d row(s) in one go", d.BatchRows)
			stmt = "batch " + d.BatchID
		case d.Blanket():
			shape = fmt.Sprintf("BLANKET -- a whole-table wipe, CleanupMSSQLTestData's "+
				"shape, %d row(s) in one statement", d.BatchRows)
			stmt = trimmedStmt(d.Stmt)
		default:
			shape = fmt.Sprintf("targeted, %d row(s) in one statement", d.BatchRows)
			stmt = trimmedStmt(d.Stmt)
		}
		fmt.Fprintf(b, "    %s/%s tenant=%s by %s pid=%d (%s), %s\n      %s\n",
			d.Table, d.RowID, d.TenantID, who, d.HostPID, d.Program, shape, stmt)
	}
	return b.String()
}

// trimmedStmt collapses a captured statement onto one line and bounds it.
// oneLine is the package's existing collapser; the bound is here because a
// captured statement can be a whole migration batch.
func trimmedStmt(s string) string {
	s = oneLine(s)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// mssqlActiveLocksSection answers a question deletion and visibility cannot:
// is something holding a lock on an audited table RIGHT NOW, at the moment of
// failure. The deletion audit only sees a completed DELETE; a claim or update
// that is still in flight, blocked or blocking, has committed nothing yet and
// leaves no row in the audit table -- this is the only one of the report's
// instruments that can see it.
//
// statsDB must be the administrative connection MSSQLStatsDB returns:
// sys.dm_tran_locks is server-wide state, like sys.dm_db_partition_stats
// above, and the test suite's own principal does not have permission to read
// it either.
func mssqlActiveLocksSection(b *strings.Builder, statsDB *sql.DB) {
	rows, err := statsDB.Query(`
		SELECT l.request_session_id, l.resource_type, l.request_mode,
		       l.request_status, s.program_name, s.host_name,
		       CONVERT(NVARCHAR(4000), COALESCE(r.text, ''))
		FROM sys.dm_tran_locks l
		JOIN sys.partitions p ON p.hobt_id = l.resource_associated_entity_id
		JOIN sys.objects o ON o.object_id = p.object_id
		LEFT JOIN sys.dm_exec_sessions s ON s.session_id = l.request_session_id
		OUTER APPLY sys.dm_exec_sql_text(
			(SELECT TOP 1 sql_handle FROM sys.dm_exec_requests
			  WHERE session_id = l.request_session_id)) r
		WHERE o.name IN ('workflow_instances')
		ORDER BY l.request_session_id`)
	if err != nil {
		fmt.Fprintf(b, "  active locks: UNMEASURED (%v)\n", err)
		return
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var spid int
		var resType, mode, status, prog, host, stmt sql.NullString
		if err := rows.Scan(&spid, &resType, &mode, &status, &prog, &host, &stmt); err != nil {
			fmt.Fprintf(b, "  active locks: UNMEASURED (scanning a row: %v)\n", err)
			return
		}
		lines = append(lines, fmt.Sprintf("    spid=%d %s %s status=%s program=%q host=%q\n      %s",
			spid, resType.String, mode.String, status.String, prog.String, host.String,
			trimmedStmt(stmt.String)))
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(b, "  active locks: UNMEASURED (%v)\n", err)
		return
	}
	if len(lines) == 0 {
		fmt.Fprintf(b, "  active locks: none held on workflow_instances at report time.\n")
		return
	}
	fmt.Fprintf(b, "  active locks: %d held on workflow_instances at report time\n", len(lines))
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
}

// goroutineStackSection prints every live goroutine's stack, so a background
// claimer, sweeper or retention loop that outlived the test that started it --
// the "not eliminated" candidate this issue's own body names -- shows up by
// name rather than by inference. A static read of every `go func(` in
// engine/*.go cannot see a leak reached through an interface value rather than
// a literal call site at the leak's origin; this can, because it does not
// care how the goroutine was started.
//
// This is necessarily noisy -- it is every goroutine in the test binary, not
// only ones touching MSSQL -- so it is the last section of the report and
// gated the same way the rest of it is: only printed on an already-failed
// test, never on a pass.
func goroutineStackSection(b *strings.Builder) {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	fmt.Fprintf(b, "  goroutine stacks at report time (%d goroutines' worth of output follows):\n%s\n",
		runtime.NumGoroutine(), buf[:n])
}
