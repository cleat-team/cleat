package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/google/uuid"

	_ "github.com/microsoft/go-mssqldb"
)

// TestMSSQLStoreBasic validates that MSSQLStore can be created and
// connected. This test requires a running SQL Server instance.
// Set CLEAT_TEST_MSSQL to the connection string, or it will be skipped.
func TestMSSQLStoreBasic(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLMinimalSchema(t, db)

	store := NewMSSQLStore(db, "default")
	if store == nil {
		t.Fatal("NewMSSQLStore returned nil")
	}

	// Verify basic properties.
	if store.db == nil {
		t.Fatal("store.db is nil")
	}
	if len(store.taskQueues) != 1 || store.taskQueues[0] != "default" {
		t.Fatalf("unexpected taskQueues: %v", store.taskQueues)
	}

	// Cleanup.
	testutil.CleanupMSSQLTestData(t, db)
}

// TestMSSQLStoreFactory tests the factory creation path.
func TestMSSQLStoreFactory(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL database test in short mode")
	}

	connStr := os.Getenv("CLEAT_TEST_MSSQL")

	// Open a temporary connection for schema setup.
	setupDB, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open setup DB: %v", err)
	}
	defer setupDB.Close()
	testutil.SetupMSSQLMinimalSchema(t, setupDB)
	defer testutil.CleanupMSSQLTestData(t, setupDB)

	factory := NewMSSQLStoreFactory(connStr)
	if factory == nil {
		t.Fatal("NewMSSQLStoreFactory returned nil")
	}

	if factory.DriverName() != "mssql" {
		t.Fatalf("unexpected driver name: %s", factory.DriverName())
	}

	store, closer, err := factory.OpenStore(context.Background(), "00000000-0000-0000-0000-000000000000", "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer closer.Close()

	if store == nil {
		t.Fatal("OpenStore returned nil store")
	}
}

// TestMSSQLStoreWithTenant tests tenant scoping.
func TestMSSQLStoreWithTenant(t *testing.T) {
	store := NewMSSQLStore(nil, "default")
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("unexpected default tenant: %s", store.tenantID)
	}

	scoped := store.WithTenant("11111111-1111-1111-1111-111111111111")
	if scoped.tenantID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("WithTenant did not set tenant: %s", scoped.tenantID)
	}
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Fatal("WithTenant mutated original store")
	}
}

// TestMSSQLStore_NewStoreDefaults verifies default field values of a
// newly-constructed MSSQLStore (no database required).
func TestMSSQLStore_NewStoreDefaults(t *testing.T) {
	store := NewMSSQLStore(nil)
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("default tenantID = %q, want zero UUID", store.tenantID)
	}
	if store.dialect != DialectMSSQL {
		t.Fatalf("dialect = %q, want mssql", store.dialect)
	}
	if len(store.taskQueues) != 1 || store.taskQueues[0] != "default" {
		t.Fatalf("taskQueues = %v, want [default]", store.taskQueues)
	}
	if store.idempotencyKeyTTL != 720*time.Hour {
		t.Fatalf("idempotencyKeyTTL = %v, want 720h", store.idempotencyKeyTTL)
	}
	if store.encryptSensitivePayloads {
		t.Fatal("encryptSensitivePayloads should default to false")
	}
	if store.disableReadRedaction {
		t.Fatal("disableReadRedaction should default to false")
	}
}

// TestMSSQLStore_NewStoreTaskQueues verifies custom task queue handling.
func TestMSSQLStore_NewStoreTaskQueues(t *testing.T) {
	store := NewMSSQLStore(nil, "gpu", "high-memory")
	if len(store.taskQueues) != 2 {
		t.Fatalf("taskQueues len = %d, want 2", len(store.taskQueues))
	}
	if store.taskQueues[0] != "gpu" || store.taskQueues[1] != "high-memory" {
		t.Fatalf("taskQueues = %v, want [gpu high-memory]", store.taskQueues)
	}
}

// TestMSSQLStore_WithTenant verifies WithTenant returns a copy with updated
// tenantID and does not mutate the original.
func TestMSSQLStore_WithTenant(t *testing.T) {
	store := NewMSSQLStore(nil, "default")

	scoped := store.WithTenant("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if scoped.tenantID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("scoped tenantID = %q", scoped.tenantID)
	}
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Fatal("WithTenant mutated original store")
	}
	if scoped.taskQueues[0] != store.taskQueues[0] {
		t.Fatal("WithTenant changed taskQueues")
	}
}

// TestMSSQLStore_WithIdempotencyKeyTTL verifies TTL override behaviour.
func TestMSSQLStore_WithIdempotencyKeyTTL(t *testing.T) {
	store := NewMSSQLStore(nil)
	updated := store.WithIdempotencyKeyTTL(24 * time.Hour)

	if store.idempotencyKeyTTL != 720*time.Hour {
		t.Fatal("original store was mutated")
	}
	if updated.idempotencyKeyTTL != 24*time.Hour {
		t.Fatalf("idempotencyKeyTTL = %v, want 24h", updated.idempotencyKeyTTL)
	}
}

// TestMSSQLStore_WithReadRedactionDisabled verifies the redaction toggle.
func TestMSSQLStore_WithReadRedactionDisabled(t *testing.T) {
	store := NewMSSQLStore(nil)

	disabled := store.WithReadRedactionDisabled(true)
	if !disabled.disableReadRedaction {
		t.Fatal("disableReadRedaction should be true")
	}
	if store.disableReadRedaction {
		t.Fatal("original store was mutated")
	}

	reEnabled := disabled.WithReadRedactionDisabled(false)
	if reEnabled.disableReadRedaction {
		t.Fatal("disableReadRedaction should be false after re-enabling")
	}
}

// TestMSSQLStore_WithEncryption verifies the encryption fields behave correctly.
func TestMSSQLStore_WithEncryption(t *testing.T) {
	store := NewMSSQLStore(nil)

	enc := &PayloadEncryption{key: make([]byte, 32)}
	encrypted := store.WithEncryption(enc, true)

	if encrypted.encryption != enc {
		t.Fatal("encryption reference not stored")
	}
	if !encrypted.encryptSensitivePayloads {
		t.Fatal("encryptSensitivePayloads should be true")
	}
	if store.encryptSensitivePayloads {
		t.Fatal("original store was mutated")
	}
	if store.encryption != nil {
		t.Fatal("original store encryption should be nil")
	}

	// Verify disabling works.
	disabled := encrypted.WithEncryption(enc, false)
	if disabled.encryptSensitivePayloads {
		t.Fatal("encryptSensitivePayloads should be false after disabling")
	}
}

// TestMSSQLStore_BuildTaskQueueParam verifies the queue parameter builder.
func TestMSSQLStore_BuildTaskQueueParam(t *testing.T) {
	tests := []struct {
		name       string
		taskQueues []string
		want       string
	}{
		{"empty falls back to default", nil, "default"},
		{"default only", []string{"default"}, "default"},
		{"single queue", []string{"gpu"}, "gpu"},
		{"multiple queues", []string{"default", "gpu", "high-memory"}, "default,gpu,high-memory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &MSSQLStore{taskQueues: tt.taskQueues}
			got := store.buildTaskQueueParam()
			if got != tt.want {
				t.Errorf("buildTaskQueueParam() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMSSQLStore_StartNewRun_TenantID verifies that StartNewRun stores the
// tenant_id correctly for both the non-idempotent and idempotent-key code paths.
func TestMSSQLStore_StartNewRun_TenantID(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	defer testutil.CleanupMSSQLTestData(t, db)

	// Insert a workflow_defs row (required by FK constraint).
	_, err := db.Exec(`
		INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, task_queue, tenant_id)
		VALUES (@p1, @p2, @p3, @p4, @p5, @p6)`,
		"test-wf", 1, []byte("wasm"), "[]", "default", nil)
	if err != nil {
		t.Fatalf("insert workflow_def: %v", err)
	}

	nonDefaultTenant := "11111111-1111-1111-1111-111111111111"
	store := NewMSSQLStore(db, "default")

	// --- Non-idempotent path ---
	runID := uuid.New().String()
	id, isDup, err := store.StartNewRun(context.Background(), runID,
		"test-wf", 1, json.RawMessage(`{"key":"val"}`), "", nonDefaultTenant, 0)
	if err != nil {
		t.Fatalf("StartNewRun (no idemkey): %v", err)
	}
	if isDup {
		t.Fatal("StartNewRun (no idemkey): unexpected duplicate")
	}
	if id != runID {
		t.Fatalf("StartNewRun (no idemkey): got runID %s, want %s", id, runID)
	}

	// Verify tenant_id was stored correctly.
	var storedTenant sql.NullString
	err = db.QueryRow(`SELECT CAST(tenant_id AS NVARCHAR(36)) FROM workflow_instances WHERE id = @p1`, runID).Scan(&storedTenant)
	if err != nil {
		t.Fatalf("query tenant_id (no idemkey): %v", err)
	}
	if !storedTenant.Valid {
		t.Fatal("StartNewRun (no idemkey): tenant_id is NULL")
	}
	if storedTenant.String != nonDefaultTenant {
		t.Fatalf("StartNewRun (no idemkey): tenant_id = %q, want %q", storedTenant.String, nonDefaultTenant)
	}

	// --- Idempotent-key path ---
	idemRunID := uuid.New().String()
	idemID, isDup, err := store.StartNewRun(context.Background(), idemRunID,
		"test-wf", 1, json.RawMessage(`{"key":"val"}`), "idem-key-214", nonDefaultTenant, 0)
	if err != nil {
		t.Fatalf("StartNewRun (idemkey): %v", err)
	}
	if isDup {
		t.Fatal("StartNewRun (idemkey): unexpected duplicate")
	}
	if idemID != idemRunID {
		t.Fatalf("StartNewRun (idemkey): got runID %s, want %s", idemID, idemRunID)
	}

	// Verify tenant_id was stored correctly.
	err = db.QueryRow(`SELECT CAST(tenant_id AS NVARCHAR(36)) FROM workflow_instances WHERE id = @p1`, idemRunID).Scan(&storedTenant)
	if err != nil {
		t.Fatalf("query tenant_id (idemkey): %v", err)
	}
	if !storedTenant.Valid {
		t.Fatal("StartNewRun (idemkey): tenant_id is NULL")
	}
	if storedTenant.String != nonDefaultTenant {
		t.Fatalf("StartNewRun (idemkey): tenant_id = %q, want %q", storedTenant.String, nonDefaultTenant)
	}
}

// TestMSSQLFactoryAttributes verifies factory DriverName and Dialect methods.
func TestMSSQLFactoryAttributes(t *testing.T) {
	factory := NewMSSQLStoreFactory("sqlserver://localhost")
	if factory == nil {
		t.Fatal("NewMSSQLStoreFactory returned nil")
	}

	if factory.DriverName() != "mssql" {
		t.Errorf("DriverName = %q, want %q", factory.DriverName(), "mssql")
	}

	if factory.Dialect() != DialectMSSQL {
		t.Errorf("Dialect() = %v, want %v", factory.Dialect(), DialectMSSQL)
	}
}

// TestMSSQLFactoryClose tests that Close cleans up tenant pools.
func TestMSSQLFactoryClose(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL database test in short mode")
	}

	connStr := os.Getenv("CLEAT_TEST_MSSQL")
	if connStr == "" {
		connStr = "sqlserver://sa:CleatTest123!@localhost:1433?database=cleat"
	}

	factory := NewMSSQLStoreFactory(connStr)
	if factory == nil {
		t.Fatal("NewMSSQLStoreFactory returned nil")
	}

	// Closing an empty factory should not error.
	if err := factory.Close(); err != nil {
		t.Errorf("Close on empty factory: %v", err)
	}
}

// TestMSSQLStoreConfigOptions verifies the With* config methods return
// correctly configured copies without mutating the original.
func TestMSSQLStoreConfigOptions(t *testing.T) {
	store := NewMSSQLStore(nil, "default")
	if store == nil {
		t.Fatal("NewMSSQLStore returned nil")
	}

	// WithReadRedactionDisabled
	s2 := store.WithReadRedactionDisabled(true)
	if !s2.disableReadRedaction {
		t.Error("disableReadRedaction should be true")
	}
	if store.disableReadRedaction {
		t.Error("original store should not be mutated by WithReadRedactionDisabled")
	}

	// WithEncryption
	s3 := store.WithEncryption(nil, false)
	if s3 == nil {
		t.Error("WithEncryption returned nil")
	}
}

// ---------------------------------------------------------------------------
// Factory edge cases and validation tests (no DB required)
// ---------------------------------------------------------------------------

func TestNewMSSQLStoreFactory_ZeroTTL(t *testing.T) {
	f := NewMSSQLStoreFactory("sqlserver://localhost", 0)
	if f.idempotencyKeyTTL != 0 {
		t.Errorf("idempotencyKeyTTL = %v, want 0", f.idempotencyKeyTTL)
	}
}

func TestNewMSSQLStoreFactory_EmptyConnStr(t *testing.T) {
	f := NewMSSQLStoreFactory("")
	if f == nil {
		t.Fatal("NewMSSQLStoreFactory returned nil for empty connStr")
	}
	if f.connStr != "" {
		t.Errorf("connStr = %q, want empty", f.connStr)
	}
	if f.tenantDBs == nil {
		t.Error("tenantDBs map should be initialized")
	}
}

func TestGetOrCreateTenantPool_InvalidUUID(t *testing.T) {
	f := &MSSQLStoreFactory{
		connStr:   "sqlserver://localhost",
		tenantDBs: make(map[string]*sql.DB),
	}
	_, err := f.getOrCreateTenantPool(context.Background(), "not-a-valid-uuid")
	if err == nil {
		t.Fatal("expected error for invalid UUID, got nil")
	}
	if !strings.Contains(err.Error(), "invalid tenant ID") {
		t.Errorf("error should mention invalid tenant ID, got: %v", err)
	}
}

func TestOpenStore_InvalidUUID(t *testing.T) {
	f := &MSSQLStoreFactory{
		connStr:   "sqlserver://localhost",
		tenantDBs: make(map[string]*sql.DB),
	}
	_, _, err := f.OpenStore(context.Background(), "not-a-valid-uuid")
	if err == nil {
		t.Fatal("expected error for invalid UUID, got nil")
	}
	if !strings.Contains(err.Error(), "open store for tenant") {
		t.Errorf("error should wrap open store message, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// mock types for tenantSessionConnector tests
// ---------------------------------------------------------------------------

type mockTSConnector struct {
	conn driver.Conn
	err  error
}

func (m *mockTSConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return m.conn, m.err
}

func (m *mockTSConnector) Driver() driver.Driver { return nil }

type mockTSConn struct {
	prepareErr error
	execErr    error
	closed     bool
}

func (m *mockTSConn) Prepare(query string) (driver.Stmt, error) {
	if m.prepareErr != nil {
		return nil, m.prepareErr
	}
	return &mockTSStmt{execErr: m.execErr}, nil
}

func (m *mockTSConn) Close() error {
	m.closed = true
	return nil
}

func (m *mockTSConn) Begin() (driver.Tx, error) { return nil, nil }

type mockTSConnPrepCtx struct {
	*mockTSConn
}

func (m *mockTSConnPrepCtx) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if m.prepareErr != nil {
		return nil, m.prepareErr
	}
	return &mockTSStmt{execErr: m.execErr}, nil
}

type mockTSStmt struct {
	execErr error
	closed  bool
}

func (m *mockTSStmt) Close() error {
	m.closed = true
	return nil
}

func (m *mockTSStmt) NumInput() int { return -1 }

func (m *mockTSStmt) Exec(args []driver.Value) (driver.Result, error) {
	if m.execErr != nil {
		return nil, m.execErr
	}
	return &mockTSResult{}, nil
}

func (m *mockTSStmt) Query(args []driver.Value) (driver.Rows, error) { return nil, nil }

type mockTSResult struct{}

func (m *mockTSResult) LastInsertId() (int64, error) { return 0, nil }
func (m *mockTSResult) RowsAffected() (int64, error) { return 0, nil }

// ---------------------------------------------------------------------------
// tenantSessionConnector tests
// ---------------------------------------------------------------------------

func TestTenantSessionConnector_BypassForEmptyTenant(t *testing.T) {
	mockConn := &mockTSConn{}
	mockCtr := &mockTSConnector{conn: mockConn}

	c := &tenantSessionConnector{Connector: mockCtr, tenantID: ""}
	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn != mockConn {
		t.Fatal("Connect returned different connection for empty tenant")
	}
}

func TestTenantSessionConnector_InvalidUUID(t *testing.T) {
	mockCtr := &mockTSConnector{conn: &mockTSConn{}}

	c := &tenantSessionConnector{Connector: mockCtr, tenantID: "not-a-uuid"}
	conn, err := c.Connect(context.Background())
	if err == nil {
		conn.Close()
		t.Fatal("expected error for invalid UUID, got nil")
	}
}

func TestTenantSessionConnector_ConnectorError(t *testing.T) {
	mockCtr := &mockTSConnector{err: errors.New("connection refused")}

	c := &tenantSessionConnector{Connector: mockCtr, tenantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
	_, err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error from underlying Connector, got nil")
	}
}

func TestTenantSessionConnector_PrepareFails(t *testing.T) {
	mockConn := &mockTSConn{prepareErr: errors.New("prepare failed")}
	mockCtr := &mockTSConnector{conn: mockConn}

	c := &tenantSessionConnector{Connector: mockCtr, tenantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
	_, err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected prepare error, got nil")
	}
	if !mockConn.closed {
		t.Error("connection should be closed on prepare error")
	}
}

func TestTenantSessionConnector_ExecFails(t *testing.T) {
	mockConn := &mockTSConn{execErr: errors.New("exec failed")}
	mockCtr := &mockTSConnector{conn: mockConn}

	c := &tenantSessionConnector{Connector: mockCtr, tenantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
	_, err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected exec error, got nil")
	}
	if !mockConn.closed {
		t.Error("connection should be closed on exec error")
	}
}

func TestTenantSessionConnector_Success(t *testing.T) {
	inner := &mockTSConn{}
	mockConn := &mockTSConnPrepCtx{inner}
	mockCtr := &mockTSConnector{conn: mockConn}

	c := &tenantSessionConnector{Connector: mockCtr, tenantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn != mockConn {
		t.Fatal("Connect returned different connection")
	}
	if inner.closed {
		t.Error("connection should NOT be closed on success")
	}
}

func TestTenantSessionConnector_SuccessWithPrepare(t *testing.T) {
	mockConn := &mockTSConn{}
	mockCtr := &mockTSConnector{conn: mockConn}

	c := &tenantSessionConnector{Connector: mockCtr, tenantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn != mockConn {
		t.Fatal("Connect returned different connection")
	}
}

func TestMSSQLNopCloser(t *testing.T) {
	if err := (mssqlNopCloser{}).Close(); err != nil {
		t.Errorf("mssqlNopCloser.Close() = %v, want nil", err)
	}
}

func TestMSSQLFactoryNew_DefaultTTL(t *testing.T) {
	f1 := NewMSSQLStoreFactory("sqlserver://localhost")
	if f1.idempotencyKeyTTL != 720*time.Hour {
		t.Errorf("default idempotencyKeyTTL = %v, want 720h", f1.idempotencyKeyTTL)
	}

	f2 := NewMSSQLStoreFactory("sqlserver://localhost", 1*time.Hour)
	if f2.idempotencyKeyTTL != 1*time.Hour {
		t.Errorf("custom idempotencyKeyTTL = %v, want 1h", f2.idempotencyKeyTTL)
	}
}

// ---------------------------------------------------------------------------
// MSSQL DB-backed tests
// ---------------------------------------------------------------------------

func TestMSSQLStore_SetSessionContext_EmptyTenant(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL database test in short mode")
	}

	db := testutil.MSSQLTestDB(t)
	defer db.Close()
	testutil.SetupMSSQLMinimalSchema(t, db)

	store := NewMSSQLStore(db)
	store.tenantID = ""

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	if err := store.setSessionContext(tx); err == nil {
		t.Fatal("expected error for empty tenant, got nil")
	}
}

func TestMSSQLStore_BeginTxWithContext_Failure(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL database test in short mode")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLMinimalSchema(t, db)
	db.Close()

	store := NewMSSQLStore(db)
	_, err := store.beginTxWithContext(context.Background())
	if err == nil {
		t.Fatal("expected error from closed DB, got nil")
	}
}

// ---------------------------------------------------------------------------
// MSSQL mock-based tests (no DB required)
// ---------------------------------------------------------------------------

func TestMSSQLStore_ListSchedules(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_schedules", data: [][]driver.Value{
			{"schedule-1", "wf-a", "entry1", "*/5 * * * *", `{"k":"v"}`, true, now, now},
			{"schedule-2", "wf-b", "entry2", "0 * * * *", `[]`, false, now, nil},
		}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	schedules, err := store.ListSchedules(context.Background())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(schedules) != 2 {
		t.Fatalf("expected 2 schedules, got %d", len(schedules))
	}

	s1 := schedules[0]
	if s1.Name != "schedule-1" || s1.DefName != "wf-a" || s1.EntryPoint != "entry1" {
		t.Errorf("schedule 1 fields: %+v", s1)
	}
	if s1.CronExpression != "*/5 * * * *" {
		t.Errorf("schedule 1 cron: %q", s1.CronExpression)
	}
	if string(s1.Input) != `{"k":"v"}` {
		t.Errorf("schedule 1 input: %q", string(s1.Input))
	}
	if !s1.Enabled {
		t.Error("schedule 1 should be enabled")
	}
	if !s1.NextRunAt.Equal(now) {
		t.Errorf("schedule 1 next_run: %v, want %v", s1.NextRunAt, now)
	}
	if s1.LastRunAt == nil || !s1.LastRunAt.Equal(now) {
		t.Errorf("schedule 1 last_run: %v, want %v", s1.LastRunAt, now)
	}

	s2 := schedules[1]
	if s2.Name != "schedule-2" {
		t.Errorf("schedule 2 name: %q", s2.Name)
	}
	if s2.Enabled {
		t.Error("schedule 2 should be disabled")
	}
	if s2.LastRunAt != nil {
		t.Error("schedule 2 LastRunAt should be nil")
	}
}

func TestMSSQLStore_ListSchedules_Empty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_schedules", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	schedules, err := store.ListSchedules(context.Background())
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(schedules) != 0 {
		t.Errorf("expected empty slice, got %d schedules", len(schedules))
	}
}

func TestMSSQLStore_ListSchedules_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_schedules", err: errors.New("connection lost")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	schedules, err := store.ListSchedules(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if schedules != nil {
		t.Errorf("expected nil schedules on error, got %v", schedules)
	}
}

func TestMSSQLStore_GetWorkflowDef_Success(t *testing.T) {
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	wasmBytes := []byte("wasm-data")
	pluginDepsJSON := []byte(`{"plugin1":"v1.0"}`)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_defs", data: [][]driver.Value{
			{"test-wf", int64(3), wasmBytes, "abi-v2", int64(1), pluginDepsJSON, createdAt, false},
		}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	def, err := store.GetWorkflowDef(context.Background(), "test-wf", 3)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil {
		t.Fatal("expected non-nil WorkflowDef")
	}
	if def.Name != "test-wf" || def.Version != 3 {
		t.Errorf("name/version: %q / %d", def.Name, def.Version)
	}
	if string(def.WASMBytes) != string(wasmBytes) {
		t.Errorf("wasm bytes: %q, want %q", def.WASMBytes, wasmBytes)
	}
	if def.ABIVersion != 2 {
		t.Errorf("abi: %d", def.ABIVersion)
	}
	if def.MinVersion != 1 {
		t.Errorf("min_version: %d", def.MinVersion)
	}
	if !def.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at: %v, want %v", def.CreatedAt, createdAt)
	}
	if def.Deprecated {
		t.Error("should not be deprecated")
	}
	if len(def.PluginDeps) != 1 || def.PluginDeps["plugin1"] != "v1.0" {
		t.Errorf("plugin deps: %v", def.PluginDeps)
	}
}

func TestMSSQLStore_GetWorkflowDef_NilPluginDeps(t *testing.T) {
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_defs", data: [][]driver.Value{
			{"test-wf", int64(1), []byte("wasm"), "abi-v1", int64(0), nil, createdAt, false},
		}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	def, err := store.GetWorkflowDef(context.Background(), "test-wf", 1)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil {
		t.Fatal("expected non-nil WorkflowDef")
	}
	if def.PluginDeps == nil {
		t.Fatal("PluginDeps should be initialized empty map, got nil")
	}
	if len(def.PluginDeps) != 0 {
		t.Errorf("expected empty PluginDeps, got %v", def.PluginDeps)
	}
}

func TestMSSQLStore_GetWorkflowDef_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_defs", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	def, err := store.GetWorkflowDef(context.Background(), "no-such", 99)
	if err != nil {
		t.Fatalf("expected nil error for not found, got: %v", err)
	}
	if def != nil {
		t.Errorf("expected nil WorkflowDef, got %+v", def)
	}
}

func TestMSSQLStore_GetWorkflowDef_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_defs", err: errors.New("scan failed")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	def, err := store.GetWorkflowDef(context.Background(), "wf", 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if def != nil {
		t.Errorf("expected nil WorkflowDef on error, got %+v", def)
	}
}

func TestMSSQLStore_GetActiveInstanceCountsByVersion(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances", data: [][]driver.Value{
			{"wf-a", int64(1), int64(5)},
			{"wf-b", int64(2), int64(3)},
		}},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	counts, err := store.GetActiveInstanceCountsByVersion(context.Background())
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	if len(counts) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(counts), counts)
	}
	if counts["wf-a:1"] != 5 {
		t.Errorf("wf-a:1 = %d, want 5", counts["wf-a:1"])
	}
	if counts["wf-b:2"] != 3 {
		t.Errorf("wf-b:2 = %d, want 3", counts["wf-b:2"])
	}
}

func TestMSSQLStore_GetActiveInstanceCountsByVersion_Empty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances", data: [][]driver.Value{}},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	counts, err := store.GetActiveInstanceCountsByVersion(context.Background())
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("expected empty map, got %v", counts)
	}
}

func TestMSSQLStore_GetActiveInstanceCountsByVersion_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances", err: errors.New("query failed")},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.GetActiveInstanceCountsByVersion(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestMSSQLStore_GetActiveInstanceCountsByVersion_BeginTxError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("connection refused"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.GetActiveInstanceCountsByVersion(context.Background())
	if err == nil {
		t.Fatal("expected beginTx error, got nil")
	}
}

func TestMSSQLStore_ResolveTenantFromAPIKey(t *testing.T) {
	expectedUUID := uuid.New()
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM tenant_api_keys", data: [][]driver.Value{{expectedUUID.String()}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	got, err := store.ResolveTenantFromAPIKey(context.Background(), []byte("test-key-hash"))
	if err != nil {
		t.Fatalf("ResolveTenantFromAPIKey: %v", err)
	}
	if got != expectedUUID {
		t.Errorf("UUID = %v, want %v", got, expectedUUID)
	}
}

func TestMSSQLStore_ResolveTenantFromAPIKey_Unknown(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM tenant_api_keys", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	got, err := store.ResolveTenantFromAPIKey(context.Background(), []byte("nonexistent"))
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
	if got != uuid.Nil {
		t.Errorf("expected uuid.Nil, got %v", got)
	}
}

func TestMSSQLStore_ResolveTenantFromAPIKey_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM tenant_api_keys", err: errors.New("connection lost")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	got, err := store.ResolveTenantFromAPIKey(context.Background(), []byte("any-key"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != uuid.Nil {
		t.Errorf("expected uuid.Nil on error, got %v", got)
	}
}

func TestMSSQLStore_AppendEventsInTx_Empty(t *testing.T) {
	store := NewMSSQLStore(nil)
	err := store.appendEventsInTx(context.Background(), nil, "wf-1", []EventRecord{})
	if err != nil {
		t.Fatalf("expected nil for empty records, got: %v", err)
	}
}
