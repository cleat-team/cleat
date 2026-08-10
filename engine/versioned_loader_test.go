package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Mock driver.Connector for WorkflowLoader DB tests
// ---------------------------------------------------------------------------

type mockVLConnector struct {
	rows     [][]driver.Value
	queryErr error
	execRes  driver.Result
	execErr  error
}

func (c *mockVLConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return &mockVLConn{c: c}, nil
}

func (c *mockVLConnector) Driver() driver.Driver {
	return &mockVLDriver{}
}

type mockVLDriver struct{}

func (d *mockVLDriver) Open(name string) (driver.Conn, error) {
	return &mockVLConn{}, nil
}

type mockVLConn struct {
	c *mockVLConnector
}

func (c *mockVLConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("Prepare not implemented")
}

func (c *mockVLConn) Close() error { return nil }

func (c *mockVLConn) Begin() (driver.Tx, error) {
	return nil, errors.New("not supported")
}

func (c *mockVLConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.c.queryErr != nil {
		return nil, c.c.queryErr
	}
	colCount := 0
	if len(c.c.rows) > 0 {
		colCount = len(c.c.rows[0])
	}
	return &mockVLRows{rows: c.c.rows, columns: make([]string, colCount)}, nil
}

func (c *mockVLConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.c.execErr != nil {
		return nil, c.c.execErr
	}
	return c.c.execRes, nil
}

type mockVLResult struct {
	rowsAffected int64
}

func (r *mockVLResult) LastInsertId() (int64, error) { return 0, nil }
func (r *mockVLResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type mockVLRows struct {
	rows    [][]driver.Value
	pos     int
	columns []string
}

func (r *mockVLRows) Columns() []string { return r.columns }
func (r *mockVLRows) Close() error      { return nil }

func (r *mockVLRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

func newMockVLDB(c *mockVLConnector) *sql.DB {
	return sql.OpenDB(c)
}

// ---------------------------------------------------------------------------
// Load tests
// ---------------------------------------------------------------------------

func TestWorkflowLoader_Load_NotFound(t *testing.T) {
	conn := &mockVLConnector{} // empty rows → io.EOF → sql.ErrNoRows
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	_, err := l.Load(context.Background(), "test-wf", 1)
	if err == nil {
		t.Fatal("expected error for not-found workflow def")
	}
	// Load wraps the not-found into a descriptive error without %w, so
	// errors.Is won't reach through. Just verify we got a non-nil error.
}

func TestWorkflowLoader_Load_DBError(t *testing.T) {
	conn := &mockVLConnector{queryErr: errors.New("connection refused")}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	_, err := l.Load(context.Background(), "test-wf", 1)
	if err == nil {
		t.Fatal("expected error from DB")
	}
}

// ---------------------------------------------------------------------------
// Deploy tests
// ---------------------------------------------------------------------------

func TestWorkflowLoader_Deploy_Success(t *testing.T) {
	conn := &mockVLConnector{
		execRes: &mockVLResult{rowsAffected: 1},
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	err := l.Deploy(context.Background(), "wf", 1, []byte{0x00, 0x61, 0x73, 0x6d}, map[string]string{"p": "v1"}, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWorkflowLoader_Deploy_ExecError(t *testing.T) {
	conn := &mockVLConnector{
		execErr: errors.New("disk full"),
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	err := l.Deploy(context.Background(), "wf", 1, []byte("wasm"), nil, 0)
	if err == nil {
		t.Fatal("expected exec error")
	}
}

func TestWorkflowLoader_Deploy_NilPluginDeps(t *testing.T) {
	conn := &mockVLConnector{
		execRes: &mockVLResult{rowsAffected: 1},
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	err := l.Deploy(context.Background(), "wf", 1, []byte("wasm"), nil, 0)
	if err != nil {
		t.Fatalf("unexpected error with nil plugin deps: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Deprecate tests
// ---------------------------------------------------------------------------

func TestWorkflowLoader_Deprecate_Success(t *testing.T) {
	conn := &mockVLConnector{
		execRes: &mockVLResult{rowsAffected: 1},
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	err := l.Deprecate(context.Background(), "wf", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWorkflowLoader_Deprecate_NotFound(t *testing.T) {
	conn := &mockVLConnector{
		execRes: &mockVLResult{rowsAffected: 0},
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	err := l.Deprecate(context.Background(), "wf", 1)
	if err == nil {
		t.Fatal("expected error for not found")
	}
}

func TestWorkflowLoader_Deprecate_ExecError(t *testing.T) {
	conn := &mockVLConnector{
		execErr: errors.New("disk full"),
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	err := l.Deprecate(context.Background(), "wf", 1)
	if err == nil {
		t.Fatal("expected exec error")
	}
}

// ---------------------------------------------------------------------------
// ListVersions tests
// ---------------------------------------------------------------------------

func TestWorkflowLoader_ListVersions_Success(t *testing.T) {
	conn := &mockVLConnector{
		rows: [][]driver.Value{
			{"wf", int64(2), []byte{0x00, 0x61, 0x73, 0x6d}, int64(1), `{"p":"v"}`, int64(1), time.Now(), false},
			{"wf", int64(1), []byte{0x00, 0x61, 0x73, 0x6d}, int64(1), nil, int64(0), time.Now(), true},
		},
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	defs, err := l.ListVersions(context.Background(), "wf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 defs, got %d", len(defs))
	}
	if defs[0].Version != 2 {
		t.Errorf("expected first def version 2 (descending), got %d", defs[0].Version)
	}
	if defs[0].Deprecated {
		t.Errorf("expected first def not deprecated")
	}
	if defs[1].Version != 1 {
		t.Errorf("expected second def version 1, got %d", defs[1].Version)
	}
	if !defs[1].Deprecated {
		t.Errorf("expected second def deprecated=true")
	}
}

func TestWorkflowLoader_ListVersions_Empty(t *testing.T) {
	conn := &mockVLConnector{} // no rows
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	defs, err := l.ListVersions(context.Background(), "wf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(defs) != 0 {
		t.Errorf("expected empty defs, got %d", len(defs))
	}
}

func TestWorkflowLoader_ListVersions_QueryError(t *testing.T) {
	conn := &mockVLConnector{queryErr: errors.New("bad query")}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	_, err := l.ListVersions(context.Background(), "wf")
	if err == nil {
		t.Fatal("expected query error")
	}
}

// ---------------------------------------------------------------------------
// ActiveVersions tests
// ---------------------------------------------------------------------------

func TestWorkflowLoader_ActiveVersions_Success(t *testing.T) {
	conn := &mockVLConnector{
		rows: [][]driver.Value{
			{"wf-a", int64(1)},
			{"wf-a", int64(3)},
			{"wf-b", int64(2)},
		},
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	active, err := l.ActiveVersions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 def names, got %d", len(active))
	}
	if len(active["wf-a"]) != 2 || active["wf-a"][0] != 1 || active["wf-a"][1] != 3 {
		t.Errorf("unexpected wf-a versions: %v", active["wf-a"])
	}
	if len(active["wf-b"]) != 1 || active["wf-b"][0] != 2 {
		t.Errorf("unexpected wf-b versions: %v", active["wf-b"])
	}
}

func TestWorkflowLoader_ActiveVersions_QueryError(t *testing.T) {
	conn := &mockVLConnector{queryErr: errors.New("bad query")}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	_, err := l.ActiveVersions(context.Background())
	if err == nil {
		t.Fatal("expected query error")
	}
}

// ---------------------------------------------------------------------------
// ResolveLatestVersion tests
// ---------------------------------------------------------------------------

func TestWorkflowLoader_ResolveLatestVersion_Found(t *testing.T) {
	conn := &mockVLConnector{
		rows: [][]driver.Value{{int64(5)}},
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	v, err := l.ResolveLatestVersion(context.Background(), "wf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 5 {
		t.Errorf("expected version 5, got %d", v)
	}
}

func TestWorkflowLoader_ResolveLatestVersion_None(t *testing.T) {
	conn := &mockVLConnector{
		rows: [][]driver.Value{{int64(0)}},
	}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	v, err := l.ResolveLatestVersion(context.Background(), "wf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 0 {
		t.Errorf("expected version 0, got %d", v)
	}
}

func TestWorkflowLoader_ResolveLatestVersion_QueryError(t *testing.T) {
	conn := &mockVLConnector{queryErr: errors.New("bad query")}
	db := newMockVLDB(conn)
	defer db.Close()

	l := NewWorkflowLoader(db, nil, nil, 10)
	_, err := l.ResolveLatestVersion(context.Background(), "wf")
	if err == nil {
		t.Fatal("expected query error")
	}
}
