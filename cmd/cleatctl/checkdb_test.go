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
)

// ---------------------------------------------------------------------------
// mockCheckDB — scripted mock driver for runCheckDB tests
// ---------------------------------------------------------------------------

type checkDBResult struct {
	cols   []string
	rows   [][]driver.Value
	err    error
	isPing bool  // true if this entry is a Ping call
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

func (s *checkDBMockStmt) Close() error                       { return nil }
func (s *checkDBMockStmt) NumInput() int                      { return -1 }
func (s *checkDBMockStmt) Exec(_ []driver.Value) (driver.Result, error) { return nil, fmt.Errorf("no exec") }

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

// runCheckDBTest runs runCheckDB with a scripted mock and returns stdout and stderr.
func runCheckDBTest(t *testing.T, script []checkDBResult, args []string) (stdout, stderr string) {
	t.Helper()
	current := 0
	connector := &checkDBMockConnector{script: script, current: &current}
	db := sql.OpenDB(connector)
	defer db.Close()

	return withExitPanicOutput(t, func() {
		runCheckDB(context.Background(), db, args)
	})
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
	script := []checkDBResult{
		makePingResult(nil),                                                          // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}), // schema
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),                  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),                            // instances (none)
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),                   // event history
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),                  // dead letters
	}
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
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, nil), // no rows
		// rest doesn't matter for this path but must be provided
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "no migrations applied yet") {
		t.Errorf("expected 'no migrations applied yet', got: %s", stdout)
	}
}

func TestRunCheckDB_SchemaVersion_ReadError(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil),                                // ping ok
		makeQueryError(fmt.Errorf("schema query timeout")), // schema error
		// rest
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	_, stderr := runCheckDBTest(t, script, nil)
	if !strings.Contains(stderr, "WARNING") || !strings.Contains(stderr, "schema version") {
		t.Errorf("expected WARNING about schema version, got: %s", stderr)
	}
}

func TestRunCheckDB_SchemaVersion_Valid(t *testing.T) {
	appliedAt := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult(
			[]string{"version", "applied_at"},
			[]driver.Value{"005_migration", &appliedAt},
		),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(1024 * 1024)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "005_migration") {
		t.Errorf("expected version '005_migration', got: %s", stdout)
	}
	if !strings.Contains(stdout, "2025-01-15") {
		t.Errorf("expected applied_at date, got: %s", stdout)
	}
}

func TestRunCheckDB_SchemaVersion_VerboseNoRows(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, nil), // no rows
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stdout, "(none)") {
		t.Errorf("expected '(none)' in verbose output, got: %s", stdout)
	}
}

// =========================================================================
// Table Accessibility Tests
// =========================================================================

func TestRunCheckDB_Tables_AllAccessible(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "all 13 accessible") {
		t.Errorf("expected 'all 13 accessible', got: %s", stdout)
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
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
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
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		// All 13 tables: information_schema returns count=0, no fallback triggered
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, stderr := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "0 accessible, 13 missing") {
		t.Errorf("expected '0 accessible, 13 missing' in stdout, got: %s", stdout)
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
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),    // table 1 accessible
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),    // table 2 info_schema says 0
		makeQueryError(fmt.Errorf("relation does not exist")),            // table 2 fallback fails
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),     // table 13
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
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, [][]driver.Value{
			{"running", int64(5)},
			{"completed", int64(10)},
			{"failed", int64(3)},
		}),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "INSTANCES: 18 total") {
		t.Errorf("expected 'INSTANCES: 18 total', got: %s", stdout)
	}
}

func TestRunCheckDB_Instances_VerboseStatuses(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, [][]driver.Value{
			{"running", int64(2)},
		}),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stdout, "by status:") {
		t.Errorf("expected 'by status:' in verbose output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "running: 2") {
		t.Errorf("expected 'running: 2', got: %s", stdout)
	}
}

func TestRunCheckDB_Instances_QueryError(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeQueryError(fmt.Errorf("instance query error")), // instance query fails
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, nil)
	// Should not contain INSTANCES line
	if strings.Contains(stdout, "INSTANCES:") {
		t.Errorf("should not contain INSTANCES on query error, got: %s", stdout)
	}
}

func TestRunCheckDB_Instances_QueryErrorVerbose(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeQueryError(fmt.Errorf("instance query error")),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	_, stderr := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stderr, "WARNING") || !strings.Contains(stderr, "workflow_instances") {
		t.Errorf("expected WARNING about workflow_instances in stderr, got: %s", stderr)
	}
}

// =========================================================================
// Event History Tests
// =========================================================================

func TestRunCheckDB_EventHistory_Size(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(5 * 1024 * 1024)}), // 5MB
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, nil)
	if !strings.Contains(stdout, "EVENT HISTORY: 5.0 MB") {
		t.Errorf("expected 'EVENT HISTORY: 5.0 MB', got: %s", stdout)
	}
}

func TestRunCheckDB_EventHistory_FallbackCount(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryError(fmt.Errorf("pg_column_size not available")), // size query fails
		makeQueryResult([]string{"count"}, []driver.Value{int64(42)}), // fallback count
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	stdout, _ := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stdout, "42 rows") {
		t.Errorf("expected '42 rows' in verbose fallback, got: %s", stdout)
	}
}

// =========================================================================
// Dead Letter Tests
// =========================================================================

func TestRunCheckDB_DeadLetters_WithCount(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(3)}), // 3 dead letters
	}
	stdout, _ := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stdout, "DEAD LETTERS: 3 workflows") {
		t.Errorf("expected 'DEAD LETTERS: 3 workflows', got: %s", stdout)
	}
}

func TestRunCheckDB_DeadLetters_ZeroCount(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}), // zero dead letters
	}
	stdout, _ := runCheckDBTest(t, script, []string{"--verbose"})
	if strings.Contains(stdout, "DEAD LETTERS:") {
		t.Errorf("should not show DEAD LETTERS when count is 0, got: %s", stdout)
	}
}

// =========================================================================
// Verbose / Summary Tests
// =========================================================================

func TestRunCheckDB_Verbose_JSONSummary(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
	_, stderr := runCheckDBTest(t, script, []string{"--verbose"})
	if !strings.Contains(stderr, "JSON summary") {
		t.Errorf("expected 'JSON summary' in stderr, got: %s", stderr)
	}
	if !strings.Contains(stderr, `"status"`) {
		t.Errorf("expected JSON with 'status', got: %s", stderr)
	}
}

func TestRunCheckDB_ShortVerboseFlag(t *testing.T) {
	script := []checkDBResult{
		makePingResult(nil), // ping ok
		makeQueryResult([]string{"version", "applied_at"}, []driver.Value{"001", nil}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 1
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 2
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 3
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 4
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 5
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 6
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 7
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 8
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 9
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 10
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 11
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 12
		makeQueryResult([]string{"count"}, []driver.Value{int64(1)}),  // table 13
		makeMultiRowResult([]string{"status", "cnt"}, nil),
		makeQueryResult([]string{"size"}, []driver.Value{int64(0)}),
		makeQueryResult([]string{"count"}, []driver.Value{int64(0)}),
	}
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
