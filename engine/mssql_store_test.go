package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"os"
	"strings"
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
	//
	// tenant_id is the default tenant, not nil: migrations/mssql/001_schema.sql
	// declares the column NOT NULL DEFAULT '000…' and always has. This passed
	// nil only because engine/testutil's copy of the schema left the column
	// nullable, which is the drift IMPROVEMENT-PLAN 3.12's ownership work
	// corrected -- so the row this test used to insert could not exist in a
	// real database.
	_, err := db.Exec(`
		INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, task_queue, tenant_id)
		VALUES (@p1, @p2, @p3, @p4, @p5, @p6)`,
		"test-wf", 1, []byte("wasm"), "[]", "default", DefaultTenantUUID)
	if err != nil {
		t.Fatalf("insert workflow_def: %v", err)
	}

	nonDefaultTenant := "11111111-1111-1111-1111-111111111111"

	// The runs below are deliberately started for a NON-default tenant, and
	// since D7 the FK on workflow_instances carries tenant_id
	// (IMPROVEMENT-PLAN 3.77), so that tenant needs its own definition row.
	// Seeded here rather than by changing the runs to the default tenant,
	// because the cross-tenant mismatch is what this test is about.
	if _, err := db.Exec(`
		INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, task_queue, tenant_id)
		VALUES (@p1, @p2, @p3, @p4, @p5, @p6)`,
		"test-wf", 1, []byte("wasm"), "[]", "default", nonDefaultTenant); err != nil {
		t.Fatalf("insert workflow_def (non-default tenant): %v", err)
	}
	store := NewMSSQLStore(db, "default")

	// The assertions below read workflow_instances directly to check what
	// StartNewRun stored. That read is subject to the shipped security
	// policies, and the rows it is looking for belong to nonDefaultTenant --
	// so on the plain connection it finds nothing and the test fails as
	// "query tenant_id: sql: no rows in result set", which reads like
	// StartNewRun not having written anything.
	//
	// It did write. This test is deliberately cross-tenant: it passes a tenant
	// as an argument to check the argument is honoured, so verifying it is
	// administrative work by definition and needs the admin connection.
	adminDB := testutil.MSSQLAdminDB(t, db)

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
	err = adminDB.QueryRow(`SELECT CAST(tenant_id AS NVARCHAR(36)) FROM workflow_instances WHERE id = @p1`, runID).Scan(&storedTenant)
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
	err = adminDB.QueryRow(`SELECT CAST(tenant_id AS NVARCHAR(36)) FROM workflow_instances WHERE id = @p1`, idemRunID).Scan(&storedTenant)
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
	// Connect wraps the driver connection so the tenant session context can
	// be re-applied on ResetSession (IMPROVEMENT-PLAN.md 2.71). Assert the
	// wrapper carries the right connection and tenant rather than asserting
	// object identity, which the wrapper necessarily breaks.
	wrapped, ok := conn.(*tenantSessionConn)
	if !ok {
		t.Fatalf("Connect returned %T, want *tenantSessionConn", conn)
	}
	if wrapped.Conn != driver.Conn(mockConn) {
		t.Fatal("wrapper does not carry the connection the connector opened")
	}
	if wrapped.tenantID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("wrapper tenantID = %q, want the connector's tenant", wrapped.tenantID)
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
	wrapped, ok := conn.(*tenantSessionConn)
	if !ok {
		t.Fatalf("Connect returned %T, want *tenantSessionConn", conn)
	}
	if wrapped.Conn != driver.Conn(mockConn) {
		t.Fatal("wrapper does not carry the connection the connector opened")
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
			{"schedule-1", "wf-a", "entry1", "*/5 * * * *", `{"k":"v"}`, true, now, now, "UTC", "00000000-0000-0000-0000-000000000000", "catch_up", 60, "allow", "run-1"},
			{"schedule-2", "wf-b", "entry2", "0 * * * *", `[]`, false, now, nil, "America/New_York", "33333333-3333-3333-3333-333333333333", "skip", 7, "skip", ""},
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
	// Different zones per row on purpose: a Scan that dropped the column would
	// leave both empty and still return two schedules.
	if s1.Timezone != "UTC" {
		t.Errorf("schedule 1 timezone: %q, want UTC", s1.Timezone)
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
	if s2.Timezone != "America/New_York" {
		t.Errorf("schedule 2 timezone: %q, want America/New_York", s2.Timezone)
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
			{"test-wf", int64(3), wasmBytes, int64(2), int64(1), pluginDepsJSON, createdAt, false},
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
			{"test-wf", int64(1), []byte("wasm"), int64(1), int64(0), nil, createdAt, false},
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
		{match: "FROM admin.tenant_api_keys", data: [][]driver.Value{{expectedUUID.String()}}},
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
		{match: "FROM admin.tenant_api_keys", data: [][]driver.Value{}},
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
		{match: "FROM admin.tenant_api_keys", err: errors.New("connection lost")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	got, err := store.ResolveTenantFromAPIKey(context.Background(), []byte("any-key"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Name the injected error rather than accepting any error. `err != nil`
	// alone cannot tell "the query failed as the mock was told to make it fail"
	// from "the mock never matched the query, so the store found no rows" --
	// and the second is what was happening: the match string said
	// `FROM tenant_api_keys` while the query says `FROM admin.tenant_api_keys`,
	// so this test passed for three commits without the mock ever firing.
	if !strings.Contains(err.Error(), "connection lost") {
		t.Errorf("error = %v, want it to carry the injected \"connection lost\" -- "+
			"a different error means the mock did not match the query", err)
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

// ---------------------------------------------------------------------------
// Heartbeat operations
// ---------------------------------------------------------------------------

func TestMSSQLStore_Heartbeat_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET heartbeat_at", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	ok, err := store.Heartbeat(context.Background(), "wf-1", "worker-1", 1)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !ok {
		t.Error("expected true (heartbeat updated)")
	}
}

func TestMSSQLStore_Heartbeat_NoRows(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET heartbeat_at", affected: 0},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	ok, err := store.Heartbeat(context.Background(), "wf-1", "worker-1", 1)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if ok {
		t.Error("expected false when no rows updated")
	}
}

func TestMSSQLStore_Heartbeat_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("connection refused"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.Heartbeat(context.Background(), "wf-1", "worker-1", 1)
	if err == nil {
		t.Fatal("expected error from begin failure, got nil")
	}
}

func TestMSSQLStore_BatchHeartbeat_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "status = 'running'", affected: 5},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	n, err := store.BatchHeartbeat(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("BatchHeartbeat: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 rows, got %d", n)
	}
}

func TestMSSQLStore_BatchHeartbeat_Zero(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "status = 'running'", affected: 0},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	n, err := store.BatchHeartbeat(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("BatchHeartbeat: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Workflow lifecycle: CompleteWorkflow, FailWorkflow, ReleaseWorkflow
// ---------------------------------------------------------------------------

func TestMSSQLStore_CompleteWorkflow_SuccessMock(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET status = 'done'", affected: 1},
		{match: "idempotency_keys SET result"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompleteWorkflow(context.Background(), "wf-1", "worker-1", 1, `{"result":"ok"}`, nil)
	if err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}
}

func TestMSSQLStore_CompleteWorkflow_BeginErrorMock(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompleteWorkflow(context.Background(), "wf-1", "worker-1", 1, `{}`, nil)
	if err == nil {
		t.Fatal("expected error from begin failure, got nil")
	}
}

func TestMSSQLStore_FailWorkflow_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET status = 'failed'", affected: 1},
		{match: "idempotency_keys SET error_msg"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.FailWorkflow(context.Background(), "wf-1", "worker-1", 1,
		"something went wrong", "ERR_001", "my-op", nil)
	if err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}
}

func TestMSSQLStore_ReleaseWorkflow_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET status = 'ready'", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.ReleaseWorkflow(context.Background(), "wf-1", "worker-1", 1, time.Now())
	if err != nil {
		t.Fatalf("ReleaseWorkflow: %v", err)
	}
}

func TestMSSQLStore_ReleaseWorkflow_NoRows(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "SET status = 'ready'", affected: 0},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.ReleaseWorkflow(context.Background(), "wf-1", "worker-1", 1, time.Now())
	if err == nil {
		t.Fatal("expected error when no rows affected, got nil")
	}
}

// ---------------------------------------------------------------------------
// Signal operations: DeliverSignal, PollSignal, CheckCancellation
// ---------------------------------------------------------------------------

func TestMSSQLStore_DeliverSignal_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "MERGE workflow_signals"},
		{match: "SET next_wake_at"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.DeliverSignal(context.Background(), "wf-1", "order-approved", `{"approved":true}`)
	if err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}
}

func TestMSSQLStore_DeliverSignal_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.DeliverSignal(context.Background(), "wf-1", "order-approved", `{}`)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestMSSQLStore_PollSignal_Found(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_signals", data: [][]driver.Value{{`{"approved":true}`}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	payload, found, err := store.PollSignal(context.Background(), "wf-1", "order-approved")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Error("expected found=true")
	}
	if payload != `{"approved":true}` {
		t.Errorf("payload = %q, want %q", payload, `{"approved":true}`)
	}
}

func TestMSSQLStore_PollSignal_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_signals", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, found, err := store.PollSignal(context.Background(), "wf-1", "nonexistent")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if found {
		t.Error("expected found=false")
	}
}

func TestMSSQLStore_PollSignal_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_signals", err: errors.New("connection lost")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, _, err := store.PollSignal(context.Background(), "wf-1", "order-approved")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestMSSQLStore_CheckCancellation_True(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances WHERE id", data: [][]driver.Value{{true, "user requested"}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	cancelled, reason, err := store.CheckCancellation(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("CheckCancellation: %v", err)
	}
	if !cancelled {
		t.Error("expected cancelled=true")
	}
	if reason != "user requested" {
		t.Errorf("reason = %q, want %q", reason, "user requested")
	}
}

func TestMSSQLStore_CheckCancellation_False(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances WHERE id", data: [][]driver.Value{{false, nil}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	cancelled, reason, err := store.CheckCancellation(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("CheckCancellation: %v", err)
	}
	if cancelled {
		t.Error("expected cancelled=false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

// ---------------------------------------------------------------------------
// RequestCancellation
// ---------------------------------------------------------------------------

func TestMSSQLStore_RequestCancellation_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "cancellation_requested"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.RequestCancellation(context.Background(), "wf-1", "too slow")
	if err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Child workflow operations
// ---------------------------------------------------------------------------

func TestMSSQLStore_StartChildWorkflow_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_instances", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	runID, err := store.StartChildWorkflow(context.Background(), "parent-1", "child-wf", `{"key":"val"}`, 0, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	if runID == "" {
		t.Error("expected non-empty runID")
	}
}

func TestMSSQLStore_StartChildWorkflow_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_instances", err: errors.New("insert failed")},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.StartChildWorkflow(context.Background(), "parent-1", "child-wf", `{}`, 0, "", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestMSSQLStore_StartChildWorkflowAtomic_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "INSERT INTO workflow_instances", affected: 1},
		{match: "INSERT INTO event_history", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	event := EventRecord{
		Step:       0,
		EventType:  EventTypeChildWorkflow,
		ChildName:  "child-wf",
		ChildInput: `{"key":"val"}`,
	}
	runID, err := store.StartChildWorkflowAtomic(context.Background(), "", "parent-1",
		"child-wf", `{"key":"val"}`, 1, "ABANDON", event, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if runID == "" {
		t.Error("expected non-empty runID")
	}
}

func TestMSSQLStore_StartChildWorkflowAtomic_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	event := EventRecord{Step: 0, EventType: EventTypeChildWorkflow}
	_, err := store.StartChildWorkflowAtomic(context.Background(), "", "parent-1",
		"child-wf", `{}`, 1, "", event, 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Workflow listing: ListWorkflows, GetWorkflowByID
// ---------------------------------------------------------------------------

func TestMSSQLStore_ListWorkflows_Simple(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances", data: [][]driver.Value{
			{
				"wf-1", "test-wf", int64(1), "running", `{"key":"val"}`,
				"worker-1", now, nil, nil, nil, now,
				int64(3), int64(0), "",
			},
		}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	wfs, err := store.ListWorkflows(context.Background(), WorkflowFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	if wfs[0].ID != "wf-1" || wfs[0].DefName != "test-wf" || wfs[0].Status != "running" {
		t.Errorf("unexpected workflow fields: %+v", wfs[0])
	}
}

func TestMSSQLStore_ListWorkflows_Empty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	wfs, err := store.ListWorkflows(context.Background(), WorkflowFilter{})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(wfs))
	}
}

func TestMSSQLStore_ListWorkflows_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.ListWorkflows(context.Background(), WorkflowFilter{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestMSSQLStore_GetWorkflowByID_Success(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances WHERE id", data: [][]driver.Value{{
			"wf-1", "test-wf", int64(1), "running", `{"key":"val"}`,
			"worker-1", now, now, nil, nil, nil, nil, nil,
			int64(3), int64(0), "",
		}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	wf, err := store.GetWorkflowByID(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("expected non-nil workflow")
	}
	if wf.ID != "wf-1" || wf.DefName != "test-wf" || wf.Status != "running" {
		t.Errorf("unexpected workflow fields: %+v", wf)
	}
	if wf.Generation != 3 {
		t.Errorf("generation = %d, want 3", wf.Generation)
	}
}

func TestMSSQLStore_GetWorkflowByID_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances WHERE id", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	wf, err := store.GetWorkflowByID(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf != nil {
		t.Error("expected nil workflow for not found")
	}
}

// ---------------------------------------------------------------------------
// Promise operations
// ---------------------------------------------------------------------------

func TestMSSQLStore_CreatePromise_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_promises"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CreatePromise(context.Background(), "wf-1", "my-promise", "promise-uuid-1")
	if err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
}

func TestMSSQLStore_CreatePromise_Duplicate(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_promises", err: errors.New("duplicate key")},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CreatePromise(context.Background(), "wf-1", "dup-promise", "dup-uuid")
	if err == nil {
		t.Fatal("expected error for duplicate, got nil")
	}
}

func TestMSSQLStore_ResolvePromise_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET status = 'resolved'"},
		{match: "SET next_wake_at"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.ResolvePromise(context.Background(), "wf-1", "promise-uuid-1", `{"result":"ok"}`)
	if err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}
}

func TestMSSQLStore_RejectPromise_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "SET status = 'rejected'"},
		{match: "SET next_wake_at"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.RejectPromise(context.Background(), "wf-1", "promise-uuid-1", "something went wrong")
	if err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}
}

func TestMSSQLStore_GetPromise_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_promises", data: [][]driver.Value{{"resolved", `{"result":"ok"}`, ""}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	status, result, errMsg, err := store.GetPromise(context.Background(), "wf-1", "promise-uuid-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "resolved" {
		t.Errorf("status = %q, want %q", status, "resolved")
	}
	if result != `{"result":"ok"}` {
		t.Errorf("result = %q, want %q", result, `{"result":"ok"}`)
	}
	if errMsg != "" {
		t.Errorf("errMsg = %q, want empty", errMsg)
	}
}

func TestMSSQLStore_GetPromise_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_promises", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	status, result, errMsg, err := store.GetPromise(context.Background(), "wf-1", "nonexistent")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q", status, "pending")
	}
	if result != "" {
		t.Errorf("result = %q, want empty", result)
	}
	if errMsg != "" {
		t.Errorf("errMsg = %q, want empty", errMsg)
	}
}

// ---------------------------------------------------------------------------
// Concurrency key operations
// ---------------------------------------------------------------------------

func TestMSSQLStore_AcquireConcurrencyKey_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "DELETE FROM concurrency_keys WHERE key_hash", affected: 0},
		{match: "INSERT INTO concurrency_keys", affected: 1},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	acquired, err := store.AcquireConcurrencyKey(context.Background(), "my-key", "wf-1", 60*time.Second)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Error("expected acquired=true")
	}
}

func TestMSSQLStore_AcquireConcurrencyKey_NotAcquired(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "DELETE FROM concurrency_keys WHERE key_hash", affected: 0},
		{match: "INSERT INTO concurrency_keys", affected: 0},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	acquired, err := store.AcquireConcurrencyKey(context.Background(), "my-key", "wf-2", 60*time.Second)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Error("expected acquired=false")
	}
}

func TestMSSQLStore_AcquireConcurrencyKey_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.AcquireConcurrencyKey(context.Background(), "my-key", "wf-1", 60*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestMSSQLStore_ReleaseConcurrencyKey_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "DELETE FROM concurrency_keys"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.ReleaseConcurrencyKey(context.Background(), "my-key")
	if err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Segment finalization
// ---------------------------------------------------------------------------

func TestMSSQLStore_FinalizeWorkflowSegment_Done(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		// finalize_workflow_status is called via QueryRow (it emits a
		// trailing SELECT reporting whether the generation fence held).
		{match: "finalize_workflow_status", data: [][]driver.Value{{true}}},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.FinalizeWorkflowSegment(context.Background(), "wf-1", "worker-1", 1,
		[]EventRecord{}, "done", `{"result":"ok"}`, "", "", nil, time.Time{})
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment(done): %v", err)
	}
}

func TestMSSQLStore_FinalizeWorkflowSegment_Failed(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "finalize_workflow_status", data: [][]driver.Value{{true}}},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.FinalizeWorkflowSegment(context.Background(), "wf-1", "worker-1", 1,
		[]EventRecord{}, "failed", "error occurred", "ERR_01", "my-op", nil, time.Time{})
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment(failed): %v", err)
	}
}

func TestMSSQLStore_FinalizeWorkflowSegment_Ready(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "finalize_workflow_status", data: [][]driver.Value{{true}}},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.FinalizeWorkflowSegment(context.Background(), "wf-1", "worker-1", 1,
		[]EventRecord{}, "ready", "", "", "", nil, time.Now())
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment(ready): %v", err)
	}
}

func TestMSSQLStore_FinalizeWorkflowSegment_UnknownStatus(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.FinalizeWorkflowSegment(context.Background(), "wf-1", "worker-1", 1,
		[]EventRecord{}, "invalid_status", "", "", "", nil, time.Time{})
	if err == nil {
		t.Fatal("expected error for unknown status, got nil")
	}
}

// ---------------------------------------------------------------------------
// Schedule operations: Create, Delete, SetEnabled, GetDue, UpdateNextRun
// ---------------------------------------------------------------------------

func TestMSSQLStore_CreateSchedule_Success(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_schedules"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	sch := Schedule{
		Name:           "hourly-job",
		DefName:        "my-wf",
		EntryPoint:     "main",
		CronExpression: "0 * * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      now,
	}
	err := store.CreateSchedule(context.Background(), sch)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
}

func TestMSSQLStore_DeleteSchedule_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_schedules"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.DeleteSchedule(context.Background(), "hourly-job")
	if err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
}

func TestMSSQLStore_SetScheduleEnabled_Success(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "UPDATE workflow_schedules SET enabled"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.SetScheduleEnabled(context.Background(), "hourly-job", false)
	if err != nil {
		t.Fatalf("SetScheduleEnabled: %v", err)
	}
}

func TestMSSQLStore_GetDueSchedules_Success(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "READPAST", data: [][]driver.Value{
			{"due-sch", "wf-a", "entry1", "*/5 * * * *", `{"k":"v"}`, true, now, now, "Asia/Tokyo", "33333333-3333-3333-3333-333333333333", "skip", 11, "skip", "run-due"},
		}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	schedules, err := store.GetDueSchedules(context.Background())
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	if len(schedules) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(schedules))
	}
	if schedules[0].Name != "due-sch" || !schedules[0].Enabled {
		t.Errorf("unexpected schedule: %+v", schedules[0])
	}
	// The scheduler computes the next firing from this field; without it
	// every schedule silently reverts to the UTC wall clock.
	if schedules[0].Timezone != "Asia/Tokyo" {
		t.Errorf("timezone = %q, want Asia/Tokyo", schedules[0].Timezone)
	}
	if schedules[0].TenantID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("tenant = %q, want 33333333-3333-3333-3333-333333333333", schedules[0].TenantID)
	}
	// Distinct per-row policy values, so a Scan that dropped these columns
	// cannot pass by returning the right number of rows.
	if schedules[0].MisfirePolicy != "skip" || schedules[0].OverlapPolicy != "skip" {
		t.Errorf("policies = %q/%q, want skip/skip", schedules[0].MisfirePolicy, schedules[0].OverlapPolicy)
	}
	if schedules[0].CatchUpLimit != 11 {
		t.Errorf("catch_up_limit = %d, want 11", schedules[0].CatchUpLimit)
	}
	if schedules[0].LastRunID != "run-due" {
		t.Errorf("last_run_id = %q, want run-due", schedules[0].LastRunID)
	}
}

func TestMSSQLStore_GetDueSchedules_Empty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "READPAST", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	schedules, err := store.GetDueSchedules(context.Background())
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	if len(schedules) != 0 {
		t.Errorf("expected 0 schedules, got %d", len(schedules))
	}
}

func TestMSSQLStore_UpdateScheduleNextRun_Success(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "next_run_at = @p2"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.UpdateScheduleNextRun(context.Background(), "hourly-job", now.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("UpdateScheduleNextRun: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Compaction operations
// ---------------------------------------------------------------------------

func TestMSSQLStore_GetCompactionCandidates_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FETCH NEXT", data: [][]driver.Value{
			{"wf-1"},
			{"wf-2"},
		}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	ids, err := store.GetCompactionCandidates(context.Background(), 100, 10)
	if err != nil {
		t.Fatalf("GetCompactionCandidates: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(ids))
	}
	if ids[0] != "wf-1" || ids[1] != "wf-2" {
		t.Errorf("ids = %v, want [wf-1 wf-2]", ids)
	}
}

func TestMSSQLStore_GetCompactionCandidates_Empty(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FETCH NEXT", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	ids, err := store.GetCompactionCandidates(context.Background(), 100, 10)
	if err != nil {
		t.Fatalf("GetCompactionCandidates: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(ids))
	}
}

func TestMSSQLStore_LoadCompactionState_Success(t *testing.T) {
	stateJSON := `{"version":1,"compacted_step":10,"events":[]}`
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances WHERE id", data: [][]driver.Value{{stateJSON}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	cs, err := store.LoadCompactionState(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs == nil {
		t.Fatal("expected non-nil CompactionState")
	}
	if cs.Version != 1 {
		t.Errorf("version = %d, want 1", cs.Version)
	}
	if cs.CompactedStep != 10 {
		t.Errorf("compacted_step = %d, want 10", cs.CompactedStep)
	}
}

func TestMSSQLStore_LoadCompactionState_Null(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances WHERE id", data: [][]driver.Value{{nil}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	cs, err := store.LoadCompactionState(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs != nil {
		t.Error("expected nil CompactionState for NULL state")
	}
}

func TestMSSQLStore_LoadCompactionState_NotFound(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM workflow_instances WHERE id", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	cs, err := store.LoadCompactionState(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs != nil {
		t.Error("expected nil CompactionState for not found")
	}
}

func TestMSSQLStore_CompactHistory_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", data: [][]driver.Value{{int64(5)}}},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "DELETE FROM event_history"},
		{match: "SET compaction_step"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompactHistory(context.Background(), "wf-1", []byte(`{"version":1,"compacted_step":10,"events":[]}`), 10, 5)
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
}

func TestMSSQLStore_CompactHistory_WorkflowGone(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT generation", err: sql.ErrNoRows},
	}, []mockExecResult{
		{match: "sp_set_session_context"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.CompactHistory(context.Background(), "nonexistent", []byte(`{}`), 1, 1)
	if err != nil {
		t.Fatalf("CompactHistory (workflow gone): %v", err)
	}
}

// ---------------------------------------------------------------------------
// AppendEventHistoryBatch
// ---------------------------------------------------------------------------

func TestMSSQLStore_AppendEventHistoryBatch_SuccessMock(t *testing.T) {
	db := newMockDBForPostgres(t, nil, []mockExecResult{
		{match: "sp_set_session_context"},
		{match: "INSERT INTO event_history", affected: 1},
		{match: "event_count = event_count"},
	})
	defer db.Close()

	store := NewMSSQLStore(db)
	recs := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "my-svc", Op: "my-op", Request: `{"key":"val"}`, Response: `{"result":"ok"}`},
	}
	err := store.AppendEventHistoryBatch(context.Background(), "wf-1", recs)
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}
}

func TestMSSQLStore_AppendEventHistoryBatch_BeginErrorMock(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	recs := []EventRecord{
		{Step: 0, EventType: EventTypeCall},
	}
	err := store.AppendEventHistoryBatch(context.Background(), "wf-1", recs)
	if err == nil {
		t.Fatal("expected error from begin failure, got nil")
	}
}

func TestMSSQLStore_AppendEventHistoryBatch_EmptyMock(t *testing.T) {
	db := newMockDBForPostgres(t, nil, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	err := store.AppendEventHistoryBatch(context.Background(), "wf-1", []EventRecord{})
	if err != nil {
		t.Fatalf("AppendEventHistoryBatch empty: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadEventHistory
// ---------------------------------------------------------------------------

func TestMSSQLStore_LoadEventHistory_Success(t *testing.T) {
	now := time.Now()
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", data: [][]driver.Value{{
			int64(0),              // step
			string(EventTypeCall), // event_type
			"my-svc",              // service
			"my-op",               // operation
			`{"key":"val"}`,       // request
			`{"result":"ok"}`,     // response
			"",                    // error
			int64(100),            // duration_ms
			"",                    // signal_names
			int64(0),              // timeout_ms
			"",                    // signal_name
			"",                    // signal_payload
			"",                    // defer_description
			"",                    // defer_id
			"",                    // child_name
			"",                    // child_input
			"",                    // run_id
			"",                    // new_input
			"",                    // plugin_name
			"",                    // plugin_func
			"",                    // plugin_input
			"",                    // plugin_output
			"",                    // plugin_error
			"",                    // payload
			"",                    // promise_name
			"",                    // promise_id
			"",                    // promise_result
			"",                    // promise_error
			now,                   // created_at
			false,                 // pending (intent_at IS NOT NULL AND checksum IS NULL)
		}}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	history, err := store.LoadEventHistory(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(history))
	}
	if history[0].Step != 0 || string(history[0].EventType) != string(EventTypeCall) {
		t.Errorf("unexpected event: step=%d, type=%s", history[0].Step, history[0].EventType)
	}
	if history[0].Service != "my-svc" || history[0].Op != "my-op" {
		t.Errorf("service/op = %s/%s", history[0].Service, history[0].Op)
	}
}

func TestMSSQLStore_LoadEventHistory_EmptyMock(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", data: [][]driver.Value{}},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	history, err := store.LoadEventHistory(context.Background(), "wf-empty")
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected 0 events, got %d", len(history))
	}
}

func TestMSSQLStore_LoadEventHistory_QueryErrorMock(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "FROM event_history", err: errors.New("connection lost")},
	}, nil)
	defer db.Close()

	store := NewMSSQLStore(db)
	_, err := store.LoadEventHistory(context.Background(), "wf-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
