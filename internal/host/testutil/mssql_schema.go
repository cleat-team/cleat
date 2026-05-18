package testutil

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// SetupMSSQLMinimalSchema creates the core tables needed for MSSQLStore tests.
// These are the minimum tables required: workflow_defs, workflow_instances, event_history, workflow_signals.
func SetupMSSQLMinimalSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	statements := []string{
		// workflow_defs
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_defs')
         CREATE TABLE workflow_defs (
             name NVARCHAR(900) NOT NULL,
             version INTEGER NOT NULL,
             wasm_bytes VARBINARY(MAX) NOT NULL,
             entry_points NVARCHAR(MAX) NOT NULL DEFAULT '[]',
             min_version INTEGER NOT NULL DEFAULT 0,
             abi_version INTEGER NOT NULL DEFAULT 1,
             plugin_deps NVARCHAR(MAX) NOT NULL DEFAULT '{}',
             deprecated BIT NOT NULL DEFAULT 0,
             task_queue NVARCHAR(MAX) NOT NULL DEFAULT 'default',
             max_history_length INTEGER NOT NULL DEFAULT 0,
             dag_spec NVARCHAR(MAX) DEFAULT NULL,
             tenant_id UNIQUEIDENTIFIER,
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             PRIMARY KEY (name, version)
         )`,

		// workflow_instances
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_instances')
         CREATE TABLE workflow_instances (
             id NVARCHAR(900) NOT NULL PRIMARY KEY,
             def_name NVARCHAR(900) NOT NULL,
             def_version INTEGER NOT NULL,
             status NVARCHAR(MAX) NOT NULL DEFAULT 'ready',
             input NVARCHAR(MAX) NOT NULL DEFAULT '{}',
             assigned_to NVARCHAR(MAX),
             heartbeat_at DATETIMEOFFSET,
             next_wake_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             completed_at DATETIMEOFFSET,
             cancellation_requested BIT NOT NULL DEFAULT 0,
             cancellation_reason NVARCHAR(MAX),
             result NVARCHAR(MAX),
             error_msg NVARCHAR(MAX),
             error_code NVARCHAR(MAX),
             error_op NVARCHAR(MAX),
             parent_workflow_id NVARCHAR(MAX),
             parent_close_policy NVARCHAR(MAX) DEFAULT 'ABANDON',
             query_state NVARCHAR(MAX) DEFAULT '{}',
             task_queue NVARCHAR(MAX) NOT NULL DEFAULT 'default',
             trace_id NVARCHAR(MAX),
             sticky_worker_id NVARCHAR(MAX),
             compaction_state NVARCHAR(MAX),
             compacted_at DATETIMEOFFSET,
             compaction_step INTEGER,
             plugin_vers NVARCHAR(MAX) NOT NULL DEFAULT '{}',
             tenant_id UNIQUEIDENTIFIER,
             generation BIGINT NOT NULL DEFAULT 0,
             FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
         )`,

		// event_history
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'event_history')
         CREATE TABLE event_history (
             workflow_id NVARCHAR(900) NOT NULL REFERENCES workflow_instances(id),
             step INTEGER NOT NULL,
             event_type NVARCHAR(MAX) NOT NULL DEFAULT 'call',
             service NVARCHAR(MAX),
             operation NVARCHAR(MAX),
             request NVARCHAR(MAX),
             response NVARCHAR(MAX),
             error NVARCHAR(MAX),
             duration_ms BIGINT,
             signal_names NVARCHAR(MAX),
             timeout_ms BIGINT,
             signal_name NVARCHAR(MAX),
             signal_payload NVARCHAR(MAX),
             defer_description NVARCHAR(MAX),
             defer_id NVARCHAR(MAX),
             child_name NVARCHAR(MAX),
             child_input NVARCHAR(MAX),
             run_id NVARCHAR(MAX),
             new_input NVARCHAR(MAX),
             plugin_name NVARCHAR(MAX),
             plugin_func NVARCHAR(MAX),
             plugin_input NVARCHAR(MAX),
             plugin_output NVARCHAR(MAX),
             plugin_error NVARCHAR(MAX),
             promise_name NVARCHAR(MAX),
             promise_id NVARCHAR(MAX),
             promise_result NVARCHAR(MAX),
             promise_error NVARCHAR(MAX),
             payload NVARCHAR(MAX),
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             checksum NVARCHAR(MAX),
             tenant_id UNIQUEIDENTIFIER,
             PRIMARY KEY (workflow_id, step)
         )`,

		// workflow_signals
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_signals')
         CREATE TABLE workflow_signals (
             workflow_id NVARCHAR(900) NOT NULL REFERENCES workflow_instances(id),
             signal_name NVARCHAR(900) NOT NULL,
             payload NVARCHAR(MAX) NOT NULL DEFAULT '{}',
             delivered_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             tenant_id UNIQUEIDENTIFIER,
             PRIMARY KEY (workflow_id, signal_name)
         )`,
	}

	for i, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup MSSQL minimal schema: statement %d: %v", i, err)
		}
	}
}

// SetupMSSQLFullSchema creates all tables needed for full WorkflowStore testing.
// Includes schedules, concurrency_keys, promises, update_requests, idempotency_keys,
// memory stats, and plugin_defs.
func SetupMSSQLFullSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	SetupMSSQLMinimalSchema(t, db)

	statements := []string{
		// workflow_schedules
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_schedules')
         CREATE TABLE workflow_schedules (
             name NVARCHAR(900) NOT NULL PRIMARY KEY,
             def_name NVARCHAR(900) NOT NULL,
             entry_point NVARCHAR(MAX) NOT NULL DEFAULT '',
             cron_expression NVARCHAR(MAX) NOT NULL,
             input NVARCHAR(MAX) NOT NULL DEFAULT '{}',
             enabled BIT NOT NULL DEFAULT 1,
             next_run_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             last_run_at DATETIMEOFFSET,
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             tenant_id UNIQUEIDENTIFIER
         )`,

		// concurrency_keys
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'concurrency_keys')
         CREATE TABLE concurrency_keys (
             key_hash VARBINARY(900) NOT NULL PRIMARY KEY,
             key_text NVARCHAR(MAX) NOT NULL,
             workflow_id NVARCHAR(900) NOT NULL,
             acquired_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             expires_at DATETIMEOFFSET NOT NULL,
             tenant_id NVARCHAR(128)
         )`,

		// workflow_promises
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_promises')
         CREATE TABLE workflow_promises (
             workflow_id NVARCHAR(900) NOT NULL REFERENCES workflow_instances(id),
             promise_id NVARCHAR(900) NOT NULL,
             tenant_id NVARCHAR(255) NOT NULL,
             promise_name NVARCHAR(MAX) NOT NULL,
             status NVARCHAR(MAX) NOT NULL DEFAULT 'pending',
             result NVARCHAR(MAX),
             error_msg NVARCHAR(MAX),
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             resolved_at DATETIMEOFFSET,
             PRIMARY KEY (workflow_id, promise_id)
         )`,

		// workflow_update_requests
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_update_requests')
         CREATE TABLE workflow_update_requests (
             workflow_id NVARCHAR(900) NOT NULL REFERENCES workflow_instances(id),
             update_name NVARCHAR(900) NOT NULL,
             payload NVARCHAR(MAX) NOT NULL DEFAULT '{}',
             promise_id NVARCHAR(MAX),
             status NVARCHAR(MAX) NOT NULL DEFAULT 'pending',
             result NVARCHAR(MAX),
             error_msg NVARCHAR(MAX),
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             completed_at DATETIMEOFFSET,
             PRIMARY KEY (workflow_id, update_name)
         )`,

		// idempotency_keys
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'idempotency_keys')
         CREATE TABLE idempotency_keys (
             key_hash VARBINARY(900) NOT NULL PRIMARY KEY,
             workflow_id NVARCHAR(MAX) NOT NULL,
             result NVARCHAR(MAX),
             error_msg NVARCHAR(MAX),
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             expires_at DATETIMEOFFSET NOT NULL DEFAULT DATEADD(DAY, 7, SYSUTCDATETIME())
         )`,

		// workflow_memory_samples (ID column uses IDENTITY, not as PK since it's BIGINT IDENTITY instead of BIGSERIAL)
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_memory_samples')
         CREATE TABLE workflow_memory_samples (
             id BIGINT IDENTITY(1,1) PRIMARY KEY,
             def_name NVARCHAR(MAX) NOT NULL,
             sample_bytes BIGINT NOT NULL,
             recorded_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
         )`,

		// workflow_memory_stats
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_memory_stats')
         CREATE TABLE workflow_memory_stats (
             def_name NVARCHAR(900) NOT NULL PRIMARY KEY,
             mean_bytes FLOAT(53) NOT NULL DEFAULT 0,
             sample_count INTEGER NOT NULL DEFAULT 0,
             alpha FLOAT(53) NOT NULL DEFAULT 0.3,
             updated_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
         )`,

		// plugin_defs
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'plugin_defs')
         CREATE TABLE plugin_defs (
             name NVARCHAR(900) NOT NULL,
             version NVARCHAR(900) NOT NULL,
             wasm_bytes VARBINARY(MAX),
             config NVARCHAR(MAX) NOT NULL DEFAULT '{}',
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             deprecated BIT NOT NULL DEFAULT 0,
             PRIMARY KEY (name, version)
         )`,
	}

	for i, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup MSSQL full schema: statement %d: %v", i, err)
		}
	}
}

// CleanupMSSQLTestData removes all test data from the MSSQL tables.
// Uses DELETE with table existence checks. Order respects FK constraints.
func CleanupMSSQLTestData(t *testing.T, db *sql.DB) {
	t.Helper()

	// Order matters due to FK constraints — delete child tables first.
	tables := []string{
		"workflow_update_requests",
		"workflow_promises",
		"workflow_signals",
		"concurrency_keys",
		"idempotency_keys",
		"event_history",
		"workflow_memory_samples",
		"workflow_memory_stats",
		"workflow_schedules",
		"workflow_instances",
		"workflow_defs",
		"plugin_defs",
	}

	for _, table := range tables {
		var exists int
		err := db.QueryRow("SELECT COUNT(1) FROM sys.tables WHERE name = @p1", table).Scan(&exists)
		if err != nil {
			t.Logf("cleanup: check table %s: %v", table, err)
			continue
		}
		if exists > 0 {
			if _, err := db.Exec(fmt.Sprintf("DELETE FROM [%s]", table)); err != nil {
				t.Logf("cleanup: delete from %s: %v", table, err)
			}
		}
	}
}

// MSSQLTestDB opens a connection to the MSSQL test database.
// Uses CLEAT_TEST_MSSQL environment variable.
// Default: sqlserver://sa:CleatTest123!@localhost:1433?database=cleat
func MSSQLTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping MSSQL database test in short mode")
	}

	connStr := os.Getenv("CLEAT_TEST_MSSQL")
	if connStr == "" {
		connStr = "sqlserver://sa:CleatTest123!@localhost:1433?database=cleat"
		t.Logf("CLEAT_TEST_MSSQL not set, using default: %s", connStr)
	}

	db, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open MSSQL test DB: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("ping MSSQL test DB: %v\nHint: Is SQL Server running? Start with:\n  docker run -e 'ACCEPT_EULA=Y' -e 'MSSQL_SA_PASSWORD=CleatTest123!' -p 1433:1433 -d mcr.microsoft.com/mssql/server:2022-latest", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db
}
