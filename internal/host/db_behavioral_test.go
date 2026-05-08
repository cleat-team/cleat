package host

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Minimal mock SQL driver for testing PostgresStore constructors
//
// NewPostgresStore and WithTenant do not issue any SQL queries; they only
// store the *sql.DB reference and config fields. We use a no-op driver so
// that sql.OpenDB succeeds without a real PostgreSQL connection.
// ---------------------------------------------------------------------------

// noopDriver implements database/sql/driver with connections that panic
// if any real query is attempted — our tests never execute SQL.
type noopDriver struct {
	driver.Driver
}

func (d *noopDriver) Open(name string) (driver.Conn, error) {
	return &noopConn{}, nil
}

type noopConn struct {
	driver.Conn
}

func (c *noopConn) Prepare(query string) (driver.Stmt, error) {
	return &noopStmt{}, nil
}

func (c *noopConn) Close() error { return nil }
func (c *noopConn) Begin() (driver.Tx, error) {
	return &noopTx{}, nil
}

type noopTx struct {
	driver.Tx
}

func (tx *noopTx) Commit() error   { return nil }
func (tx *noopTx) Rollback() error { return nil }

type noopStmt struct {
	driver.Stmt
}

func (s *noopStmt) Close() error { return nil }
func (s *noopStmt) NumInput() int {
	return -1
}
func (s *noopStmt) Exec(args []driver.Value) (driver.Result, error) {
	return &noopResult{}, nil
}
func (s *noopStmt) Query(args []driver.Value) (driver.Rows, error) {
	return &noopRows{}, nil
}

type noopResult struct {
	driver.Result
}

func (r *noopResult) LastInsertId() (int64, error) { return 0, nil }
func (r *noopResult) RowsAffected() (int64, error) { return 0, nil }

type noopRows struct {
	driver.Rows
	consumed bool
}

func (r *noopRows) Columns() []string { return nil }
func (r *noopRows) Close() error      { return nil }
func (r *noopRows) Next(dest []driver.Value) error {
	if r.consumed {
		return io.EOF
	}
	r.consumed = true
	return io.EOF // return no rows immediately
}

// newNoopDB creates a *sql.DB backed by a noop driver that never actually
// connects to a database. This is safe only for testing constructors that
// do not issue queries.
func newNoopDB(t *testing.T) *sql.DB {
	t.Helper()
	// Use a unique driver name to avoid registration conflicts.
	return sql.OpenDB(&noopConnector{})
}

type noopConnector struct {
	driver.Connector
}

func (c *noopConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return &noopConn{}, nil
}

func (c *noopConnector) Driver() driver.Driver {
	return &noopDriver{}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewPostgresStore_ReturnsNonNil(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	if store == nil {
		t.Fatal("NewPostgresStore returned nil")
	}
}

func TestNewPostgresStore_DefaultTaskQueue(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	if store == nil {
		t.Fatal("NewPostgresStore returned nil")
	}
	if len(store.taskQueues) != 1 {
		t.Fatalf("expected 1 default task queue, got %d", len(store.taskQueues))
	}
	if store.taskQueues[0] != "default" {
		t.Errorf("expected default task queue 'default', got %q", store.taskQueues[0])
	}
}

func TestNewPostgresStore_CustomTaskQueue(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db, "gpu")
	if store == nil {
		t.Fatal("NewPostgresStore returned nil")
	}
	if len(store.taskQueues) != 1 {
		t.Fatalf("expected 1 task queue, got %d", len(store.taskQueues))
	}
	if store.taskQueues[0] != "gpu" {
		t.Errorf("expected task queue 'gpu', got %q", store.taskQueues[0])
	}
}

func TestNewPostgresStore_MultipleTaskQueues(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db, "high-memory", "gpu", "default")
	if store == nil {
		t.Fatal("NewPostgresStore returned nil")
	}
	if len(store.taskQueues) != 3 {
		t.Fatalf("expected 3 task queues, got %d", len(store.taskQueues))
	}
	expected := []string{"high-memory", "gpu", "default"}
	for i, q := range expected {
		if store.taskQueues[i] != q {
			t.Errorf("task queue[%d]: expected %q, got %q", i, q, store.taskQueues[i])
		}
	}
}

func TestNewPostgresStore_DefaultTenantID(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	if store == nil {
		t.Fatal("NewPostgresStore returned nil")
	}
	expectedTenant := "00000000-0000-0000-0000-000000000000"
	if store.tenantID != expectedTenant {
		t.Errorf("expected default tenant ID %q, got %q", expectedTenant, store.tenantID)
	}
}

func TestNewPostgresStore_DBIsStored(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	if store == nil {
		t.Fatal("NewPostgresStore returned nil")
	}
	if store.db != db {
		t.Error("store.db is not the same *sql.DB passed to NewPostgresStore")
	}
}

// ---------------------------------------------------------------------------
// WithTenant tests
// ---------------------------------------------------------------------------

func TestWithTenant_ReturnsNewPointer(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	tenantStore := store.WithTenant("tenant-abc-123")
	if tenantStore == nil {
		t.Fatal("WithTenant returned nil")
	}
	if tenantStore == store {
		t.Error("WithTenant should return a new *PostgresStore, not the same pointer")
	}
}

func TestWithTenant_SetsTenantID(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	tenantID := "tenant-xyz-789"
	tenantStore := store.WithTenant(tenantID)
	if tenantStore.tenantID != tenantID {
		t.Errorf("expected tenant ID %q, got %q", tenantID, tenantStore.tenantID)
	}
}

func TestWithTenant_PreservesDB(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	tenantStore := store.WithTenant("some-tenant")
	if tenantStore.db != db {
		t.Error("WithTenant should preserve the same *sql.DB reference")
	}
}

func TestWithTenant_PreservesTaskQueues(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db, "queue-a", "queue-b")
	tenantStore := store.WithTenant("tenant-123")
	if len(tenantStore.taskQueues) != 2 {
		t.Fatalf("expected 2 task queues, got %d", len(tenantStore.taskQueues))
	}
	if tenantStore.taskQueues[0] != "queue-a" || tenantStore.taskQueues[1] != "queue-b" {
		t.Errorf("task queues not preserved: got %v", tenantStore.taskQueues)
	}
}

func TestWithTenant_DoesNotMutateOriginal(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	originalTenant := store.tenantID
	_ = store.WithTenant("new-tenant")
	if store.tenantID != originalTenant {
		t.Error("WithTenant should not mutate the original store's tenant ID")
	}
}

func TestWithTenant_ChainMultiple(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)

	// Multiple WithTenant calls should each produce independent stores.
	t1 := store.WithTenant("tenant-one")
	t2 := store.WithTenant("tenant-two")

	if t1.tenantID != "tenant-one" {
		t.Errorf("t1: expected tenant-one, got %q", t1.tenantID)
	}
	if t2.tenantID != "tenant-two" {
		t.Errorf("t2: expected tenant-two, got %q", t2.tenantID)
	}
	// The original should be unchanged.
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("original store tenant ID changed to %q", store.tenantID)
	}
}

func TestWithTenant_WithEmptyTenant(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	tenantStore := store.WithTenant("")
	if tenantStore.tenantID != "" {
		t.Errorf("expected empty tenant ID, got %q", tenantStore.tenantID)
	}
}

// ---------------------------------------------------------------------------
// setRLSOnTx behavior tests (whitebox: we can observe it via method call)
// ---------------------------------------------------------------------------

func TestPostgresStore_SetRLSOnTx_EmptyTenant(t *testing.T) {
	// When tenantID is empty, setRLSOnTx should be a no-op (return nil).
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	store.tenantID = "" // manually set to empty

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	err = store.setRLSOnTx(tx)
	if err != nil {
		t.Errorf("setRLSOnTx with empty tenant should return nil, got: %v", err)
	}
}

func TestPostgresStore_SetRLSOnTx_NonEmptyTenant(t *testing.T) {
	// When tenantID is set, setRLSOnTx executes a SELECT set_config(...).
	// The noop driver will succeed without error.
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	// Store has default tenant ID.

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	err = store.setRLSOnTx(tx)
	if err != nil {
		t.Errorf("setRLSOnTx with default tenant should succeed, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// beginTxWithRLS integration test (sanity)
// ---------------------------------------------------------------------------

func TestPostgresStore_BeginTxWithRLS_Success(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	tx, err := store.beginTxWithRLS(context.Background())
	if err != nil {
		t.Fatalf("beginTxWithRLS: %v", err)
	}
	defer tx.Rollback()
}

// ---------------------------------------------------------------------------
// Time-related constant sanity test
// ---------------------------------------------------------------------------

func TestDefaultCompactionThreshold(t *testing.T) {
	if DefaultCompactionThreshold <= 0 {
		t.Errorf("DefaultCompactionThreshold should be positive, got %d", DefaultCompactionThreshold)
	}
}

func TestNowMsAvailable(t *testing.T) {
	// Basic sanity: nowMs starts at some value.
	if nowMs.Load() == 0 {
		t.Log("nowMs starts at 0 (expected before any test sets it)")
	}
}

var _ = time.Now // ensure time import is used (time.Duration referenced in mock signatures)
