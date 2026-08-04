package testutil

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// schemaApplyLockKey is the advisory-lock key that serialises concurrent
// applications of 001_schema.sql against one database. Any fixed int64 works
// as long as every caller uses the same one; this is "cleatddl" in ASCII,
// chosen to be recognisable in pg_locks when diagnosing a stuck test.
const schemaApplyLockKey int64 = 0x636c65617464646c

// execIgnoreDupKey executes a SQL statement, ignoring MySQL error 1061
// (Duplicate key name) and 1060 (Duplicate column name). Other errors are
// passed through. This allows idempotent CREATE INDEX / ALTER TABLE ADD
// COLUMN in MySQL which does not support IF NOT EXISTS for those operations.
func execIgnoreDupKey(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "Error 1061") && !strings.Contains(msg, "Error 1060") {
			t.Fatalf("setup schema: %v", err)
		}
	}
}

// execMSSQLBestEffort executes a SQL statement, ignoring errors that are
// expected in test schemas (e.g., index creation on NVARCHAR(MAX) columns).
func execMSSQLBestEffort(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		msg := err.Error()
		// NVARCHAR(MAX) columns cannot be index keys; this is expected in
		// test schemas that use NVARCHAR(MAX) for flexibility.
		if strings.Contains(msg, "invalid for use as a key column") {
			return
		}
		t.Fatalf("setup schema: %v", err)
	}
}

// postgresSchemaFile locates the real, shipped PostgreSQL schema migration
// (migrations/postgres/001_schema.sql), applied directly by
// SetupMinimalSchema/SetupFullSchema for DialectPostgres instead of a
// hand-maintained duplicate.
//
// This file previously hand-duplicated the schema (a third copy, alongside
// migrations/postgres/001_schema.sql and the root schema.sql) and had
// already drifted from it twice in one session before this fix (the
// `generation` column's nullability, and MySQL collation). Reading the real
// migration from disk -- the same approach store_backends_procedures_test.go
// already uses for 003_procedures.sql/004_*.sql -- makes drift structurally
// impossible for Postgres.
//
// The path is computed from this source file's own location via
// runtime.Caller rather than a hardcoded relative path, because this
// package is exercised from two different `go test` working directories:
// engine/ (which imports testutil) and engine/testutil/ itself
// (testutil_test.go) -- a single ".."-relative path cannot be correct for
// both.
func postgresSchemaFile() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("postgresSchemaFile: runtime.Caller failed")
	}
	// thisFile is .../engine/testutil/schema.go; the repo root is two
	// levels up, and the schema lives at migrations/postgres/001_schema.sql
	// under it.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", "postgres", "001_schema.sql")
}

// applyPostgresSchemaFile reads and executes postgresSchemaFile() against db.
// lib/pq's simple query protocol accepts the whole multi-statement file as a
// single Exec (as applyPostgresProcedures in store_backends_procedures_test.go
// already relies on for 003/004).
//
// The statements are idempotent (CREATE ... IF NOT EXISTS, CREATE OR REPLACE,
// DROP POLICY IF EXISTS ... CREATE POLICY), so this is safe to call more than
// once against the same database **sequentially**. It is NOT safe to call
// concurrently, and an earlier version of this comment claimed otherwise --
// which is why the resulting flake read as mysterious rather than obvious
// (IMPROVEMENT-PLAN §2.21). PostgreSQL's IF NOT EXISTS forms are not atomic:
// two sessions both observe the object missing, both insert the catalog row,
// and one loses on a unique index. Observed as
//
//	pq: duplicate key value violates unique constraint
//	"pg_extension_name_index" (23505)
//
// from CREATE EXTENSION IF NOT EXISTS pgcrypto, though CREATE TABLE IF NOT
// EXISTS carries the same hazard.
//
// Concurrency here is the norm, not the exception: `go test ./plugins/...`
// runs distinct packages in parallel (-p defaults to NumCPU) and they all
// point at the same CLEAT_TEST_POSTGRES database. So the apply is serialised
// with a session-level advisory lock.
//
// The lock is taken on a single pinned *sql.Conn rather than on db. Advisory
// locks belong to a session, and database/sql hands out arbitrary pooled
// connections per call -- so locking via db could take the lock on one
// connection and try to release it on another, which silently fails to
// unlock and leaks the lock for the life of that connection.
//
// Must be called with a connection that owns (or can create) the schema --
// migrations/postgres/001_schema.sql creates the `admin` and `cleat`
// schemas, several admin.* functions, and enables/forces Row-Level Security
// on every tenant-scoped table. For tests that need RLS to actually be
// enforced against their queries (rather than silently bypassed, which
// PostgreSQL does unconditionally for superuser connections and, absent
// FORCE ROW LEVEL SECURITY, for the owning role too) see
// SetupPostgresRLSRole and OpenPostgresRLSTestDB below.
func applyPostgresSchemaFile(t *testing.T, db *sql.DB) {
	t.Helper()
	path := postgresSchemaFile()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("apply %s: acquire connection: %v", path, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaApplyLockKey); err != nil {
		t.Fatalf("apply %s: acquire advisory lock: %v", path, err)
	}
	defer func() {
		// Release explicitly. conn.Close() only returns the connection to the
		// pool; the session lives on and would keep holding the lock.
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, schemaApplyLockKey); err != nil {
			t.Errorf("apply %s: release advisory lock: %v", path, err)
		}
	}()

	// Skip the DDL entirely when this exact schema file has already been
	// applied to this database.
	//
	// The advisory lock above serialises schema application against schema
	// application, which is not the collision that actually bites. This
	// function used to run on *every* TestDB call -- 24 times for
	// tests/integrity alone -- and every run takes ACCESS EXCLUSIVE on tables
	// another package's tests are reading and writing at that moment. Go runs
	// distinct packages in parallel against the same CLEAT_TEST_DB, so
	// `go test ./tests/integrity/... ./tests/upgrade/... ./tests/scale/...`
	// deadlocked DDL against DML:
	//
	//	apply migrations/postgres/001_schema.sql: pq: deadlock detected (40P01)
	//	append events in tx: increment event_count: pq: deadlock detected (40P01)
	//
	// 17 failures, every one of which passes when the suites are run one at a
	// time -- a screen of red that means nothing, which is its own kind of
	// false signal. Fingerprinting makes the DDL run once for a given schema
	// file instead of once per test. IMPROVEMENT-PLAN 2.39.
	//
	// Tests that add their own columns (all IF NOT EXISTS) or drop objects
	// they themselves created are unaffected: the fingerprint tracks the
	// schema *file*, and re-applying it is exactly what those tests do not
	// need.
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(data))
	var applied string
	err = conn.QueryRowContext(ctx, `SELECT fingerprint FROM cleat_test_schema WHERE id = 1`).Scan(&applied)
	if err == nil && applied == fingerprint {
		return
	}

	// No-args Exec keeps lib/pq on the simple query protocol, which is what
	// allows the whole multi-statement file to go in one round trip.
	if _, err := conn.ExecContext(ctx, string(data)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cleat_test_schema (
		id INTEGER PRIMARY KEY, fingerprint TEXT NOT NULL)`); err != nil {
		t.Fatalf("apply %s: create fingerprint table: %v", path, err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO cleat_test_schema (id, fingerprint) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET fingerprint = EXCLUDED.fingerprint`, fingerprint); err != nil {
		t.Fatalf("apply %s: record fingerprint: %v", path, err)
	}
}

// SetupMinimalSchema creates the minimal tables needed by all DB tests:
// workflow_defs, workflow_instances, event_history, workflow_signals.
// Uses CREATE TABLE IF NOT EXISTS (or equivalent) so it is idempotent.
//
// For DialectPostgres this actually applies the full real schema (see
// applyPostgresSchemaFile) since that file is cheap, idempotent, and not
// worth hand-splitting into a separate "minimal" subset.
func SetupMinimalSchema(t *testing.T, db *sql.DB, dialect Dialect) {
	t.Helper()

	if dialect == DialectPostgres {
		applyPostgresSchemaFile(t, db)
		return
	}

	var stmts []string
	switch dialect {
	case DialectMySQL:
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS workflow_defs (
				name VARCHAR(255) NOT NULL, version INTEGER NOT NULL,
				wasm_bytes LONGBLOB NOT NULL, entry_points JSON NOT NULL DEFAULT ('[]'),
				min_version INTEGER NOT NULL DEFAULT 0,
				max_history_length INTEGER NOT NULL DEFAULT 0,
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				abi_version INTEGER NOT NULL DEFAULT 1,
				plugin_deps JSON NOT NULL DEFAULT ('{}'),
				deprecated TINYINT(1) NOT NULL DEFAULT 0,
				tenant_id VARCHAR(36),
				task_queue VARCHAR(255) NOT NULL DEFAULT 'default',
				PRIMARY KEY (name, version))`,
			`CREATE TABLE IF NOT EXISTS workflow_instances (
				id VARCHAR(255) PRIMARY KEY, def_name VARCHAR(255) NOT NULL,
				def_version INTEGER NOT NULL DEFAULT 1,
				status VARCHAR(255) NOT NULL DEFAULT 'ready', input JSON NOT NULL DEFAULT ('{}'),
				assigned_to VARCHAR(255), heartbeat_at TIMESTAMP(6),
				next_wake_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6), completed_at TIMESTAMP(6),
				result JSON, error_msg TEXT, error_code VARCHAR(255), error_op VARCHAR(255),
				parent_workflow_id TEXT,
				parent_close_policy VARCHAR(255) DEFAULT 'ABANDON',
				trace_id TEXT, query_state JSON DEFAULT ('{}'),
				task_queue VARCHAR(255) NOT NULL DEFAULT 'default',
				cancellation_requested TINYINT(1) NOT NULL DEFAULT 0,
				cancellation_reason TEXT, sticky_worker_id TEXT,
				tenant_id VARCHAR(36),
				compaction_state JSON, compacted_at TIMESTAMP(6), compaction_step INTEGER,
				plugin_vers JSON NOT NULL DEFAULT ('{}'),
				event_count BIGINT NOT NULL DEFAULT 0,
				priority INTEGER NOT NULL DEFAULT 0,
				allowed_signals JSON DEFAULT NULL,
					generation BIGINT NOT NULL DEFAULT 0)`,
			`CREATE TABLE IF NOT EXISTS event_history (
				workflow_id VARCHAR(255) NOT NULL, step INTEGER NOT NULL,
				event_type VARCHAR(255) NOT NULL DEFAULT 'call',
				service TEXT, operation TEXT, request TEXT, response TEXT, error TEXT,
				duration_ms BIGINT, signal_names TEXT, timeout_ms BIGINT,
				signal_name TEXT, signal_payload TEXT, defer_description TEXT,
				defer_id TEXT, child_name TEXT, child_input TEXT, run_id TEXT,
				new_input TEXT, plugin_name TEXT, plugin_func TEXT,
				plugin_input TEXT, plugin_output TEXT, plugin_error TEXT,
				promise_name TEXT, promise_id TEXT, promise_result TEXT, promise_error TEXT,
				tenant_id VARCHAR(36),
				payload JSON,
				checksum TEXT,
				thread_id VARCHAR(255) NOT NULL DEFAULT 'main',
				local_step INTEGER NOT NULL DEFAULT 0,
				global_seq BIGINT NOT NULL DEFAULT 0,
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				PRIMARY KEY (workflow_id, step),
				FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE)`,
			`CREATE TABLE IF NOT EXISTS workflow_signals (
				workflow_id VARCHAR(255) NOT NULL, signal_name VARCHAR(255) NOT NULL,
				payload JSON NOT NULL DEFAULT ('{}'),
				delivered_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				tenant_id VARCHAR(36),
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
					abi_version INTEGER NOT NULL DEFAULT 1,
					plugin_deps NVARCHAR(MAX) NOT NULL DEFAULT '{}',
					deprecated BIT NOT NULL DEFAULT 0,
					tenant_id UNIQUEIDENTIFIER,
					task_queue NVARCHAR(MAX) NOT NULL DEFAULT 'default',
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
					parent_close_policy NVARCHAR(MAX) DEFAULT 'ABANDON',
					trace_id NVARCHAR(MAX),
					query_state NVARCHAR(MAX) DEFAULT ('{}'),
					task_queue NVARCHAR(MAX) NOT NULL DEFAULT 'default',
					cancellation_requested BIT NOT NULL DEFAULT 0,
					cancellation_reason NVARCHAR(MAX),
					sticky_worker_id NVARCHAR(MAX),
					tenant_id UNIQUEIDENTIFIER,
					compaction_state NVARCHAR(MAX), compacted_at DATETIMEOFFSET, compaction_step INTEGER,
					plugin_vers NVARCHAR(MAX) NOT NULL DEFAULT '{}',
					event_count BIGINT NOT NULL DEFAULT 0,
					priority INTEGER NOT NULL DEFAULT 0,
					allowed_signals NVARCHAR(MAX) NULL,
					generation BIGINT NOT NULL DEFAULT 0)`,
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
					tenant_id UNIQUEIDENTIFIER,
					payload NVARCHAR(MAX),
					checksum NVARCHAR(MAX),
					thread_id NVARCHAR(MAX) NOT NULL DEFAULT 'main',
					local_step INTEGER NOT NULL DEFAULT 0,
					global_seq BIGINT NOT NULL DEFAULT 0,
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (workflow_id, step),
					CONSTRAINT fk_event_history_workflow FOREIGN KEY (workflow_id)
						REFERENCES workflow_instances(id) ON DELETE CASCADE)`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_signals')
				CREATE TABLE workflow_signals (
					workflow_id NVARCHAR(64) NOT NULL,
					signal_name NVARCHAR(255) NOT NULL,
					payload NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					delivered_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					tenant_id UNIQUEIDENTIFIER,
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
// (workflow_schedules, concurrency_keys, workflow_promises, idempotency_keys,
// workflow_update_requests), all indexes, the pgcrypto extension (PostgreSQL only),
// and ALTER TABLE ADD COLUMN IF NOT EXISTS (PostgreSQL only).
//
// For DialectPostgres, SetupMinimalSchema above already applied the full,
// real migrations/postgres/001_schema.sql (which is the final, consolidated
// schema -- all tables, all indexes, RLS included), so there is nothing left
// to add here.
func SetupFullSchema(t *testing.T, db *sql.DB, dialect Dialect) {
	t.Helper()
	SetupMinimalSchema(t, db, dialect)

	if dialect == DialectPostgres {
		return
	}

	var stmts []string
	switch dialect {
	case DialectMySQL:
		stmts = []string{
			// Additional tables
			`CREATE TABLE IF NOT EXISTS workflow_schedules (
				name VARCHAR(255) PRIMARY KEY, def_name VARCHAR(255) NOT NULL,
				entry_point VARCHAR(255) NOT NULL DEFAULT '', cron_expression TEXT NOT NULL,
				input JSON NOT NULL DEFAULT ('{}'), enabled TINYINT(1) NOT NULL DEFAULT 1,
				next_run_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6), last_run_at TIMESTAMP(6),
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				tenant_id VARCHAR(36))`,
			`CREATE TABLE IF NOT EXISTS concurrency_keys (
				key_hash VARBINARY(255) PRIMARY KEY, key_text TEXT NOT NULL,
				workflow_id TEXT NOT NULL,
				acquired_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				expires_at TIMESTAMP(6) NOT NULL,
				tenant_id VARCHAR(255))`,
			`CREATE TABLE IF NOT EXISTS workflow_promises (
				workflow_id VARCHAR(255) NOT NULL, promise_id VARCHAR(255) NOT NULL,
				promise_name TEXT NOT NULL, priority INTEGER NOT NULL DEFAULT 0,
				status VARCHAR(255) NOT NULL DEFAULT 'pending',
				result JSON, error_msg TEXT,
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				resolved_at TIMESTAMP(6),
				tenant_id VARCHAR(255) NOT NULL,
				PRIMARY KEY (workflow_id, promise_id))`,
			`CREATE TABLE IF NOT EXISTS idempotency_keys (
				key_hash VARBINARY(32) NOT NULL PRIMARY KEY,
				workflow_id VARCHAR(255) NOT NULL,
				result JSON,
				error_msg TEXT,
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				expires_at TIMESTAMP(6) NOT NULL) /* TTL is application-configured (default 720h) */`,
			`CREATE TABLE IF NOT EXISTS workflow_update_requests (
				workflow_id VARCHAR(255) NOT NULL, update_name VARCHAR(255) NOT NULL,
				priority INTEGER NOT NULL DEFAULT 0,
				payload JSON NOT NULL DEFAULT ('{}'),
				promise_id VARCHAR(255),
				status VARCHAR(255) NOT NULL DEFAULT 'pending',
				result JSON,
				error_msg TEXT,
				tenant_id VARCHAR(255) NOT NULL,
				created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
				completed_at TIMESTAMP(6),
				PRIMARY KEY (workflow_id, update_name))`,
			// Memory statistics tables
			`CREATE TABLE IF NOT EXISTS workflow_memory_samples (
				id BIGINT AUTO_INCREMENT PRIMARY KEY,
				def_name VARCHAR(255) NOT NULL,
				sample_bytes BIGINT NOT NULL,
				recorded_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6))`,
			`CREATE TABLE IF NOT EXISTS workflow_memory_stats (
				def_name VARCHAR(255) PRIMARY KEY,
				mean_bytes DOUBLE NOT NULL DEFAULT 0,
				sample_count INTEGER NOT NULL DEFAULT 0,
				alpha DOUBLE NOT NULL DEFAULT 0.3,
				updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6))`,
		}
	case DialectMSSQL:
		stmts = []string{
			// Additional tables
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_schedules')
				CREATE TABLE workflow_schedules (
					name NVARCHAR(255) PRIMARY KEY, def_name NVARCHAR(MAX) NOT NULL,
					entry_point NVARCHAR(MAX) NOT NULL DEFAULT '',
					cron_expression NVARCHAR(MAX) NOT NULL,
					input NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					enabled BIT NOT NULL DEFAULT 1,
					next_run_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					last_run_at DATETIMEOFFSET,
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					tenant_id UNIQUEIDENTIFIER)`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'concurrency_keys')
				CREATE TABLE concurrency_keys (
					key_hash VARBINARY(32) NOT NULL PRIMARY KEY,
					key_text NVARCHAR(MAX) NOT NULL,
					workflow_id NVARCHAR(64) NOT NULL,
					acquired_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					expires_at DATETIMEOFFSET NOT NULL,
					tenant_id NVARCHAR(128))`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_promises')
				CREATE TABLE workflow_promises (
					workflow_id NVARCHAR(64) NOT NULL, promise_id NVARCHAR(64) NOT NULL,
					promise_name NVARCHAR(MAX) NOT NULL, priority INTEGER NOT NULL DEFAULT 0,
					status NVARCHAR(MAX) NOT NULL DEFAULT 'pending',
					result NVARCHAR(MAX), error_msg NVARCHAR(MAX),
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					resolved_at DATETIMEOFFSET,
					tenant_id UNIQUEIDENTIFIER,
					PRIMARY KEY (workflow_id, promise_id))`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'idempotency_keys')
				CREATE TABLE idempotency_keys (
					key_hash VARBINARY(32) NOT NULL PRIMARY KEY,
					workflow_id NVARCHAR(64) NOT NULL,
					result NVARCHAR(MAX),
					error_msg NVARCHAR(MAX),
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					expires_at DATETIMEOFFSET NOT NULL) -- TTL is application-configured (default 720h)`,
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_update_requests')
				CREATE TABLE workflow_update_requests (
					workflow_id NVARCHAR(64) NOT NULL, update_name NVARCHAR(255) NOT NULL,
					priority INTEGER NOT NULL DEFAULT 0,
					payload NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					promise_id NVARCHAR(64),
					status NVARCHAR(MAX) NOT NULL DEFAULT 'pending',
					result NVARCHAR(MAX),
					error_msg NVARCHAR(MAX),
					tenant_id UNIQUEIDENTIFIER,
					created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					completed_at DATETIMEOFFSET,
					PRIMARY KEY (workflow_id, update_name))`,
			// ADD COLUMN for migrated columns
			`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('workflow_instances') AND name = 'error_code')
				ALTER TABLE workflow_instances ADD error_code NVARCHAR(MAX) NULL`,
			`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('workflow_instances') AND name = 'error_op')
				ALTER TABLE workflow_instances ADD error_op NVARCHAR(MAX) NULL`,
			`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('concurrency_keys') AND name = 'tenant_id')
				ALTER TABLE concurrency_keys ADD tenant_id NVARCHAR(128) NULL`,
			`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('workflow_promises') AND name = 'tenant_id')
				ALTER TABLE workflow_promises ADD tenant_id UNIQUEIDENTIFIER NULL`,
			`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('workflow_update_requests') AND name = 'tenant_id')
				ALTER TABLE workflow_update_requests ADD tenant_id UNIQUEIDENTIFIER NULL`,
			// Memory statistics tables
			`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_memory_samples')
				CREATE TABLE workflow_memory_samples (
					id BIGINT IDENTITY(1,1) PRIMARY KEY,
					def_name NVARCHAR(MAX) NOT NULL,
					sample_bytes BIGINT NOT NULL,
					recorded_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME())`,
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
	// MySQL does not support IF NOT EXISTS for indexes, so create them
	// idempotently by ignoring "Duplicate key name" errors.
	if dialect == DialectMySQL {
		execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_ready ON workflow_instances(status, next_wake_at)`)
		execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at)`)
		execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at)`)
		execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_sticky ON workflow_instances(sticky_worker_id)`)
		_, _ = db.Exec(`DROP INDEX idx_instances_tenant_queue_ready ON workflow_instances`)
		execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_tenant_queue_ready ON workflow_instances(tenant_id, task_queue, status, priority, next_wake_at)`)
		execIgnoreDupKey(t, db, `CREATE INDEX idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`)
		execIgnoreDupKey(t, db, `CREATE INDEX idx_idempotency_keys_expires ON idempotency_keys(expires_at)`)
		execIgnoreDupKey(t, db, `CREATE INDEX idx_mem_samples_def ON workflow_memory_samples(def_name, recorded_at DESC)`)
	}
	// MSSQL test schemas use NVARCHAR(MAX) for status and other columns,
	// which cannot be index keys. These indexes are best-effort.
	if dialect == DialectMSSQL {
		execMSSQLBestEffort(t, db, `IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_ready' AND object_id = OBJECT_ID('workflow_instances'))
			CREATE INDEX idx_instances_ready ON workflow_instances(status, next_wake_at) WHERE status = 'ready'`)
		execMSSQLBestEffort(t, db, `IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_heartbeat' AND object_id = OBJECT_ID('workflow_instances'))
			CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running'`)
		execMSSQLBestEffort(t, db, `IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_stale' AND object_id = OBJECT_ID('workflow_instances'))
			CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running'`)
		execMSSQLBestEffort(t, db, `IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_sticky' AND object_id = OBJECT_ID('workflow_instances'))
			CREATE INDEX idx_instances_sticky ON workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL`)
		_, _ = db.Exec(`DROP INDEX IF EXISTS idx_instances_tenant_queue_ready ON dbo.workflow_instances`)
		execMSSQLBestEffort(t, db, `IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_instances_tenant_queue_ready' AND object_id = OBJECT_ID('workflow_instances'))
			CREATE INDEX idx_instances_tenant_queue_ready ON dbo.workflow_instances(tenant_id, task_queue, status, priority ASC, next_wake_at) WHERE status = 'ready'`)
		execMSSQLBestEffort(t, db, `IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_concurrency_keys_workflow' AND object_id = OBJECT_ID('concurrency_keys'))
			CREATE INDEX idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`)
		execMSSQLBestEffort(t, db, `IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_mem_samples_def' AND object_id = OBJECT_ID('workflow_memory_samples'))
			CREATE INDEX idx_mem_samples_def ON workflow_memory_samples(def_name, recorded_at DESC)`)
	}
}

// CleanupPostgresTestData deletes all rows from the cleat test tables.
// Call before and after tests to ensure isolation from parallel tests.
func CleanupPostgresTestData(t *testing.T, db *sql.DB) {
	t.Helper()
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
	}
	for _, table := range tables {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			t.Logf("cleanup: delete from %s: %v", table, err)
		}
	}
}

// CleanupTestData deletes test data matching the given runID pattern
// (e.g., "test-%") from event_history, workflow_signals, workflow_promises,
// concurrency_keys, idempotency_keys, workflow_update_requests, and workflow_instances.
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
	_, _ = db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM workflow_signals WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM workflow_promises WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM concurrency_keys WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM idempotency_keys WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM workflow_update_requests WHERE workflow_id LIKE `+p, runID)
	_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id LIKE `+p, runID)
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
	// configured records whether a DSN for *this* dialect was supplied, as
	// opposed to falling back to a built-in default. It must be per-dialect:
	// the Multi-DB CI MySQL job sets CLEAT_TEST_MYSQL and has no PostgreSQL at
	// all, so treating any DSN variable as "a database was requested" would
	// fail every PostgreSQL subtest there for the right reason in the wrong
	// job.
	var configured bool
	switch dialect {
	case DialectPostgres:
		dsn = PostgresTestDSN()
		driverName = "postgres"
		configured = os.Getenv("CLEAT_TEST_POSTGRES") != "" || os.Getenv("CLEAT_TEST_DB") != ""
	case DialectMySQL:
		dsn = os.Getenv("CLEAT_TEST_MYSQL")
		if dsn == "" {
			t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
		}
		driverName = "mysql"
		configured = true
	case DialectMSSQL:
		dsn = os.Getenv("CLEAT_TEST_MSSQL")
		if dsn == "" {
			t.Skip("CLEAT_TEST_MSSQL not set, skipping MSSQL tests")
		}
		driverName = "sqlserver"
		configured = true
	default:
		t.Fatalf("TestDB: unknown dialect: %s", dialect)
	}

	// An unreachable database is only a reason to skip when nobody asked for
	// one. If a DSN was configured explicitly, being unable to connect to it
	// is a failure of the configuration, and skipping hides it: the Multi-DB
	// CI workflow set CLEAT_TEST_POSTGRES for a service container it had not
	// published a port for, so every PostgreSQL subtest skipped itself and the
	// job reported green for its whole existence without connecting once. The
	// same treatment is applied in cmd/cleat-worker/auth_test.go.
	unavailable := t.Skipf
	reason := "no %s database at %s (default DSN, none configured): %v"
	if configured {
		unavailable = t.Fatalf
		reason = "configured %s database at %s is unreachable: %v"
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		unavailable(reason, dialect, redactDSN(dsn), err)
		return nil
	}
	if err := db.Ping(); err != nil {
		unavailable(reason, dialect, redactDSN(dsn), err)
		return nil
	}
	SetupMinimalSchema(t, db, dialect)
	return db
}

// redactDSN strips the password from a DSN so it can appear in test output.
func redactDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		u.User = url.User(u.User.Username())
		return u.String()
	}
	// Not a URL (the MySQL driver uses its own format); drop anything that
	// looks like credentials before an @.
	if i := strings.LastIndex(dsn, "@"); i >= 0 {
		return "***@" + dsn[i+1:]
	}
	return dsn
}

// PostgresTestDSN resolves the PostgreSQL test DSN the same way
// TestDB(t, DialectPostgres) does: CLEAT_TEST_POSTGRES, falling back to
// CLEAT_TEST_DB, falling back to a hardcoded localhost DSN. Exported so
// callers that need a second, differently-privileged connection to the same
// test database (see OpenPostgresRLSTestDB) can derive it without
// duplicating the env var precedence.
func PostgresTestDSN() string {
	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}
	if dsn == "" {
		dsn = "postgres://localhost:5432/cleat?sslmode=disable"
	}
	return dsn
}

// PostgresRLSTestRole is a fixed, low-privilege PostgreSQL role used by
// tests that must exercise real Row-Level Security enforcement rather than
// merely configure it.
//
// PostgreSQL unconditionally bypasses RLS for superuser connections, and
// bypasses it for the owning role of a table unless that table has FORCE
// ROW LEVEL SECURITY set (migrations/postgres/001_schema.sql sets FORCE on
// all seven tenant-scoped tables, but that only closes the owner gap, not
// the superuser one). CLEAT_TEST_DB / CLEAT_TEST_POSTGRES conventionally
// point at a superuser role -- e.g. the default "postgres" role, or the
// POSTGRES_USER bootstrap role in the official postgres Docker image, which
// is also a superuser -- that then creates and therefore owns every table
// in SetupMinimalSchema/SetupFullSchema. A connection using that role would
// see RLS as a no-op regardless of how the policies are written, proving
// nothing about tenant isolation. Any role that is neither a superuser nor
// the table owner is always subject to RLS in Postgres (FORCE or not), so
// SetupPostgresRLSRole provisions exactly such a role, and
// OpenPostgresRLSTestDB opens a connection as it.
const PostgresRLSTestRole = "cleat_rls_test_role"

// postgresRLSTestPassword is the fixed login password for
// PostgresRLSTestRole. This role only ever exists inside ephemeral test
// databases (CLEAT_TEST_DB/CLEAT_TEST_POSTGRES), never a real deployment,
// so a hardcoded password is fine.
const postgresRLSTestPassword = "cleat-rls-test-role-password"

// SetupPostgresRLSRole ensures PostgresRLSTestRole exists and can perform
// ordinary DML (SELECT/INSERT/UPDATE/DELETE) against every table in the
// public schema, plus EXECUTE on cleat.assert_tenant_set(), without owning
// any of it. Must be called with a superuser/owner connection (e.g. the one
// TestDB(t, DialectPostgres) returns) after the schema has been applied, so
// that GRANT ... ON ALL TABLES IN SCHEMA public sees every table.
//
// It deliberately does not grant anything beyond DML: the whole point of
// this role is to be an ordinary, non-owning application role whose queries
// are actually subject to RLS.
func SetupPostgresRLSRole(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + PostgresRLSTestRole + `') THEN
				CREATE ROLE ` + PostgresRLSTestRole + ` LOGIN PASSWORD '` + postgresRLSTestPassword + `' NOSUPERUSER NOCREATEDB NOCREATEROLE;
			END IF;
		END $$;`,
		`GRANT USAGE ON SCHEMA public TO ` + PostgresRLSTestRole,
		`GRANT USAGE ON SCHEMA cleat TO ` + PostgresRLSTestRole,
		`GRANT EXECUTE ON FUNCTION cleat.assert_tenant_set() TO ` + PostgresRLSTestRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + PostgresRLSTestRole,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + PostgresRLSTestRole,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup postgres RLS test role: %v\nstatement: %s", err, stmt)
		}
	}
}

// PostgresRLSDSN derives a DSN for PostgresRLSTestRole from a superuser/
// owner DSN (as returned by PostgresTestDSN), preserving host, port,
// database, and query parameters and replacing only the user info.
func PostgresRLSDSN(superuserDSN string) (string, error) {
	u, err := url.Parse(superuserDSN)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	u.User = url.UserPassword(PostgresRLSTestRole, postgresRLSTestPassword)
	return u.String(), nil
}

// OpenPostgresRLSTestDB provisions PostgresRLSTestRole via superuserDB (see
// SetupPostgresRLSRole), then opens and returns a *separate* connection
// authenticated as that role. Tests that need genuine RLS enforcement --
// rather than the superuser/owner bypass that superuserDB itself is subject
// to -- must build their WorkflowStore (or issue their raw SQL) against the
// returned *sql.DB, not against superuserDB. superuserDB should still be
// used for schema setup and any privileged cleanup.
func OpenPostgresRLSTestDB(t *testing.T, superuserDB *sql.DB) *sql.DB {
	t.Helper()
	SetupPostgresRLSRole(t, superuserDB)
	dsn, err := PostgresRLSDSN(PostgresTestDSN())
	if err != nil {
		t.Fatalf("derive RLS test role DSN: %v", err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open RLS test role connection: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping RLS test role connection: %v", err)
	}
	return db
}
