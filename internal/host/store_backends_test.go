package host

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/internal/host/testutil"
)

// StoreBackend represents a database backend that can be tested.
// Each backend (PostgreSQL, MySQL, SQL Server) implements this interface
// so that shared test groups can run against all of them.
type StoreBackend interface {
	// Name returns the backend name, e.g. "postgres", "mysql", "mssql".
	Name() string
	// Setup creates a WorkflowStore for testing and returns a teardown function.
	Setup(t *testing.T) (WorkflowStore, func())
	// Enabled reports whether this backend is available in the current environment.
	Enabled() bool
}

// registeredBackends holds all backends registered via RegisterBackend.
var registeredBackends []StoreBackend

// RegisterBackend adds a backend to the test suite.
func RegisterBackend(b StoreBackend) {
	registeredBackends = append(registeredBackends, b)
}

// PostgresBackend implements StoreBackend for PostgreSQL.
type PostgresBackend struct{}

func (b *PostgresBackend) Name() string {
	return "postgres"
}

func (b *PostgresBackend) Enabled() bool {
	return true
}

func (b *PostgresBackend) Setup(t *testing.T) (WorkflowStore, func()) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	store := NewPostgresStore(db)
	teardown := func() {
		// Clean up all test data in the right order (child tables first).
		db.Exec(`DELETE FROM workflow_schedules`)
		db.Exec(`DELETE FROM workflow_promises`)
		db.Exec(`DELETE FROM concurrency_keys`)
		db.Exec(`DELETE FROM event_history`)
		db.Exec(`DELETE FROM workflow_signals`)
		db.Exec(`DELETE FROM workflow_instances`)
		db.Exec(`DELETE FROM workflow_defs`)
		db.Close()
	}
	return store, teardown
}

func (b *PostgresBackend) SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func()) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	store := NewPostgresStore(db)
	store.tenantID = tenantID
	teardown := func() {
		db.Exec(`DELETE FROM workflow_schedules`)
		db.Exec(`DELETE FROM workflow_promises`)
		db.Exec(`DELETE FROM concurrency_keys`)
		db.Exec(`DELETE FROM event_history`)
		db.Exec(`DELETE FROM workflow_signals`)
		db.Exec(`DELETE FROM workflow_instances`)
		db.Exec(`DELETE FROM workflow_defs`)
		db.Close()
	}
	return store, teardown
}

// MySQLBackend implements StoreBackend for MySQL 8.0+ / MariaDB 10.6+.
type MySQLBackend struct{}

func (b *MySQLBackend) Name() string {
	return "mysql"
}

func (b *MySQLBackend) Enabled() bool {
	return os.Getenv("CLEAT_TEST_MYSQL") != ""
}

func (b *MySQLBackend) Setup(t *testing.T) (WorkflowStore, func()) {
	t.Helper()
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	db := testutil.MySQLTestDB(t)
	testutil.SetupMySQLFullSchema(t, db)
	store := NewMySQLStore(db)
	teardown := func() {
		testutil.CleanupMySQLTestData(t, db)
		db.Close()
	}
	return store, teardown
}

func (b *MySQLBackend) SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func()) {
	t.Helper()
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	db := testutil.MySQLTestDB(t)
	testutil.SetupMySQLFullSchema(t, db)
	store := NewMySQLStore(db)
	store.tenantID = tenantID
	teardown := func() {
		testutil.CleanupMySQLTestData(t, db)
		db.Close()
	}
	return store, teardown
}

// MSSQLBackend implements StoreBackend for SQL Server 2017+ / Azure SQL Database.
type MSSQLBackend struct{}

func (b *MSSQLBackend) Name() string {
	return "mssql"
}

func (b *MSSQLBackend) Enabled() bool {
	return os.Getenv("CLEAT_TEST_MSSQL") != ""
}

func (b *MSSQLBackend) Setup(t *testing.T) (WorkflowStore, func()) {
	t.Helper()
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	store := NewMSSQLStore(db)
	teardown := func() {
		testutil.CleanupMSSQLTestData(t, db)
		db.Close()
	}
	return store, teardown
}

func (b *MSSQLBackend) SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func()) {
	t.Helper()
	if !b.Enabled() {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	store := NewMSSQLStore(db)
	store.tenantID = tenantID
	teardown := func() {
		testutil.CleanupMSSQLTestData(t, db)
		db.Close()
	}
	return store, teardown
}

func init() {
	RegisterBackend(&PostgresBackend{})
	RegisterBackend(&MySQLBackend{})
	RegisterBackend(&MSSQLBackend{})
}

// setupTestData creates standard test fixtures needed by most test groups.
// Call this after backend.Setup() in each test.
func setupTestData(t *testing.T, store WorkflowStore) {
	t.Helper()

	// Deploy a workflow def so instances can reference it
	def := &WorkflowDef{
		Name:       "test-workflow",
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d}, // minimal WASM magic bytes
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(context.Background(), def); err != nil {
		t.Fatalf("setupTestData: DeployWorkflowDef: %v", err)
	}

	// Create workflow instances in various states for testing
	now := time.Now()

	// A "ready" workflow instance
	readyWfID, _, err := store.StartNewRun(context.Background(), "", "test-workflow", 1,
		json.RawMessage(`{"key":"value"}`), "setup-ready-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("setupTestData: StartNewRun ready: %v", err)
	}

	// A "running" workflow instance
	_, _, err = store.StartNewRun(context.Background(), "", "test-workflow", 1,
		json.RawMessage(`{"key":"running"}`), "setup-running-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("setupTestData: StartNewRun running: %v", err)
	}
	// Claim it to make it running
	_, err = store.ClaimWorkflow(context.Background(), "test-worker")
	if err != nil {
		t.Fatalf("setupTestData: ClaimWorkflow: %v", err)
	}

	// Create a schedule for testing
	err = store.CreateSchedule(context.Background(), Schedule{
		Name:           "test-schedule",
		DefName:        "test-workflow",
		EntryPoint:     "main",
		CronExpression: "* * * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      now.Add(-1 * time.Hour), // due now
	})
	if err != nil {
		t.Fatalf("setupTestData: CreateSchedule: %v", err)
	}

	// Create a promise for testing
	err = store.CreatePromise(context.Background(), readyWfID, "test-promise", "promise-1")
	if err != nil {
		t.Fatalf("setupTestData: CreatePromise: %v", err)
	}
}

// truncateAll cleans up ALL test data between test cases.
// setupTestData leaves behind a "ready" workflow and a "running" workflow
// that DeleteExpiredEvents does not touch (it only cleans events for
// terminal workflows).  We delete every row from all dynamic tables so that
// subtests and subsequent tests each start with a truly empty database.
func truncateAll(t *testing.T, store WorkflowStore) {
	t.Helper()
	ctx := context.Background()

	// Delete schedules via the store interface.
	_ = store.DeleteSchedule(ctx, "test-schedule")
	_ = store.DeleteSchedule(ctx, "test-schedule-2")

	// Delete expired events for terminal workflows (a no-op for ready/running
	// instances, but harmless).
	_, _ = store.DeleteExpiredEvents(ctx, time.Now().Add(1*time.Hour))

	// Wipe rows from dynamic tables so the next test section starts clean.
	// Type-switch on the concrete store so we can access the unexported *sql.DB.
	//
	// Only tables present in all three backend test schemas are included.
	// The deletion order respects MySQL's FK constraints (child rows first).
	// MySQL-only tables (workflow_update_requests, idempotency_keys) are
	// handled in a separate branch.
	switch s := store.(type) {
	case *MySQLStore:
		s.db.Exec("DELETE FROM workflow_update_requests")
		s.db.Exec("DELETE FROM workflow_promises")
		s.db.Exec("DELETE FROM workflow_signals")
		s.db.Exec("DELETE FROM concurrency_keys")
		s.db.Exec("DELETE FROM idempotency_keys")
		s.db.Exec("DELETE FROM event_history")
		s.db.Exec("DELETE FROM workflow_instances")
	case *PostgresStore:
		s.db.Exec("DELETE FROM workflow_update_requests")
		s.db.Exec("DELETE FROM workflow_promises")
		s.db.Exec("DELETE FROM workflow_signals")
		s.db.Exec("DELETE FROM concurrency_keys")
		s.db.Exec("DELETE FROM idempotency_keys")
		s.db.Exec("DELETE FROM event_history")
		s.db.Exec("DELETE FROM workflow_instances")
	case *MSSQLStore:
		s.db.Exec("DELETE FROM workflow_update_requests")
		s.db.Exec("DELETE FROM workflow_promises")
		s.db.Exec("DELETE FROM workflow_signals")
		s.db.Exec("DELETE FROM concurrency_keys")
		s.db.Exec("DELETE FROM idempotency_keys")
		s.db.Exec("DELETE FROM event_history")
		s.db.Exec("DELETE FROM workflow_instances")
	}
}
