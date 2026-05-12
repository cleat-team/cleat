package testutil

import (
	"database/sql"
	"os"
	"testing"
)

// SetupMinimalSchema creates the minimal tables needed by all DB tests:
// workflow_defs, workflow_instances, event_history, workflow_signals.
// Uses CREATE TABLE IF NOT EXISTS (or equivalent) so it is idempotent.
func SetupMinimalSchema(t *testing.T, db *sql.DB, dialect Dialect) {
	t.Helper()

	var stmts []string
	switch dialect {
	case DialectPostgres:
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS workflow_defs (
				name TEXT NOT NULL, version INTEGER NOT NULL,
				wasm_bytes BYTEA NOT NULL, entry_points TEXT[] NOT NULL DEFAULT '{}',
				min_version INTEGER NOT NULL DEFAULT 0,
				max_history_length INTEGER NOT NULL DEFAULT 0,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				PRIMARY KEY (name, version))`,
			`CREATE TABLE IF NOT EXISTS workflow_instances (
				id TEXT PRIMARY KEY, def_name TEXT NOT NULL, def_version INTEGER NOT NULL DEFAULT 1,
				status TEXT NOT NULL DEFAULT 'ready', input JSONB NOT NULL DEFAULT '{}',
				assigned_to TEXT, heartbeat_at TIMESTAMPTZ,
				next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				created_at TIMESTAMPTZ NOT NULL DEFAULT now(), completed_at TIMESTAMPTZ,
				result JSONB, error_msg TEXT, error_code TEXT, error_op TEXT, parent_workflow_id TEXT,
				trace_id TEXT,
				query_state JSONB DEFAULT '{}', task_queue TEXT NOT NULL DEFAULT 'default',
				cancellation_requested BOOLEAN NOT NULL DEFAULT false,
				cancellation_reason TEXT, sticky_worker_id TEXT)`,
			`CREATE TABLE IF NOT EXISTS event_history (
				workflow_id TEXT NOT NULL, step INTEGER NOT NULL,
				event_type TEXT NOT NULL DEFAULT 'call',
				service TEXT, operation TEXT, request JSONB, response JSONB, error TEXT,
				duration_ms BIGINT, signal_names TEXT, timeout_ms BIGINT,
				signal_name TEXT, signal_payload JSONB, defer_description TEXT,
				defer_id TEXT, child_name TEXT, child_input JSONB, run_id TEXT,
				new_input JSONB, plugin_name TEXT, plugin_func TEXT,
				plugin_input JSONB, plugin_output JSONB, plugin_error TEXT,
				promise_name TEXT, promise_id TEXT, promise_result TEXT, promise_error TEXT,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				PRIMARY KEY (workflow_id, step))`,
			`CREATE TABLE IF NOT EXISTS workflow_signals (
				workflow_id TEXT NOT NULL, signal_name TEXT NOT NULL,
				payload JSONB NOT NULL DEFAULT '{}',
				delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				PRIMARY KEY (workflow_id, signal_name))`,
		}
	case DialectMySQL:
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS workflow_defs (
				name VARCHAR(255) NOT NULL, version INTEGER NOT NULL,
				wasm_bytes LONGBLOB NOT NULL, entry_points JSON NOT NULL DEFAULT ('[]'),
				min_version INTEGER NOT NULL DEFAULT 0,
				max_history_length INTEGER NOT NULL DEFAULT 0,
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				PRIMARY KEY (name, version))`,
			`CREATE TABLE IF NOT EXISTS workflow_instances (
				id VARCHAR(255) PRIMARY KEY, def_name VARCHAR(255) NOT NULL,
				def_version INTEGER NOT NULL DEFAULT 1,
				status VARCHAR(255) NOT NULL DEFAULT 'ready', input JSON NOT NULL DEFAULT ('{}'),
				assigned_to TEXT, heartbeat_at TIMESTAMP(6),
				next_wake_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6), completed_at TIMESTAMP(6),
				result JSON, error_msg TEXT, error_code VARCHAR(255), error_op VARCHAR(255),
				parent_workflow_id TEXT,
				trace_id TEXT, query_state JSON DEFAULT ('{}'),
				task_queue VARCHAR(255) NOT NULL DEFAULT 'default',
				cancellation_requested TINYINT(1) NOT NULL DEFAULT 0,
				cancellation_reason TEXT, sticky_worker_id TEXT)`,
			`CREATE TABLE IF NOT EXISTS event_history (
				workflow_id VARCHAR(255) NOT NULL, step INTEGER NOT NULL,
				event_type VARCHAR(255) NOT NULL DEFAULT 'call',
				service TEXT, operation TEXT, request JSON, response JSON, error TEXT,
				duration_ms BIGINT, signal_names TEXT, timeout_ms BIGINT,
				signal_name TEXT, signal_payload JSON, defer_description TEXT,
				defer_id TEXT, child_name TEXT, child_input JSON, run_id TEXT,
				new_input JSON, plugin_name TEXT, plugin_func TEXT,
				plugin_input JSON, plugin_output JSON, plugin_error TEXT,
				promise_name TEXT, promise_id TEXT, promise_result TEXT, promise_error TEXT,
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				PRIMARY KEY (workflow_id, step))`,
			`CREATE TABLE IF NOT EXISTS workflow_signals (
				workflow_id VARCHAR(255) NOT NULL, signal_name VARCHAR(255) NOT NULL,
				payload JSON NOT NULL DEFAULT ('{}'),
				delivered_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				PRIMARY KEY (workflow_id, signal_name))`,
		}
	case DialectMSSQL:
		stmts = []string{
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_defs')
				CREATE TABLE workflow_defs (
					name NVARCHAR(255) NOT NULL, version INTEGER NOT NULL,
					wasm_bytes VARBINARY(MAX) NOT NULL,
					entry_points NVARCHAR(MAX) NOT NULL DEFAULT '[]',
					min_version INTEGER NOT NULL DEFAULT 0,
					max_history_length INTEGER NOT NULL DEFAULT 0,
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (name, version))`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_instances')
				CREATE TABLE workflow_instances (
					id NVARCHAR(64) NOT NULL PRIMARY KEY,
					def_name NVARCHAR(255) NOT NULL,
					def_version INTEGER NOT NULL DEFAULT 1,
					status NVARCHAR(MAX) NOT NULL DEFAULT 'ready',
					input NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					assigned_to NVARCHAR(MAX), heartbeat_at DATETIMEOFFSET,
					next_wake_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					completed_at DATETIMEOFFSET,
					result NVARCHAR(MAX), error_msg NVARCHAR(MAX),
					error_code NVARCHAR(MAX), error_op NVARCHAR(MAX),
					parent_workflow_id NVARCHAR(MAX),
					trace_id NVARCHAR(MAX),
					query_state NVARCHAR(MAX) DEFAULT ('{}'),
					task_queue NVARCHAR(MAX) NOT NULL DEFAULT 'default',
					cancellation_requested BIT NOT NULL DEFAULT 0,
					cancellation_reason NVARCHAR(MAX),
					sticky_worker_id NVARCHAR(MAX))`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'event_history')
				CREATE TABLE event_history (
					workflow_id NVARCHAR(64) NOT NULL, step INTEGER NOT NULL,
					event_type NVARCHAR(MAX) NOT NULL DEFAULT 'call',
					service NVARCHAR(MAX), operation NVARCHAR(MAX),
					request NVARCHAR(MAX), response NVARCHAR(MAX), error NVARCHAR(MAX),
					duration_ms BIGINT, signal_names NVARCHAR(MAX), timeout_ms BIGINT,
					signal_name NVARCHAR(MAX), signal_payload NVARCHAR(MAX),
					defer_description NVARCHAR(MAX),
					defer_id NVARCHAR(MAX), child_name NVARCHAR(MAX),
					child_input NVARCHAR(MAX), run_id NVARCHAR(MAX),
					new_input NVARCHAR(MAX), plugin_name NVARCHAR(MAX),
					plugin_func NVARCHAR(MAX),
					plugin_input NVARCHAR(MAX), plugin_output NVARCHAR(MAX),
					plugin_error NVARCHAR(MAX),
					promise_name NVARCHAR(MAX), promise_id NVARCHAR(MAX),
					promise_result NVARCHAR(MAX), promise_error NVARCHAR(MAX),
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (workflow_id, step))`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_signals')
				CREATE TABLE workflow_signals (
					workflow_id NVARCHAR(64) NOT NULL,
					signal_name NVARCHAR(255) NOT NULL,
					payload NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					delivered_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (workflow_id, signal_name))`,
		}
	default:
		t.Fatalf("setup minimal schema: unknown dialect: %s", dialect)
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
}

// SetupFullSchema calls SetupMinimalSchema and then adds the remaining tables
// (workflow_schedules, concurrency_keys, workflow_promises), all indexes,
// the pgcrypto extension (PostgreSQL only), and ALTER TABLE ADD COLUMN IF NOT
// EXISTS (PostgreSQL only).
func SetupFullSchema(t *testing.T, db *sql.DB, dialect Dialect) {
	t.Helper()
	SetupMinimalSchema(t, db, dialect)

	var stmts []string
	switch dialect {
	case DialectPostgres:
		stmts = []string{
			// Additional tables
			`CREATE TABLE IF NOT EXISTS workflow_schedules (
				name TEXT PRIMARY KEY, def_name TEXT NOT NULL,
				entry_point TEXT NOT NULL DEFAULT '', cron_expression TEXT NOT NULL,
				input JSONB NOT NULL DEFAULT '{}', enabled BOOLEAN NOT NULL DEFAULT true,
				next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_run_at TIMESTAMPTZ,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
			`CREATE TABLE IF NOT EXISTS concurrency_keys (
				key_hash BYTEA PRIMARY KEY, key_text TEXT NOT NULL,
				workflow_id TEXT NOT NULL, acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				expires_at TIMESTAMPTZ NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS workflow_promises (
				workflow_id TEXT NOT NULL, promise_id TEXT NOT NULL,
				promise_name TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
				result JSONB, error_msg TEXT,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ,
				PRIMARY KEY (workflow_id, promise_id))`,
			// workflow_instances indexes
			`CREATE INDEX IF NOT EXISTS idx_instances_ready ON workflow_instances(status, next_wake_at) WHERE status = 'ready'`,
			`CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running'`,
			`CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running'`,
			`CREATE INDEX IF NOT EXISTS idx_instances_sticky ON workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL`,
			// concurrency_keys index
			`CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`,
			// pgcrypto extension (required for digest(), gen_random_uuid())
			`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
			// ALTER TABLE ADD COLUMN IF NOT EXISTS for migrated columns
			`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS tenant_id TEXT`,
			`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS sticky_worker_id TEXT`,
			`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS error_code TEXT`,
			`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS error_op TEXT`,
			`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS payload JSONB`,
			// Memory statistics tables (migration 010)
			`CREATE TABLE IF NOT EXISTS workflow_memory_samples (
				id BIGSERIAL PRIMARY KEY,
				def_name TEXT NOT NULL,
				sample_bytes BIGINT NOT NULL,
				recorded_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
			`CREATE INDEX IF NOT EXISTS idx_mem_samples_def ON workflow_memory_samples(def_name, recorded_at DESC)`,
			`CREATE TABLE IF NOT EXISTS workflow_memory_stats (
				def_name TEXT PRIMARY KEY,
				mean_bytes DOUBLE PRECISION NOT NULL DEFAULT 0,
				sample_count INTEGER NOT NULL DEFAULT 0,
				alpha DOUBLE PRECISION NOT NULL DEFAULT 0.3,
				updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		}
	case DialectMySQL:
		stmts = []string{
			// Additional tables
			`CREATE TABLE IF NOT EXISTS workflow_schedules (
				name VARCHAR(255) PRIMARY KEY, def_name VARCHAR(255) NOT NULL,
				entry_point VARCHAR(255) NOT NULL DEFAULT '', cron_expression TEXT NOT NULL,
				input JSON NOT NULL DEFAULT ('{}'), enabled TINYINT(1) NOT NULL DEFAULT 1,
				next_run_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6), last_run_at TIMESTAMP(6),
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6))`,
			`CREATE TABLE IF NOT EXISTS concurrency_keys (
				key_hash VARBINARY(255) PRIMARY KEY, key_text TEXT NOT NULL,
				workflow_id TEXT NOT NULL,
				acquired_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				expires_at TIMESTAMP(6) NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS workflow_promises (
				workflow_id VARCHAR(255) NOT NULL, promise_id VARCHAR(255) NOT NULL,
				promise_name TEXT NOT NULL,
				status VARCHAR(255) NOT NULL DEFAULT 'pending',
				result JSON, error_msg TEXT,
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				resolved_at TIMESTAMP(6),
				PRIMARY KEY (workflow_id, promise_id))`,
			// workflow_instances indexes — no WHERE clause (MySQL does not support partial indexes)
			`CREATE INDEX idx_instances_ready ON workflow_instances(status, next_wake_at)`,
			`CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at)`,
			`CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at)`,
			`CREATE INDEX idx_instances_sticky ON workflow_instances(sticky_worker_id)`,
			// concurrency_keys index
			`CREATE INDEX idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`,
			// error_code/error_op columns are created by SetupMinimalSchema.
			// MySQL does not support IF NOT EXISTS for ALTER TABLE ADD COLUMN,
			// so existing test databases must be recreated if upgrading.
			// Memory statistics tables
			`CREATE TABLE IF NOT EXISTS workflow_memory_samples (
				id BIGINT AUTO_INCREMENT PRIMARY KEY,
				def_name VARCHAR(255) NOT NULL,
				sample_bytes BIGINT NOT NULL,
				recorded_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6))`,
			`CREATE INDEX idx_mem_samples_def ON workflow_memory_samples(def_name, recorded_at DESC)`,
			`CREATE TABLE IF NOT EXISTS workflow_memory_stats (
				def_name VARCHAR(255) PRIMARY KEY,
				mean_bytes DOUBLE NOT NULL DEFAULT 0,
				sample_count INTEGER NOT NULL DEFAULT 0,
				alpha DOUBLE NOT NULL DEFAULT 0.3,
				updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6))`,
		}
	case DialectMSSQL:
		stmts = []string{
			// Additional tables with IF NOT EXISTS (MSSQL does not support CREATE TABLE IF NOT EXISTS)
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_schedules')
				CREATE TABLE workflow_schedules (
					name NVARCHAR(255) PRIMARY KEY, def_name NVARCHAR(MAX) NOT NULL,
					entry_point NVARCHAR(MAX) NOT NULL DEFAULT '',
					cron_expression NVARCHAR(MAX) NOT NULL,
					input NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					enabled BIT NOT NULL DEFAULT 1,
					next_run_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					last_run_at DATETIMEOFFSET,
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME())`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'concurrency_keys')
				CREATE TABLE concurrency_keys (
					key_hash VARBINARY(32) NOT NULL PRIMARY KEY,
					key_text NVARCHAR(MAX) NOT NULL,
					workflow_id NVARCHAR(64) NOT NULL,
					acquired_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					expires_at DATETIMEOFFSET NOT NULL)`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_promises')
				CREATE TABLE workflow_promises (
					workflow_id NVARCHAR(64) NOT NULL, promise_id NVARCHAR(64) NOT NULL,
					promise_name NVARCHAR(MAX) NOT NULL,
					status NVARCHAR(MAX) NOT NULL DEFAULT 'pending',
					result NVARCHAR(MAX), error_msg NVARCHAR(MAX),
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					resolved_at DATETIMEOFFSET,
					PRIMARY KEY (workflow_id, promise_id))`,
			// workflow_instances indexes — MSSQL supports filtered indexes natively
			`IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_ready' AND object_id = OBJECT_ID('workflow_instances'))
				CREATE INDEX idx_instances_ready ON workflow_instances(status, next_wake_at) WHERE status = 'ready'`,
			`IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_heartbeat' AND object_id = OBJECT_ID('workflow_instances'))
				CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running'`,
			`IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_stale' AND object_id = OBJECT_ID('workflow_instances'))
				CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running'`,
			`IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_sticky' AND object_id = OBJECT_ID('workflow_instances'))
				CREATE INDEX idx_instances_sticky ON workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL`,
			// concurrency_keys index
			`IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_concurrency_keys_workflow' AND object_id = OBJECT_ID('concurrency_keys'))
				CREATE INDEX idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`,
			// ADD COLUMN for error_code/error_op (idempotent via sys.columns check)
			`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('workflow_instances') AND name = 'error_code')
				ALTER TABLE workflow_instances ADD error_code NVARCHAR(MAX) NULL`,
			`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('workflow_instances') AND name = 'error_op')
				ALTER TABLE workflow_instances ADD error_op NVARCHAR(MAX) NULL`,
			// Memory statistics tables
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_memory_samples')
				CREATE TABLE workflow_memory_samples (
					id BIGINT IDENTITY(1,1) PRIMARY KEY,
					def_name NVARCHAR(MAX) NOT NULL,
					sample_bytes BIGINT NOT NULL,
					recorded_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME())`,
			`IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_mem_samples_def' AND object_id = OBJECT_ID('workflow_memory_samples'))
				CREATE INDEX idx_mem_samples_def ON workflow_memory_samples(def_name, recorded_at DESC)`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_memory_stats')
				CREATE TABLE workflow_memory_stats (
					def_name NVARCHAR(255) NOT NULL PRIMARY KEY,
					mean_bytes FLOAT(53) NOT NULL DEFAULT 0,
					sample_count INTEGER NOT NULL DEFAULT 0,
					alpha FLOAT(53) NOT NULL DEFAULT 0.3,
					updated_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME())`,
		}
	default:
		t.Fatalf("setup full schema: unknown dialect: %s", dialect)
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup schema: %v", err)
		}
	}
}

// CleanupTestData deletes test data matching the given runID pattern
// (e.g., "test-%") from event_history, workflow_signals, workflow_promises,
// concurrency_keys, and workflow_instances.
func CleanupTestData(t *testing.T, db *sql.DB, dialect Dialect, runID string) {
	t.Helper()
	var p string
	switch dialect {
	case DialectPostgres:
		p = "$1"
	case DialectMySQL:
		p = "?"
	case DialectMSSQL:
		p = "@p1"
	default:
		t.Fatalf("cleanup test data: unknown dialect: %s", dialect)
	}
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE `+p, runID)
	db.Exec(`DELETE FROM workflow_signals WHERE workflow_id LIKE `+p, runID)
	db.Exec(`DELETE FROM workflow_promises WHERE workflow_id LIKE `+p, runID)
	db.Exec(`DELETE FROM concurrency_keys WHERE workflow_id LIKE `+p, runID)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE `+p, runID)
}

// TestDB opens a database connection for the given dialect using environment
// variables:
//
//	DialectPostgres — CLEAT_TEST_POSTGRES (fallback CLEAT_TEST_DB, then
//	                  postgres://localhost:5432/cleat?sslmode=disable)
//	DialectMySQL    — CLEAT_TEST_MYSQL (skipped if not set)
//	DialectMSSQL    — CLEAT_TEST_MSSQL (skipped if not set)
//
// It creates the minimal schema and returns the connection. The test is
// skipped in short mode or if no database is available.
func TestDB(t *testing.T, dialect Dialect) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping database test in short mode")
	}

	var dsn string
	var driverName string
	switch dialect {
	case DialectPostgres:
		dsn = os.Getenv("CLEAT_TEST_POSTGRES")
		if dsn == "" {
			dsn = os.Getenv("CLEAT_TEST_DB")
		}
		if dsn == "" {
			dsn = "postgres://localhost:5432/cleat?sslmode=disable"
		}
		driverName = "postgres"
	case DialectMySQL:
		dsn = os.Getenv("CLEAT_TEST_MYSQL")
		if dsn == "" {
			t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
		}
		driverName = "mysql"
	case DialectMSSQL:
		dsn = os.Getenv("CLEAT_TEST_MSSQL")
		if dsn == "" {
			t.Skip("CLEAT_TEST_MSSQL not set, skipping MSSQL tests")
		}
		driverName = "sqlserver"
	default:
		t.Fatalf("TestDB: unknown dialect: %s", dialect)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Skipf("Skipping test: no database available: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping test: cannot ping database: %v", err)
	}
	SetupMinimalSchema(t, db, dialect)
	return db
}
