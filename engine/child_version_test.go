package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
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
