package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
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
// Pure-logic tests (no database required)
// ---------------------------------------------------------------------------

func TestMSSQLNopCloser(t *testing.T) {
	var c mssqlNopCloser
	if err := c.Close(); err != nil {
		t.Errorf("nopCloser.Close() returned error: %v", err)
	}
}

func TestMSSQLStoreConfigOptionsExtended(t *testing.T) {
	store := NewMSSQLStore(nil)

	// WithIdempotencyKeyTTL with zero duration.
	zeroTTL := store.WithIdempotencyKeyTTL(0)
	if zeroTTL.idempotencyKeyTTL != 0 {
		t.Errorf("idempotencyKeyTTL = %v, want 0", zeroTTL.idempotencyKeyTTL)
	}
	if store.idempotencyKeyTTL == 0 {
		t.Error("original store should not be mutated by WithIdempotencyKeyTTL")
	}

	// WithReadRedactionDisabled round-trip.
	disabled := store.WithReadRedactionDisabled(true)
	if !disabled.disableReadRedaction {
		t.Error("disableReadRedaction should be true")
	}
	reEnabled := disabled.WithReadRedactionDisabled(false)
	if reEnabled.disableReadRedaction {
		t.Error("disableReadRedaction should be false after toggling back")
	}

	// WithEncryption with nil key, enabled=true.
	encEnabled := store.WithEncryption(nil, true)
	if !encEnabled.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be true")
	}
	if encEnabled.encryption != nil {
		t.Error("encryption should be nil when nil key passed")
	}
}

func TestMSSQLSetSessionContext_EmptyTenant(t *testing.T) {
	store := NewMSSQLStore(nil)
	store.tenantID = ""
	err := store.setSessionContext(nil)
	if err == nil {
		t.Error("setSessionContext with empty tenant should return error")
	}
}

// ---------------------------------------------------------------------------
// DB-backed tests (require CLEAT_TEST_MSSQL)
// ---------------------------------------------------------------------------

func TestMSSQLSetSessionContext(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLMinimalSchema(t, db)
	defer testutil.CleanupMSSQLTestData(t, db)

	store := NewMSSQLStore(db, "default")
	store.tenantID = "11111111-1111-1111-1111-111111111111"

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	if err := store.setSessionContext(tx); err != nil {
		t.Errorf("setSessionContext: %v", err)
	}
}

func TestMSSQLBeginTxWithContext(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLMinimalSchema(t, db)
	defer testutil.CleanupMSSQLTestData(t, db)

	store := NewMSSQLStore(db, "default")
	store.tenantID = "11111111-1111-1111-1111-111111111111"

	tx, err := store.beginTxWithContext(context.Background())
	if err != nil {
		t.Fatalf("beginTxWithContext: %v", err)
	}
	if tx == nil {
		t.Fatal("beginTxWithContext returned nil tx")
	}
	if err := tx.Rollback(); err != nil {
		t.Errorf("rollback: %v", err)
	}
}

func TestMSSQLEnforceParentClosePolicy(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	defer testutil.CleanupMSSQLTestData(t, db)

	// Insert a workflow_defs row to satisfy FK constraints.
	_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES (@p1, @p2, @p3)`,
		"test-parent-close-policy", 1, []byte{})
	if err != nil {
		t.Fatalf("insert workflow_defs: %v", err)
	}

	parentID := uuid.New().String()
	childTerminateID := uuid.New().String()
	childCancelID := uuid.New().String()

	// Insert parent workflow.
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status) VALUES (@p1, @p2, @p3, @p4)`,
		parentID, "test-parent-close-policy", 1, "running")
	if err != nil {
		t.Fatalf("insert parent workflow: %v", err)
	}

	// Insert TERMINATE child.
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, parent_workflow_id, parent_close_policy) VALUES (@p1, @p2, @p3, @p4, @p5, @p6)`,
		childTerminateID, "test-parent-close-policy", 1, "running", parentID, "TERMINATE")
	if err != nil {
		t.Fatalf("insert terminate child: %v", err)
	}

	// Insert REQUEST_CANCEL child.
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, parent_workflow_id, parent_close_policy) VALUES (@p1, @p2, @p3, @p4, @p5, @p6)`,
		childCancelID, "test-parent-close-policy", 1, "running", parentID, "REQUEST_CANCEL")
	if err != nil {
		t.Fatalf("insert cancel child: %v", err)
	}

	store := NewMSSQLStore(db, "default")
	store.tenantID = "00000000-0000-0000-0000-000000000000"
	store.enforceParentClosePolicy(context.Background(), parentID)

	// Verify TERMINATE child is now failed.
	var status, errorMsg string
	err = db.QueryRow(`SELECT status, error_msg FROM workflow_instances WHERE id = @p1`, childTerminateID).Scan(&status, &errorMsg)
	if err != nil {
		t.Fatalf("query terminate child: %v", err)
	}
	if status != "failed" {
		t.Errorf("terminate child status = %q, want failed", status)
	}
	if errorMsg != "parent workflow terminated" {
		t.Errorf("terminate child error_msg = %q, want 'parent workflow terminated'", errorMsg)
	}

	// Verify REQUEST_CANCEL child has cancellation_requested = 1.
	var cancelled bool
	err = db.QueryRow(`SELECT cancellation_requested FROM workflow_instances WHERE id = @p1`, childCancelID).Scan(&cancelled)
	if err != nil {
		t.Fatalf("query cancel child: %v", err)
	}
	if !cancelled {
		t.Error("cancel child should have cancellation_requested = 1")
	}

	// Verify parent is unchanged.
	var parentStatus string
	err = db.QueryRow(`SELECT status FROM workflow_instances WHERE id = @p1`, parentID).Scan(&parentStatus)
	if err != nil {
		t.Fatalf("query parent: %v", err)
	}
	if parentStatus != "running" {
		t.Errorf("parent status = %q, want running", parentStatus)
	}
}
