package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// mockCheckDB — scripted mock driver for runCheckDB tests
// ---------------------------------------------------------------------------

type checkDBResult struct {
	cols   []string
	rows   [][]driver.Value
	err    error
	isPing bool // true if this entry is a Ping call
}

type checkDBMockConnector struct {
	script  []checkDBResult
	current *int
}

type checkDBMockDriver struct{}

func (d *checkDBMockDriver) Open(_ string) (driver.Conn, error) { return nil, fmt.Errorf("unused") }

func (c *checkDBMockConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &checkDBMockConn{script: c.script, current: c.current}, nil
}

func (c *checkDBMockConnector) Driver() driver.Driver { return &checkDBMockDriver{} }

type checkDBMockConn struct {
	script  []checkDBResult
	current *int
}

func (c *checkDBMockConn) Prepare(query string) (driver.Stmt, error) {
	if *c.current >= len(c.script) {
		return nil, fmt.Errorf("unexpected query #%d: %s", *c.current, query)
	}
	res := c.script[*c.current]
	*c.current++
	if res.err != nil {
		return nil, res.err
	}
	return &checkDBMockStmt{result: res}, nil
}

func (c *checkDBMockConn) Close() error              { return nil }
func (c *checkDBMockConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("no tx") }

func (c *checkDBMockConn) Ping(_ context.Context) error {
	if *c.current >= len(c.script) {
		return fmt.Errorf("unexpected ping at #%d", *c.current)
	}
	res := c.script[*c.current]
	if !res.isPing {
		return fmt.Errorf("expected Ping marker at position %d", *c.current)
	}
	*c.current++
	return res.err
}

type checkDBMockStmt struct {
	result checkDBResult
}

func (s *checkDBMockStmt) Close() error  { return nil }
func (s *checkDBMockStmt) NumInput() int { return -1 }
func (s *checkDBMockStmt) Exec(_ []driver.Value) (driver.Result, error) {
	return nil, fmt.Errorf("no exec")
}

func (s *checkDBMockStmt) Query(_ []driver.Value) (driver.Rows, error) {
	return &checkDBMockRows{cols: s.result.cols, rows: s.result.rows}, nil
}

type checkDBMockRows struct {
	cols   []string
	rows   [][]driver.Value
	pos    int
	closed bool
}

func (r *checkDBMockRows) Columns() []string { return r.cols }
func (r *checkDBMockRows) Close() error      { r.closed = true; return nil }

func (r *checkDBMockRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.pos]
	r.pos++
	for i, v := range row {
		if i < len(dest) {
			dest[i] = v
		}
	}
	return nil
}

// makePingResult creates a checkDBResult entry that represents a Ping call.
func makePingResult(err error) checkDBResult {
	return checkDBResult{isPing: true, err: err}
}

// makeQueryResult creates a checkDBResult for a single-row query.
// Pass vals=nil to indicate zero rows returned.
func makeQueryResult(cols []string, vals []driver.Value) checkDBResult {
	if vals == nil {
		return checkDBResult{cols: cols, rows: [][]driver.Value{}}
	}
	return checkDBResult{cols: cols, rows: [][]driver.Value{vals}}
}

// makeQueryError creates a checkDBResult that returns an error.
func makeQueryError(err error) checkDBResult {
	return checkDBResult{err: err}
}

// makeMultiRowResult creates a checkDBResult for a multi-row query.
func makeMultiRowResult(cols []string, rows [][]driver.Value) checkDBResult {
	return checkDBResult{cols: cols, rows: rows}
}

// tablesAllPresent returns one "this table exists" result per core table.
//
// The scripts in this file used to spell out thirteen of these by hand, which
// is why correcting coreTables to match the schema (cleat#1216) broke every one
// of them at once -- and why the next migration that adds a table would break
// them all again. The count is derived here for the same reason the list itself
// is derived from migrations/: one place that knows, instead of nineteen places
// that have to be remembered.
func tablesAllPresent() []checkDBResult {
	out := make([]checkDBResult, 0, len(coreTables))
	for range coreTables {
		out = append(out, makeQueryResult([]string{"count"}, []driver.Value{int64(1)}))
	}
	return out
}

// tablesAllAbsent is tablesAllPresent's mirror: information_schema reports 0
// for every core table. Same derivation, same reason.
func tablesAllAbsent() []checkDBResult {
	out := make([]checkDBResult, 0, len(coreTables))
	for range coreTables {
		out = append(out, makeQueryResult([]string{"count"}, []driver.Value{int64(0)}))
	}
	return out
}

// runCheckDBTest runs runCheckDB with a scripted mock and returns stdout and stderr.
func runCheckDBTest(t *testing.T, script []checkDBResult, args []string) (stdout, stderr string) {
	t.Helper()
	// The default posture is "exempt", so a test that says nothing about
	// row-level security gets the configuration cleatctl is meant to run on
	// and its script describes only the statements it cares about. Tests that
	// are about the posture set rlsPostureFn themselves, before calling this.
	if rlsPostureFn == nil {
		t.Fatal("rlsPostureFn is nil")
	}
	restore := rlsPostureFn
	t.Cleanup(func() { rlsPostureFn = restore })
	rlsPostureFn = stubPosture(rlsExempt, nil)

	return runCheckDBTestNoStub(t, script, args)
}

// runCheckDBTestNoStub leaves rlsPostureFn alone, for the tests whose subject IS
// the posture. They set it themselves before calling.
func runCheckDBTestNoStub(t *testing.T, script []checkDBResult, args []string) (stdout, stderr string) {
	t.Helper()
	current := 0
	connector := &checkDBMockConnector{script: script, current: &current}
	db := sql.OpenDB(connector)
	defer db.Close()

	return withExitPanicOutput(t, func() {
		runCheckDB(context.Background(), db, dialectPostgres, args)
	})
}

// stubPosture returns a replacement for rlsPostureFn. It takes a posture and an
// error rather than a database, because the thing under test in every caller is
// what runCheckDB DOES with the answer, not how the answer is derived --
// rlsPostureOf's own derivation is exercised against a real PostgreSQL.
func stubPosture(p rlsPosture, err error) func(context.Context, *sql.DB) (rlsPosture, []engine.RLSBypassReason, error) {
	return func(context.Context, *sql.DB) (rlsPosture, []engine.RLSBypassReason, error) {
		if err != nil {
			return rlsUnknown, nil, err
		}
		var reasons []engine.RLSBypassReason
		switch p {
		case rlsExempt:
			reasons = []engine.RLSBypassReason{{Kind: "superuser", Detail: "test role is a superuser"}}
		case rlsUnprotected:
			reasons = []engine.RLSBypassReason{{Kind: "no_policies", Detail: "no table in schema public has any policy"}}
		}
		return p, reasons, nil
	}
}

// =========================================================================
// Ping Tests
// =========================================================================

func TestRunCheckDB_PingFailure(t *testing.T) {
	script := []checkDBResult{
		makePingResult(fmt.Errorf("connection refused")),
	}
	_, stderr := runCheckDBTest(t, script, nil)
	if !strings.Contains(stderr, "DISCONNECTED") {
		t.Errorf("expected DISCONNECTED, got: %s", stderr)
	}
	if !strings.Contains(stderr, "connection refused") {
		t.Errorf("expected 'connection refused', got: %s", stderr)
	}
	if !strings.Contains(stderr, "UNHEALTHY") {
		t.Errorf("expected UNHEALTHY, got: %s", stderr)
	}
}

func TestRunCheckDB_PingSuccess(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}), // schema
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),           // instances (none)
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),  // event history
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // dead letters
	)
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "connected") {
		t.Errorf("expected 'connected', got: %s", stdout)
	}
	if !strings.Contains(stdout, "STATUS: healthy") {
		t.Errorf("expected 'STATUS: healthy', got: %s", stdout)
	}
}

// =========================================================================
// Schema Version Tests
// =========================================================================

func TestRunCheckDB_SchemaVersion_NoRows(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, nil), // no rows
		// rest doesn't matter for this path but must be provided
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "no migrations applied yet") {
		t.Errorf("expected 'no migrations applied yet', got: %s", stdout)
	}
}

func TestRunCheckDB_SchemaVersion_ReadError(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryError(fmt.Errorf("schema query timeout")), // schema error
		// rest
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	_, stderr := runCheckDBTest(t, script, nil)
	if !strings.Contains(stderr, "WARNING") || !strings.Contains(stderr, "schema version") {
		t.Errorf("expected WARNING about schema version, got: %s", stderr)
	}
}

func TestRunCheckDB_SchemaVersion_Valid(t *testing.T) {
	appliedAt := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult(
			[]string{"version", "applied_at"},
			[]driver.Value{"005_migration", &appliedAt},
		),
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(1024 * 1024)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "005_migration") {
		t.Errorf("expected version '005_migration', got: %s", stdout)
	}
	if !strings.Contains(stdout, "2025-01-15") {
		t.Errorf("expected applied_at date, got: %s", stdout)
	}
}

func TestRunCheckDB_SchemaVersion_VerboseNoRows(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, nil), // no rows
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, _ := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stdout, "(none)") {
		t.Errorf("expected '(none)' in verbose output, got: %s", stdout)
	}
}

// =========================================================================
// Table Accessibility Tests
// =========================================================================

func TestRunCheckDB_Tables_AllAccessible(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, _ := runCheckDBTest(t, script, nil)
	want := fmt.Sprintf("all %d accessible", len(coreTables))
	if !strings.Contains(stdout, want) {
		t.Errorf("expected %q, got: %s", want, stdout)
	}
}

// TestRunCheckDB_Tables_FallbackPath tests that when the information_schema
// query fails, the fallback SELECT COUNT(*) FROM <table> works.
func TestRunCheckDB_Tables_FallbackPath(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		// Table 1: information_schema error, fallback succeeds
		makeQueryError(fmt.Errorf("information_schema not available")),
		makeQueryResult([]string{"count"}, []driver.Value{int64(42)}),
		// Table 2: information_schema succeeds (count=1)
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "accessible") {
		t.Errorf("expected accessible count, got: %s", stdout)
	}
}

func TestRunCheckDB_Tables_AllMissing(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		// Every core table absent: information_schema returns count=0, no
		// fallback triggered. Built from len(coreTables) rather than restated,
		// so a migration that adds a table does not silently leave one entry
		// of this script unconsumed -- which would shift every result after it
		// and fail somewhere unrelated.
	}, tablesAllAbsent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, stderr := runCheckDBTest(t, script, nil)
	wantMissing := fmt.Sprintf("0 accessible, %d missing", len(coreTables))
	if !strings.Contains(stdout, wantMissing) {
		t.Errorf("expected %q in stdout, got: %s", wantMissing, stdout)
	}
	if !strings.Contains(stderr, "DEGRADED") {
		t.Errorf("expected DEGRADED in stderr, got: %s", stderr)
	}
}

func TestRunCheckDB_Tables_VerboseMissing(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		// 1 accessible, 1 missing
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 1 accessible
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 2 info_schema says 0
		makeQueryError(fmt.Errorf("relation does not exist")),        // table 2 fallback fails
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}), // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, stderr := runCheckDBTest(t, script, []string{"--verbose"})
	_ = stderr
	if !strings.Contains(stdout, "MISSING:") {
		t.Errorf("expected 'MISSING:' in verbose output, got: %s", stdout)
	}
}

// =========================================================================
// Workflow Instance Counts Tests
// =========================================================================

func TestRunCheckDB_Instances_WithStatuses(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, [][]driver.Value{
			{"running", int64(5)},
			{"completed", int64(10)},
			{"failed", int64(3)},
		}),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "INSTANCES: 18 total") {
		t.Errorf("expected 'INSTANCES: 18 total', got: %s", stdout)
	}
}

func TestRunCheckDB_Instances_VerboseStatuses(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, [][]driver.Value{
			{"running", int64(2)},
		}),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, _ := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stdout, "by status:") {
		t.Errorf("expected 'by status:' in verbose output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "running: 2") {
		t.Errorf("expected 'running: 2', got: %s", stdout)
	}
}

func TestRunCheckDB_Instances_QueryError(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeQueryError(fmt.Errorf("instance query error")), // instance query fails
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, stderr := runCheckDBTest(t, script, nil)

	// This test asserted the defect until cleat#1184: "should not contain
	// INSTANCES on query error". Saying nothing is exactly what made a
	// cleat_app connection report STATUS: healthy with the section missing.
	if strings.Contains(stdout, "INSTANCES:") {
		t.Errorf("the INSTANCES line belongs on stderr when the table cannot be read, "+
			"not stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "INSTANCES: UNREADABLE") {
		t.Errorf("an unreadable workflow_instances must be reported WITHOUT --verbose; "+
			"got stderr: %s", stderr)
	}
	if !strings.Contains(stderr, "instance query error") {
		t.Errorf("the underlying error must be named, got: %s", stderr)
	}
	if !strings.Contains(stderr, "DEGRADED") {
		t.Errorf("a table that could not be read is not a healthy database, got: %s", stderr)
	}
}

func TestRunCheckDB_Instances_QueryErrorVerbose(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeQueryError(fmt.Errorf("instance query error")),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	_, stderr := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stderr, "UNREADABLE") || !strings.Contains(stderr, "workflow_instances") {
		t.Errorf("expected the workflow_instances failure in stderr, got: %s", stderr)
	}
}

// =========================================================================
// Event History Tests
// =========================================================================

func TestRunCheckDB_EventHistory_Size(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(5 * 1024 * 1024)}), // 5MB
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "EVENT HISTORY: 5.0 MB") {
		t.Errorf("expected 'EVENT HISTORY: 5.0 MB', got: %s", stdout)
	}
}

func TestRunCheckDB_EventHistory_FallbackCount(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryError(fmt.Errorf("pg_column_size not available")),    // size query fails
		makeQueryResult([]string{"count"}, []driver.Value{int64(42)}), // fallback count
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	stdout, _ := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stdout, "42 rows") {
		t.Errorf("expected '42 rows' in verbose fallback, got: %s", stdout)
	}
}

// =========================================================================
// Dead Letter Tests
// =========================================================================

// The two DEAD LETTERS tests that stood here are deleted, and what they were
// testing is worth recording.
//
// TestRunCheckDB_DeadLetters_WithCount asserted that check-db prints
// "DEAD LETTERS: 3 workflows". It passed for years. That line has never once
// been printed by the command, on any database: it ran
// `SELECT COUNT(*) FROM workflow_dead_letters` behind `if err == nil`, and
// there is no such table -- dead_lettered is a status on workflow_instances.
// Against a real PostgreSQL the query returns
// `ERROR: relation "workflow_dead_letters" does not exist`, err is non-nil,
// and the branch is unreachable.
//
// It passed because the mock driver in this file DISCARDS the SQL string and
// answers from a positional script, so a statement naming a table that has
// never existed is indistinguishable from one that works. The test did not
// merely fail to catch the defect; it asserted the defective behaviour was
// correct, and would have had to be edited by anyone fixing it.
//
// This is the sharpest case of the thing cleat#1216 is about, and the reason
// the fix removes the query rather than repointing it: section 4's `by status`
// line already reports dead_lettered under the same --verbose gate. cleat#1216.

// =========================================================================
// Verbose / Summary Tests
// =========================================================================

func TestRunCheckDB_Verbose_JSONSummary(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	_, stderr := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stderr, "JSON summary") {
		t.Errorf("expected 'JSON summary' in stderr, got: %s", stderr)
	}
	if !strings.Contains(stderr, `"status"`) {
		t.Errorf("expected JSON with 'status', got: %s", stderr)
	}
}

func TestRunCheckDB_ShortVerboseFlag(t *testing.T) {
	script := append(append([]checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
	}, tablesAllPresent()...),
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	)
	_, stderr := runCheckDBTest(t, script, []string{"-v"})
	if !strings.Contains(stderr, "JSON summary") {
		t.Errorf("expected JSON summary with -v flag, got: %s", stderr)
	}
}

// =========================================================================
// Print Help Test
// =========================================================================

func TestPrintCheckDBUsage(t *testing.T) {
	stderr := captureStderr(t, func() {
		printCheckDBUsage()
	})
	if !strings.Contains(stderr, "Usage: cleatctl check-db") {
		t.Errorf("expected 'Usage: cleatctl check-db', got: %s", stderr)
	}
	if !strings.Contains(stderr, "--verbose") {
		t.Errorf("expected '--verbose' in usage, got: %s", stderr)
	}
	if !strings.Contains(stderr, "Database ping") {
		t.Errorf("expected 'Database ping' in usage, got: %s", stderr)
	}
	if !strings.Contains(stderr, "CLEAT_DB_URL") {
		t.Errorf("expected 'CLEAT_DB_URL' in usage, got: %s", stderr)
	}
}
