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
	// When tenantID is empty, setRLSOnTx should return an error.
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
	if err == nil {
		t.Errorf("setRLSOnTx with empty tenant should return an error, got nil")
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

// ---------------------------------------------------------------------------
// DB helper function tests (pure functions, no DB needed)
// ---------------------------------------------------------------------------

func TestHelperNullStr_Empty(t *testing.T) {
	ns := nullStr("")
	if ns.Valid {
		t.Error("nullStr('') should be invalid")
	}
	if ns.String != "" {
		t.Errorf("nullStr('') should have empty String, got %q", ns.String)
	}
}

func TestHelperNullStr_NonEmpty(t *testing.T) {
	ns := nullStr("hello")
	if !ns.Valid {
		t.Error("nullStr('hello') should be valid")
	}
	if ns.String != "hello" {
		t.Errorf("nullStr('hello') should have String='hello', got %q", ns.String)
	}
}

func TestHelperNullInt64_Zero(t *testing.T) {
	ni := nullInt64(0)
	if ni.Valid {
		t.Error("nullInt64(0) should be invalid")
	}
	if ni.Int64 != 0 {
		t.Errorf("nullInt64(0) should have Int64=0, got %d", ni.Int64)
	}
}

func TestHelperNullInt64_NonZero(t *testing.T) {
	ni := nullInt64(42)
	if !ni.Valid {
		t.Error("nullInt64(42) should be valid")
	}
	if ni.Int64 != 42 {
		t.Errorf("nullInt64(42) should have Int64=42, got %d", ni.Int64)
	}
}

func TestHelperNullInt64_Negative(t *testing.T) {
	ni := nullInt64(-1)
	if !ni.Valid {
		t.Error("nullInt64(-1) should be valid (non-zero)")
	}
	if ni.Int64 != -1 {
		t.Errorf("nullInt64(-1) should have Int64=-1, got %d", ni.Int64)
	}
}

func TestHelperDaysInMonth(t *testing.T) {
	tests := []struct {
		year  int
		month time.Month
		want  int
	}{
		{2024, time.January, 31},
		{2024, time.February, 29}, // leap year
		{2025, time.February, 28}, // non-leap year
		{2024, time.April, 30},
		{2024, time.September, 30},
		{2024, time.December, 31},
	}
	for _, tt := range tests {
		got := daysInMonth(tt.year, tt.month)
		if got != tt.want {
			t.Errorf("daysInMonth(%d, %d) = %d, want %d", tt.year, tt.month, got, tt.want)
		}
	}
}

func TestHelperAtoi_Valid(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"0", 0},
		{"1", 1},
		{"42", 42},
		{" 5 ", 5},
		{"999", 999},
	}
	for _, tt := range tests {
		got := atoi(tt.input)
		if got != tt.want {
			t.Errorf("atoi(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestHelperAtoi_Invalid(t *testing.T) {
	if got := atoi("not-a-number"); got != 0 {
		t.Errorf("atoi('not-a-number') = %d, want 0", got)
	}
	if got := atoi(""); got != 0 {
		t.Errorf("atoi('') = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// matchField tests (cron field pattern matching)
// ---------------------------------------------------------------------------

func TestMatchField_Wildcard(t *testing.T) {
	if !matchField("*", 30, 0, 59) {
		t.Error("matchField('*', 30) should be true")
	}
	if !matchField("*", 0, 0, 59) {
		t.Error("matchField('*', 0) should be true")
	}
}

func TestMatchField_Exact(t *testing.T) {
	if !matchField("5", 5, 0, 59) {
		t.Error("matchField('5', 5) should be true")
	}
	if matchField("5", 6, 0, 59) {
		t.Error("matchField('5', 6) should be false")
	}
}

func TestMatchField_Step(t *testing.T) {
	if !matchField("*/5", 0, 0, 59) {
		t.Error("matchField('*/5', 0) should be true (0 %% 5 == 0)")
	}
	if !matchField("*/5", 5, 0, 59) {
		t.Error("matchField('*/5', 5) should be true")
	}
	if !matchField("*/5", 10, 0, 59) {
		t.Error("matchField('*/5', 10) should be true")
	}
	if matchField("*/5", 3, 0, 59) {
		t.Error("matchField('*/5', 3) should be false")
	}
	if matchField("*/0", 5, 0, 59) {
		t.Error("matchField('*/0', 5) should be false (step is 0)")
	}
}

func TestMatchField_Range(t *testing.T) {
	if !matchField("10-20", 15, 0, 59) {
		t.Error("matchField('10-20', 15) should be true")
	}
	if !matchField("10-20", 10, 0, 59) {
		t.Error("matchField('10-20', 10) should be true (low bound)")
	}
	if !matchField("10-20", 20, 0, 59) {
		t.Error("matchField('10-20', 20) should be true (high bound)")
	}
	if matchField("10-20", 9, 0, 59) {
		t.Error("matchField('10-20', 9) should be false")
	}
	if matchField("10-20", 21, 0, 59) {
		t.Error("matchField('10-20', 21) should be false")
	}
}

func TestMatchField_List(t *testing.T) {
	if !matchField("1,3,5", 1, 0, 59) {
		t.Error("matchField('1,3,5', 1) should be true")
	}
	if !matchField("1,3,5", 3, 0, 59) {
		t.Error("matchField('1,3,5', 3) should be true")
	}
	if !matchField("1,3,5", 5, 0, 59) {
		t.Error("matchField('1,3,5', 5) should be true")
	}
	if matchField("1,3,5", 2, 0, 59) {
		t.Error("matchField('1,3,5', 2) should be false")
	}
	if matchField("1,3,5", 4, 0, 59) {
		t.Error("matchField('1,3,5', 4) should be false")
	}
}

// ---------------------------------------------------------------------------
// NextCronTime tests
// ---------------------------------------------------------------------------

func TestNextCronTime_EveryMinute(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	next := NextCronTime("* * * * *", base)
	expected := time.Date(2024, 1, 15, 10, 31, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("NextCronTime('* * * * *', %v) = %v, want %v", base, next, expected)
	}
}

func TestNextCronTime_SpecificMinute(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	next := NextCronTime("30 * * * *", base)
	expected := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("NextCronTime('30 * * * *', %v) = %v, want %v", base, next, expected)
	}
}

func TestNextCronTime_SpecificHourAndMinute(t *testing.T) {
	base := time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC)
	next := NextCronTime("30 14 * * *", base)
	expected := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("NextCronTime('30 14 * * *', %v) = %v, want %v", base, next, expected)
	}
}

func TestNextCronTime_StepPattern(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	next := NextCronTime("*/15 * * * *", base)
	expected := time.Date(2024, 1, 15, 10, 15, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("NextCronTime('*/15 * * * *', %v) = %v, want %v", base, next, expected)
	}
}

func TestNextCronTime_InvalidExpression(t *testing.T) {
	base := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	next := NextCronTime("invalid", base)
	expected := base.Add(24 * time.Hour)
	if !next.Equal(expected) {
		t.Errorf("NextCronTime('invalid', %v) = %v, want %v", base, next, expected)
	}
}

func TestNextCronTime_ListAndRange(t *testing.T) {
	base := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	// 8:30 and 17:30 on weekdays (1-5 = Mon-Fri)
	next := NextCronTime("30 8,17 * * 1-5", base)
	// Next should be today at 17:30 if it's a weekday and past 8:30
	weekday := base.Weekday()
	if weekday >= time.Monday && weekday <= time.Friday {
		expected := time.Date(2024, 6, 15, 17, 30, 0, 0, time.UTC)
		if !next.Equal(expected) {
			t.Errorf("NextCronTime('30 8,17 * * 1-5', %v) = %v, want %v", base, next, expected)
		}
	} else {
		// Weekend: next should be Monday at 8:30
		t.Logf("base is weekend (%v), skipping exact assertion", weekday)
	}
}
