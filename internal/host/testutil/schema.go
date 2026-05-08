package testutil

import (
	"database/sql"
	"os"
	"testing"
)

// SetupMinimalSchema creates the minimal tables needed by all DB tests:
// workflow_defs, workflow_instances, event_history, workflow_signals.
// Uses CREATE TABLE IF NOT EXISTS so it is idempotent.
func SetupMinimalSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS workflow_defs (
			name TEXT NOT NULL, version INTEGER NOT NULL,
			wasm_bytes BYTEA NOT NULL, entry_points TEXT[] NOT NULL DEFAULT '{}',
			min_version INTEGER NOT NULL DEFAULT 0, namespace TEXT NOT NULL DEFAULT 'default',
			max_history_length INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (name, version))`,
		`CREATE TABLE IF NOT EXISTS workflow_instances (
			id TEXT PRIMARY KEY, def_name TEXT NOT NULL, def_version INTEGER NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'ready', input JSONB NOT NULL DEFAULT '{}',
			assigned_to TEXT, heartbeat_at TIMESTAMPTZ,
			next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), completed_at TIMESTAMPTZ,
			result JSONB, error_msg TEXT, parent_workflow_id TEXT,
			namespace TEXT NOT NULL DEFAULT 'default', trace_id TEXT,
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
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
}

// SetupFullSchema calls SetupMinimalSchema and then adds the remaining tables
// (workflow_schedules, concurrency_keys, workflow_promises), all indexes,
// the pgcrypto extension, and ALTER TABLE ADD COLUMN IF NOT EXISTS for
// migrated columns (tenant_id, sticky_worker_id, payload).
func SetupFullSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	SetupMinimalSchema(t, db)

	stmts := []string{
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
		`CREATE INDEX IF NOT EXISTS idx_instances_namespace_ready ON workflow_instances(namespace, status, next_wake_at) WHERE status = 'ready'`,
		// concurrency_keys index
		`CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`,
		// pgcrypto extension (required for digest(), gen_random_uuid())
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		// ALTER TABLE ADD COLUMN IF NOT EXISTS for migrated columns
		`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS tenant_id TEXT`,
		`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS sticky_worker_id TEXT`,
		`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS payload JSONB`,
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
func CleanupTestData(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE $1`, runID)
	db.Exec(`DELETE FROM workflow_signals WHERE workflow_id LIKE $1`, runID)
	db.Exec(`DELETE FROM workflow_promises WHERE workflow_id LIKE $1`, runID)
	db.Exec(`DELETE FROM concurrency_keys WHERE workflow_id LIKE $1`, runID)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE $1`, runID)
}

// TestDB opens a PostgreSQL connection using the CLEAT_TEST_DB environment
// variable (defaulting to postgres://localhost:5432/cleat?sslmode=disable),
// calls SetupMinimalSchema, and returns the connection. The test is skipped
// in short mode or if no database is available.
func TestDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping database test in short mode")
	}
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://localhost:5432/cleat?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("Skipping test: no database available: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping test: cannot ping database: %v", err)
	}
	SetupMinimalSchema(t, db)
	return db
}
