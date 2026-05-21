package host

import (
	"context"
	"database/sql"
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

// TestCascadeDelete verifies that ON DELETE CASCADE foreign keys work correctly
// on all five child tables referencing workflow_instances. It tests against
// each available database backend using raw SQL.
func TestCascadeDelete(t *testing.T) {
	dialects := []struct {
		name    string
		dialect testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	}

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if d.name == "mysql" && os.Getenv("CLEAT_TEST_MYSQL") == "" {
				t.Skip("CLEAT_TEST_MYSQL not set")
			}
			if d.name == "mssql" && os.Getenv("CLEAT_TEST_MSSQL") == "" {
				t.Skip("CLEAT_TEST_MSSQL not set")
			}

			db := testutil.TestDB(t, d.dialect)
			testutil.SetupFullSchema(t, db, d.dialect)

			addCascadeFKs(t, db, d.dialect)

			// Insert a workflow def so workflow_instances FK is satisfied.
			db.Exec(`DELETE FROM workflow_defs WHERE name = 'cascade-test-def'`)
			_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES ('cascade-test-def', 1, '')`)
			if err != nil {
				// Retry with dialect-specific empty blob.
				_, err = db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES ('cascade-test-def', 1, '\x')`)
			}
			if err != nil {
				t.Fatalf("insert workflow_defs: %v", err)
			}

			wfID := "cascade-test-001"

			// Insert workflow instance.
			insertWorkflowInstance(t, db, d.dialect, wfID)

			// Insert child rows in all 5 tables.
			insertChildRows(t, db, d.dialect, wfID)

			// Delete the workflow instance - cascade should clean up children.
			_, err = db.Exec(`DELETE FROM workflow_instances WHERE id = '` + wfID + `'`)
			if err != nil {
				t.Fatalf("delete workflow_instances: %v", err)
			}

			// Verify all child rows are gone.
			childTables := []string{
				"event_history",
				"workflow_signals",
				"workflow_promises",
				"concurrency_keys",
				"workflow_update_requests",
			}
			for _, table := range childTables {
				var count int
				query := `SELECT COUNT(*) FROM ` + table + ` WHERE workflow_id = '` + wfID + `'`
				switch d.dialect {
				case testutil.DialectMSSQL:
					query = `SELECT COUNT(*) FROM [` + table + `] WHERE workflow_id = '` + wfID + `'`
				}
				if err := db.QueryRow(query).Scan(&count); err != nil {
					t.Errorf("count %s: %v", table, err)
				}
				if count != 0 {
					t.Errorf("%s: expected 0 rows after cascade delete, got %d", table, count)
				}
			}

			// Clean up workflow_defs.
			db.Exec(`DELETE FROM workflow_defs WHERE name = 'cascade-test-def'`)

			testutil.CleanupTestData(t, db, d.dialect, "cascade-test-%")
			db.Close()
		})
	}
}

// addCascadeFKs adds ON DELETE CASCADE foreign keys to the test schema.
// The approach differs by dialect because the test schemas have different FK states:
//   - Postgres: no FKs at all, so add them directly.
//   - MySQL: 4 tables have FKs (drop+re-add), concurrency_keys has none (add fresh).
//   - MSSQL: 4 tables have inline REFERENCES (auto-named, IF EXISTS skips them),
//     ADD CONSTRAINT creates named CASCADE FK alongside. concurrency_keys has no FK.
func addCascadeFKs(t *testing.T, db *sql.DB, dialect testutil.Dialect) {
	t.Helper()

	exec := func(sql string) {
		t.Helper()
		if _, err := db.Exec(sql); err != nil {
			t.Fatalf("add CASCADE FK: %v\nSQL: %s", err, sql)
		}
	}

	switch dialect {
	case testutil.DialectPostgres:
		// Drop test constraints from prior runs so re-runs are idempotent.
		exec(`ALTER TABLE event_history DROP CONSTRAINT IF EXISTS fk_test_cascade_eh`)
		exec(`ALTER TABLE workflow_signals DROP CONSTRAINT IF EXISTS fk_test_cascade_ws`)
		exec(`ALTER TABLE workflow_promises DROP CONSTRAINT IF EXISTS fk_test_cascade_wp`)
		exec(`ALTER TABLE concurrency_keys DROP CONSTRAINT IF EXISTS fk_test_cascade_ck`)
		exec(`ALTER TABLE workflow_update_requests DROP CONSTRAINT IF EXISTS fk_test_cascade_wu`)
		exec(`ALTER TABLE event_history ADD CONSTRAINT fk_test_cascade_eh FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE workflow_signals ADD CONSTRAINT fk_test_cascade_ws FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE workflow_promises ADD CONSTRAINT fk_test_cascade_wp FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE concurrency_keys ADD CONSTRAINT fk_test_cascade_ck FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)
		exec(`ALTER TABLE workflow_update_requests ADD CONSTRAINT fk_test_cascade_wu FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

	case testutil.DialectMySQL:
		// event_history
		exec(`SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'event_history' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances')`)
		exec(`SET @sql = CONCAT('ALTER TABLE event_history DROP FOREIGN KEY ', @cname)`)
		exec(`PREPARE stmt FROM @sql`)
		exec(`EXECUTE stmt`)
		exec(`DEALLOCATE PREPARE stmt`)
		exec(`ALTER TABLE event_history ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

		// workflow_signals
		exec(`SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_signals' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances')`)
		exec(`SET @sql = CONCAT('ALTER TABLE workflow_signals DROP FOREIGN KEY ', @cname)`)
		exec(`PREPARE stmt FROM @sql`)
		exec(`EXECUTE stmt`)
		exec(`DEALLOCATE PREPARE stmt`)
		exec(`ALTER TABLE workflow_signals ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

		// workflow_promises
		exec(`SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_promises' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances')`)
		exec(`SET @sql = CONCAT('ALTER TABLE workflow_promises DROP FOREIGN KEY ', @cname)`)
		exec(`PREPARE stmt FROM @sql`)
		exec(`EXECUTE stmt`)
		exec(`DEALLOCATE PREPARE stmt`)
		exec(`ALTER TABLE workflow_promises ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

		// workflow_update_requests
		exec(`SET @cname = (SELECT CONSTRAINT_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE TABLE_NAME = 'workflow_update_requests' AND CONSTRAINT_SCHEMA = DATABASE() AND REFERENCED_TABLE_NAME = 'workflow_instances')`)
		exec(`SET @sql = CONCAT('ALTER TABLE workflow_update_requests DROP FOREIGN KEY ', @cname)`)
		exec(`PREPARE stmt FROM @sql`)
		exec(`EXECUTE stmt`)
		exec(`DEALLOCATE PREPARE stmt`)
		exec(`ALTER TABLE workflow_update_requests ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

		// concurrency_keys: add FK for the first time
		exec(`ALTER TABLE concurrency_keys ADD FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE`)

	case testutil.DialectMSSQL:
		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_event_history_workflow') ALTER TABLE dbo.event_history DROP CONSTRAINT fk_event_history_workflow`)
		exec(`ALTER TABLE dbo.event_history ADD CONSTRAINT fk_event_history_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)

		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_signals_workflow') ALTER TABLE dbo.workflow_signals DROP CONSTRAINT fk_signals_workflow`)
		exec(`ALTER TABLE dbo.workflow_signals ADD CONSTRAINT fk_signals_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)

		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_promises_workflow') ALTER TABLE dbo.workflow_promises DROP CONSTRAINT fk_promises_workflow`)
		exec(`ALTER TABLE dbo.workflow_promises ADD CONSTRAINT fk_promises_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)

		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_concurrency_keys_workflow') ALTER TABLE dbo.concurrency_keys DROP CONSTRAINT fk_concurrency_keys_workflow`)
		exec(`ALTER TABLE dbo.concurrency_keys ADD CONSTRAINT fk_concurrency_keys_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)

		exec(`IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_update_requests_workflow') ALTER TABLE dbo.workflow_update_requests DROP CONSTRAINT fk_update_requests_workflow`)
		exec(`ALTER TABLE dbo.workflow_update_requests ADD CONSTRAINT fk_update_requests_workflow FOREIGN KEY (workflow_id) REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE`)
	}
}

// insertWorkflowInstance inserts a single workflow_instances row for cascade testing.
func insertWorkflowInstance(t *testing.T, db *sql.DB, dialect testutil.Dialect, wfID string) {
	t.Helper()

	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at) VALUES ($1, 'cascade-test-def', 1, 'dead_lettered', NOW())`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_instances (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at) VALUES (?, 'cascade-test-def', 1, 'dead_lettered', NOW(6))`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_instances (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at) VALUES (@p1, 'cascade-test-def', 1, 'dead_lettered', SYSUTCDATETIME())`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_instances (mssql): %v", err)
		}
	}
}

// insertChildRows inserts one row into each of the 5 child tables for cascade testing.
func insertChildRows(t *testing.T, db *sql.DB, dialect testutil.Dialect, wfID string) {
	t.Helper()

	// event_history
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO event_history (workflow_id, step, service, operation, request, event_type) VALUES ($1, 1, 'test-svc', 'test-op', '{}', 'call')`, wfID)
		if err != nil {
			t.Fatalf("insert event_history (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO event_history (workflow_id, step, service, operation, request, event_type) VALUES (?, 1, 'test-svc', 'test-op', '{}', 'call')`, wfID)
		if err != nil {
			t.Fatalf("insert event_history (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO event_history (workflow_id, step, service, operation, request, event_type) VALUES (@p1, 1, 'test-svc', 'test-op', '{}', 'call')`, wfID)
		if err != nil {
			t.Fatalf("insert event_history (mssql): %v", err)
		}
	}

	// workflow_signals
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO workflow_signals (workflow_id, signal_name) VALUES ($1, 'test-signal')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_signals (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO workflow_signals (workflow_id, signal_name) VALUES (?, 'test-signal')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_signals (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO workflow_signals (workflow_id, signal_name) VALUES (@p1, 'test-signal')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_signals (mssql): %v", err)
		}
	}

	// workflow_promises
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name) VALUES ($1, 'promise-1', 'test-promise')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_promises (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id) VALUES (?, 'promise-1', 'test-promise', '')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_promises (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id) VALUES (@p1, 'promise-1', 'test-promise', '')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_promises (mssql): %v", err)
		}
	}

	// concurrency_keys
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at) VALUES (digest('cascade-test-key', 'sha256'), 'cascade-test-key', $1, NOW() + INTERVAL '1 hour')`, wfID)
		if err != nil {
			t.Fatalf("insert concurrency_keys (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at) VALUES (UNHEX(SHA2('cascade-test-key', 256)), 'cascade-test-key', ?, NOW(6) + INTERVAL 1 HOUR)`, wfID)
		if err != nil {
			t.Fatalf("insert concurrency_keys (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at) VALUES (HASHBYTES('SHA2_256', 'cascade-test-key'), 'cascade-test-key', @p1, DATEADD(HOUR, 1, SYSUTCDATETIME()))`, wfID)
		if err != nil {
			t.Fatalf("insert concurrency_keys (mssql): %v", err)
		}
	}

	// workflow_update_requests
	switch dialect {
	case testutil.DialectPostgres:
		_, err := db.Exec(`INSERT INTO workflow_update_requests (workflow_id, update_name) VALUES ($1, 'test-update')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_update_requests (postgres): %v", err)
		}
	case testutil.DialectMySQL:
		_, err := db.Exec(`INSERT INTO workflow_update_requests (workflow_id, update_name) VALUES (?, 'test-update')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_update_requests (mysql): %v", err)
		}
	case testutil.DialectMSSQL:
		_, err := db.Exec(`INSERT INTO workflow_update_requests (workflow_id, update_name) VALUES (@p1, 'test-update')`, wfID)
		if err != nil {
			t.Fatalf("insert workflow_update_requests (mssql): %v", err)
		}
	}
}
