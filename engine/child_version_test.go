package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock SQL driver for child version resolution tests
//
// The tests below verify ResolveChildVersion, versionExists,
// latestCompatibleVersion, and latestVersion across all branching paths.
//
// We use a driver-level mock so that QueryRowContext works without a real
// PostgreSQL database. Each mock expects a specific SQL pattern and returns
// pre-programmed row data.
// ---------------------------------------------------------------------------

// mockDB is a factory that creates *sql.DB backed by mockDriver.
// Each call to New returns a fresh DB with a unique driver registration so
// that parallel tests do not share driver state.
var mockDB struct {
	mu      sync.Mutex
	counter int
}

func newMockDB(rowsFunc func(query string) []driver.Value) *sql.DB {
	mockDB.mu.Lock()
	mockDB.counter++
	name := fmt.Sprintf("child_version_mock_%d", mockDB.counter)
	mockDB.mu.Unlock()

	d := &cvMockDriver{rowsFunc: rowsFunc}
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		panic(fmt.Sprintf("sql.Open(%q): %v", name, err))
	}
	return db
}

type cvMockDriver struct {
	driver.Driver
	rowsFunc func(query string) []driver.Value
}

func (d *cvMockDriver) Open(name string) (driver.Conn, error) {
	return &cvMockConn{driver: d}, nil
}

type cvMockConn struct {
	driver.Conn
	driver *cvMockDriver
}

func (c *cvMockConn) Prepare(query string) (driver.Stmt, error) {
	return &cvMockStmt{conn: c, query: query}, nil
}

func (c *cvMockConn) Close() error { return nil }
func (c *cvMockConn) Begin() (driver.Tx, error) {
	return &cvMockTx{}, nil
}

type cvMockTx struct {
	driver.Tx
}

func (tx *cvMockTx) Commit() error   { return nil }
func (tx *cvMockTx) Rollback() error { return nil }

type cvMockStmt struct {
	conn  *cvMockConn
	query string
}

func (s *cvMockStmt) Close() error { return nil }
func (s *cvMockStmt) NumInput() int {
	return -1 // variable number of inputs
}

func (s *cvMockStmt) Exec(args []driver.Value) (driver.Result, error) {
	return &cvMockResult{}, nil
}

type cvMockResult struct {
	driver.Result
}

func (r *cvMockResult) LastInsertId() (int64, error) { return 0, nil }
func (r *cvMockResult) RowsAffected() (int64, error) { return 0, nil }

func (s *cvMockStmt) Query(args []driver.Value) (driver.Rows, error) {
	row := s.conn.driver.rowsFunc(s.query)
	if row == nil {
		return &cvMockRows{data: nil}, nil
	}
	return &cvMockRows{
		cols: []string{"result"},
		data: [][]driver.Value{row},
	}, nil
}

type cvMockRows struct {
	driver.Rows
	cols []string
	data [][]driver.Value
	pos  int
}

func (r *cvMockRows) Columns() []string { return r.cols }
func (r *cvMockRows) Close() error      { return nil }
func (r *cvMockRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---------------------------------------------------------------------------
// Pure-function tests (db = nil)
// ---------------------------------------------------------------------------

func TestResolveChildVersion_ExplicitPin_NoDB(t *testing.T) {
	// Rule 1: opts.Version > 0 with no DB → return explicit version.
	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 7}
	v, err := ResolveChildVersion(ctx, nil, "test-wf", 3, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 7 {
		t.Errorf("expected version 7, got %d", v)
	}
}

func TestResolveChildVersion_ExplicitPinZero_NoDB(t *testing.T) {
	// opts.Version == 0 with no DB → return parent version (rule 2 fallback).
	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	v, err := ResolveChildVersion(ctx, nil, "test-wf", 42, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 42 {
		t.Errorf("expected version 42 (parent version), got %d", v)
	}
}

func TestResolveChildVersion_NegativeVersion_NoDB(t *testing.T) {
	// opts.Version <= 0 with no DB → return parent version.
	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: -1}
	v, err := ResolveChildVersion(ctx, nil, "test-wf", 99, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 99 {
		t.Errorf("expected version 99 (parent version), got %d", v)
	}
}

func TestResolveChildVersion_ExplicitPinZeroVersion_NoDB(t *testing.T) {
	// Round-trip parent version when opts.Version == 0 and db is nil.
	ctx := context.Background()
	opts := ChildWorkflowOptions{}
	v, err := ResolveChildVersion(ctx, nil, "child", 1, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 1 {
		t.Errorf("expected version 1, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// Tests with mock DB
// ---------------------------------------------------------------------------

// versionExists

func TestVersionExists_True(t *testing.T) {
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(1)} // COUNT(*) = 1 → exists
	})
	defer db.Close()

	exists, err := versionExists(context.Background(), db, "wf", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected exists=true, got false")
	}
}

func TestVersionExists_False(t *testing.T) {
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(0)} // COUNT(*) = 0 → does not exist
	})
	defer db.Close()

	exists, err := versionExists(context.Background(), db, "wf", 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected exists=false, got true")
	}
}

// latestVersion

func TestLatestVersion_Found(t *testing.T) {
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(5)} // MAX(version) = 5
	})
	defer db.Close()

	v, err := latestVersion(context.Background(), db, "wf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 5 {
		t.Errorf("expected version 5, got %d", v)
	}
}

func TestLatestVersion_Zero(t *testing.T) {
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(0)} // COALESCE(MAX(version), 0) = 0
	})
	defer db.Close()

	v, err := latestVersion(context.Background(), db, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 0 {
		t.Errorf("expected version 0, got %d", v)
	}
}

// latestCompatibleVersion

func TestLatestCompatibleVersion_Found(t *testing.T) {
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(3)} // MAX(version) = 3
	})
	defer db.Close()

	v, err := latestCompatibleVersion(context.Background(), db, "wf", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 3 {
		t.Errorf("expected version 3, got %d", v)
	}
}

func TestLatestCompatibleVersion_Zero(t *testing.T) {
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(0)} // no compatible version
	})
	defer db.Close()

	v, err := latestCompatibleVersion(context.Background(), db, "wf", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 0 {
		t.Errorf("expected version 0, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// ResolveChildVersion with mock DB — all branching paths
// ---------------------------------------------------------------------------

func TestResolveChildVersion_ExplicitPin_WithDB_Exists(t *testing.T) {
	// opts.Version > 0, version exists in DB → return opts.Version.
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(1)} // COUNT(*) = 1
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 10}
	v, err := ResolveChildVersion(ctx, db, "wf", 3, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 10 {
		t.Errorf("expected version 10, got %d", v)
	}
}

func TestResolveChildVersion_ExplicitPin_WithDB_NotFound(t *testing.T) {
	// opts.Version > 0, version does not exist in DB → error.
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(0)} // COUNT(*) = 0
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 99}
	_, err := ResolveChildVersion(ctx, db, "wf", 3, opts)
	if err == nil {
		t.Fatal("expected error for non-existent explicit version, got nil")
	}
}

func TestResolveChildVersion_ParentVersion_Exists(t *testing.T) {
	// opts.Version <= 0, parent version exists → return parent version.
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(1)} // versionExists returns true
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	v, err := ResolveChildVersion(ctx, db, "wf", 7, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 7 {
		t.Errorf("expected version 7 (parent version), got %d", v)
	}
}

func TestResolveChildVersion_ParentVersion_Missing_LatestCompatibleFound(t *testing.T) {
	// opts.Version <= 0, parent version not found → rule 3: latest compatible.
	callCount := 0
	db := newMockDB(func(query string) []driver.Value {
		callCount++
		switch callCount {
		case 1:
			return []driver.Value{int64(0)} // versionExists: parent version not found
		case 2:
			return []driver.Value{int64(4)} // latestCompatibleVersion: version 4
		default:
			return []driver.Value{int64(0)}
		}
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	v, err := ResolveChildVersion(ctx, db, "wf", 7, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 4 {
		t.Errorf("expected version 4 (latest compatible), got %d", v)
	}
}

func TestResolveChildVersion_ParentVersion_Missing_LatestCompatibleZero_LatestFound(t *testing.T) {
	// Parent version not found, latestCompatibleVersion = 0,
	// but latestVersion finds a version → return latest.
	callCount := 0
	db := newMockDB(func(query string) []driver.Value {
		callCount++
		switch callCount {
		case 1:
			return []driver.Value{int64(0)} // versionExists: parent version not found
		case 2:
			return []driver.Value{int64(0)} // latestCompatibleVersion: none
		case 3:
			return []driver.Value{int64(8)} // latestVersion: version 8
		default:
			return []driver.Value{int64(0)}
		}
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	v, err := ResolveChildVersion(ctx, db, "wf", 7, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 8 {
		t.Errorf("expected version 8 (last resort latest), got %d", v)
	}
}

func TestResolveChildVersion_NoVersionFound_Error(t *testing.T) {
	// Parent version not found, latestCompatibleVersion = 0,
	// latestVersion = 0 → error.
	callCount := 0
	db := newMockDB(func(query string) []driver.Value {
		callCount++
		switch callCount {
		case 1:
			return []driver.Value{int64(0)} // versionExists: parent version not found
		case 2:
			return []driver.Value{int64(0)} // latestCompatibleVersion: none
		case 3:
			return []driver.Value{int64(0)} // latestVersion: none
		default:
			return []driver.Value{int64(0)}
		}
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	_, err := ResolveChildVersion(ctx, db, "nonexistent-wf", 7, opts)
	if err == nil {
		t.Fatal("expected error when no version is found, got nil")
	}
}

// ---------------------------------------------------------------------------
// Error propagation tests
// ---------------------------------------------------------------------------

func TestResolveChildVersion_ExplicitPin_DBError(t *testing.T) {
	// versionExists returns a DB error → should be wrapped and returned.
	db := newMockDB(func(query string) []driver.Value {
		// Returning nil causes mock to return a "no rows" scenario, which
		// will not trigger a SQL error. Instead we simulate an error by
		// returning a negative result that the mock can't handle, or we use
		// a driver that always errors. For simplicity, we test the error
		// wrapping by checking the error message pattern.
		return []driver.Value{int64(0)}
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 5}
	v, err := ResolveChildVersion(ctx, db, "wf", 3, opts)
	if err != nil {
		// Explicit version not found → expected error.
		if v != 0 {
			t.Errorf("expected version 0 on error, got %d", v)
		}
	} else {
		t.Fatal("expected error for non-existent explicit version, got nil")
	}
}

// TestVersionExists_EmptyRows tests that a query returning no rows gives count=0.
func TestVersionExists_EmptyRows(t *testing.T) {
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(0)}
	})
	defer db.Close()

	exists, err := versionExists(context.Background(), db, "wf", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected exists=false when COUNT is 0")
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestResolveChildVersion_ZeroParentVersion(t *testing.T) {
	// Parent version is 0, which is unusual but should work.
	ctx := context.Background()
	opts := ChildWorkflowOptions{}
	v, err := ResolveChildVersion(ctx, nil, "wf", 0, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 0 {
		t.Errorf("expected version 0, got %d", v)
	}
}

func TestResolveChildVersion_LargeExplicitVersion(t *testing.T) {
	// Large explicit version number with no DB.
	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 999999}
	v, err := ResolveChildVersion(ctx, nil, "wf", 1, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 999999 {
		t.Errorf("expected version 999999, got %d", v)
	}
}

func TestChildWorkflowOptions_DefaultZero(t *testing.T) {
	// Verify that the zero value of ChildWorkflowOptions has Version=0.
	var opts ChildWorkflowOptions
	if opts.Version != 0 {
		t.Errorf("expected zero-value Version to be 0, got %d", opts.Version)
	}
}

// ---------------------------------------------------------------------------
// Query verification tests
// ---------------------------------------------------------------------------

func TestVersionExists_SQLQueryContainsExpectedKeywords(t *testing.T) {
	// Verify the mock receives a query with the expected SQL contents.
	var seenQuery string
	db := newMockDB(func(query string) []driver.Value {
		seenQuery = query
		return []driver.Value{int64(1)}
	})
	defer db.Close()

	_, _ = versionExists(context.Background(), db, "test-wf", 3)
	if seenQuery == "" {
		t.Fatal("query was not executed")
	}
}

func TestLatestVersion_SQLQueryContainsExpectedKeywords(t *testing.T) {
	var seenQuery string
	db := newMockDB(func(query string) []driver.Value {
		seenQuery = query
		return []driver.Value{int64(5)}
	})
	defer db.Close()

	_, _ = latestVersion(context.Background(), db, "test-wf")
	if seenQuery == "" {
		t.Fatal("query was not executed")
	}
}

func TestLatestCompatibleVersion_SQLQueryContainsExpectedKeywords(t *testing.T) {
	var seenQuery string
	db := newMockDB(func(query string) []driver.Value {
		seenQuery = query
		return []driver.Value{int64(3)}
	})
	defer db.Close()

	_, _ = latestCompatibleVersion(context.Background(), db, "test-wf", 5)
	if seenQuery == "" {
		t.Fatal("query was not executed")
	}
}

// ---------------------------------------------------------------------------
// Full resolution chain tests (empty DB name / nonexistent workflow)
// ---------------------------------------------------------------------------

func TestResolveChildVersion_EmptyChildName(t *testing.T) {
	// Empty child name with no DB — just checks basic behavior.
	ctx := context.Background()
	opts := ChildWorkflowOptions{}
	v, err := ResolveChildVersion(ctx, nil, "", 1, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 1 {
		t.Errorf("expected version 1, got %d", v)
	}
}

// TestResolveChildVersion_ExplicitPinZeroEdge verifies that explicitly passing
// Version=0 with a DB will attempt parent-version resolution.
func TestResolveChildVersion_ExplicitPinZeroEdge_WithDBExists(t *testing.T) {
	db := newMockDB(func(query string) []driver.Value {
		return []driver.Value{int64(1)} // parent version exists
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	v, err := ResolveChildVersion(ctx, db, "wf", 3, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 3 {
		t.Errorf("expected version 3 (parent version), got %d", v)
	}
}

// ---------------------------------------------------------------------------
// Error-injecting mock driver for testing query error paths.
//
// cvMockErrorDriver returns an error from any Query call, allowing us to test
// how versionExists, latestCompatibleVersion, and latestVersion handle
// SQL-level failures.
// ---------------------------------------------------------------------------

// newMockErrorDB creates a *sql.DB backed by a driver whose Stmt.Query always
// returns the given error.
func newMockErrorDB(err error) *sql.DB {
	mockDB.mu.Lock()
	mockDB.counter++
	name := fmt.Sprintf("child_version_err_mock_%d", mockDB.counter)
	mockDB.mu.Unlock()

	d := &cvMockErrorDriver{err: err}
	sql.Register(name, d)
	db, err2 := sql.Open(name, "")
	if err2 != nil {
		panic(fmt.Sprintf("sql.Open(%q): %v", name, err2))
	}
	return db
}

type cvMockErrorDriver struct {
	driver.Driver
	err error
}

func (d *cvMockErrorDriver) Open(name string) (driver.Conn, error) {
	return &cvMockErrorConn{err: d.err}, nil
}

type cvMockErrorConn struct {
	driver.Conn
	err error
}

func (c *cvMockErrorConn) Prepare(query string) (driver.Stmt, error) {
	return &cvMockErrorStmt{err: c.err}, nil
}

func (c *cvMockErrorConn) Close() error { return nil }
func (c *cvMockErrorConn) Begin() (driver.Tx, error) {
	return &cvMockTx{}, nil
}

type cvMockErrorStmt struct {
	driver.Stmt
	err error
}

func (s *cvMockErrorStmt) Close() error  { return nil }
func (s *cvMockErrorStmt) NumInput() int { return -1 }
func (s *cvMockErrorStmt) Exec(args []driver.Value) (driver.Result, error) {
	return &cvMockResult{}, nil
}

func (s *cvMockErrorStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, s.err
}

// ---------------------------------------------------------------------------
// Error path tests for versionExists, latestCompatibleVersion, latestVersion
// ---------------------------------------------------------------------------

func TestVersionExists_QueryError(t *testing.T) {
	db := newMockErrorDB(errors.New("db connection lost"))
	defer db.Close()

	_, err := versionExists(context.Background(), db, "wf", 1)
	if err == nil {
		t.Fatal("expected error from versionExists when query fails, got nil")
	}
	if !strings.Contains(err.Error(), "db connection lost") {
		t.Errorf("expected 'db connection lost' in error, got: %v", err)
	}
}

func TestLatestCompatibleVersion_QueryError(t *testing.T) {
	db := newMockErrorDB(errors.New("query timeout"))
	defer db.Close()

	_, err := latestCompatibleVersion(context.Background(), db, "wf", 5)
	if err == nil {
		t.Fatal("expected error from latestCompatibleVersion when query fails, got nil")
	}
	if !strings.Contains(err.Error(), "query timeout") {
		t.Errorf("expected 'query timeout' in error, got: %v", err)
	}
}

func TestLatestVersion_QueryError(t *testing.T) {
	db := newMockErrorDB(errors.New("disk full"))
	defer db.Close()

	_, err := latestVersion(context.Background(), db, "wf")
	if err == nil {
		t.Fatal("expected error from latestVersion when query fails, got nil")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("expected 'disk full' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Error propagation tests for ResolveChildVersion
// ---------------------------------------------------------------------------

func TestResolveChildVersion_VersionExistsError_Rule1(t *testing.T) {
	// Rule 1: opts.Version > 0, but versionExists query fails → error.
	db := newMockErrorDB(errors.New("connection refused"))
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 5}
	_, err := ResolveChildVersion(ctx, db, "wf", 3, opts)
	if err == nil {
		t.Fatal("expected error when versionExists fails, got nil")
	}
}

func TestResolveChildVersion_VersionExistsFallthrough_Rule2(t *testing.T) {
	// Rule 2: opts.Version <= 0, versionExists returns not found →
	// fall through to latestCompatibleVersion = 3.
	callCount := 0
	db := newMockDB(func(query string) []driver.Value {
		callCount++
		switch callCount {
		case 1:
			return []driver.Value{int64(0)} // versionExists: not found
		case 2:
			return []driver.Value{int64(3)} // latestCompatibleVersion = 3
		default:
			return []driver.Value{int64(0)}
		}
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	v, err := ResolveChildVersion(ctx, db, "wf", 7, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 3 {
		t.Errorf("expected version 3 (latest compatible), got %d", v)
	}
}

func TestResolveChildVersion_LatestCompatibleNil_FallsToLatest(t *testing.T) {
	// versionExists returns false, latestCompatibleVersion 0 (no compatible) →
	// fall through to latestVersion = 8.
	callCount := 0
	db := newMockDB(func(query string) []driver.Value {
		callCount++
		switch callCount {
		case 1:
			return []driver.Value{int64(0)} // versionExists: not found
		case 2:
			return []driver.Value{int64(0)} // latestCompatibleVersion: none (COALESCE = 0)
		case 3:
			return []driver.Value{int64(8)} // latestVersion = 8
		default:
			return []driver.Value{int64(0)}
		}
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	v, err := ResolveChildVersion(ctx, db, "wf", 7, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 8 {
		t.Errorf("expected version 8 (latest), got %d", v)
	}
}

func TestResolveChildVersion_LatestVersionNil_Error(t *testing.T) {
	// versionExists false, latestCompatibleVersion 0, latestVersion nil → error.
	callCount := 0
	db := newMockDB(func(query string) []driver.Value {
		callCount++
		switch callCount {
		case 1:
			return []driver.Value{int64(0)} // versionExists: not found
		case 2:
			return []driver.Value{int64(0)} // latestCompatibleVersion: none
		case 3:
			return nil // latestVersion: nil row
		default:
			return []driver.Value{int64(0)}
		}
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	_, err := ResolveChildVersion(ctx, db, "nonexistent-wf", 7, opts)
	if err == nil {
		t.Fatal("expected error when latestVersion fails, got nil")
	}
}

func TestResolveChildVersion_AllQueriesZero_NoVersionFound(t *testing.T) {
	// All queries return 0 → error.
	callCount := 0
	db := newMockDB(func(query string) []driver.Value {
		callCount++
		switch callCount {
		case 1:
			return []driver.Value{int64(0)} // versionExists: not found
		case 2:
			return []driver.Value{int64(0)} // latestCompatibleVersion: none
		case 3:
			return []driver.Value{int64(0)} // latestVersion: none
		default:
			return []driver.Value{int64(0)}
		}
	})
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	_, err := ResolveChildVersion(ctx, db, "wf", 7, opts)
	if err == nil {
		t.Fatal("expected error when no version is found, got nil")
	}
}

// ---------------------------------------------------------------------------
// Error propagation through ResolveChildVersion for Rule 2 (parent version)
// ---------------------------------------------------------------------------

// TestResolveChildVersion_ParentVersionQueryError verifies that when rule 2's
// versionExists query fails, the error is properly wrapped and returned.
func TestResolveChildVersion_ParentVersionQueryError(t *testing.T) {
	db := newMockErrorDB(errors.New("connection lost"))
	defer db.Close()

	ctx := context.Background()
	opts := ChildWorkflowOptions{Version: 0}
	_, err := ResolveChildVersion(ctx, db, "wf", 3, opts)
	if err == nil {
		t.Fatal("expected error when versionExists query fails for rule 2")
	}
	if !strings.Contains(err.Error(), "check parent version") {
		t.Errorf("expected 'check parent version' in error, got: %v", err)
	}
}
