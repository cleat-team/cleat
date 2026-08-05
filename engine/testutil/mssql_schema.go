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
		// workflow_defs.
		//
		// tenant_id is NOT NULL DEFAULT '000…', as
		// migrations/mssql/001_schema.sql declares it. This said a bare
		// "tenant_id UNIQUEIDENTIFIER", so a definition deployed against the
		// test schema came back with a NULL owner where the shipped schema
		// gives the default tenant -- the drift class of IMPROVEMENT-PLAN 1.9,
		// found by 3.12's ownership test reading the column back.
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
             tenant_id UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
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
             CONSTRAINT ck_workflow_instances_query_state CHECK (ISJSON(query_state) = 1),
             -- NOT NULL DEFAULT, as migrations/mssql/001_schema.sql declares
             -- it. Left nullable here, a fixture that inserted a row without
             -- naming tenant_id got NULL where a real database gives the
             -- default tenant -- so every tenant-scoped predicate missed it.
             -- IMPROVEMENT-PLAN 3.11; the same drift class as 1.9.
             tenant_id UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
             priority INTEGER NOT NULL DEFAULT 0,
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
             -- Write-ahead call intent (1.4 phase D). Must match
             -- migrations/mssql/020_event_intent.sql.
             intent_at DATETIMEOFFSET NULL,
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
             tenant_id UNIQUEIDENTIFIER,
             CONSTRAINT ck_workflow_schedules_input CHECK (ISJSON(input) = 1)
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
             priority INTEGER NOT NULL DEFAULT 0,
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
             priority INTEGER NOT NULL DEFAULT 0,
             payload NVARCHAR(MAX) NOT NULL DEFAULT '{}',
             promise_id NVARCHAR(MAX),
             status NVARCHAR(MAX) NOT NULL DEFAULT 'pending',
             result NVARCHAR(MAX),
             error_msg NVARCHAR(MAX),
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             completed_at DATETIMEOFFSET,
             tenant_id UNIQUEIDENTIFIER NULL,
             PRIMARY KEY (workflow_id, update_name)
         )`,

		// idempotency_keys. The primary key is (key_hash, tenant_id), not
		// key_hash alone: an Idempotency-Key is a client-supplied header, so
		// two tenants picking "order-123" must not collide.
		// migrations/mssql/010_idempotency_keys_tenant_id.sql,
		// IMPROVEMENT-PLAN 3.10.
		`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'idempotency_keys')
         CREATE TABLE idempotency_keys (
             key_hash VARBINARY(32) NOT NULL,
             workflow_id NVARCHAR(MAX) NOT NULL,
             result NVARCHAR(MAX),
             error_msg NVARCHAR(MAX),
             created_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
             expires_at DATETIMEOFFSET NOT NULL DEFAULT DATEADD(DAY, 7, SYSUTCDATETIME()),
             tenant_id UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
             CONSTRAINT pk_idempotency_keys PRIMARY KEY (key_hash, tenant_id)
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

	// tenant_api_keys (needed by ResolveTenantFromAPIKey)
	if _, err := db.Exec(`IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'tenant_api_keys' AND schema_name(schema_id) = 'dbo')
	         CREATE TABLE tenant_api_keys (
	             key_hash VARBINARY(32) NOT NULL PRIMARY KEY,
	             tenant_id UNIQUEIDENTIFIER NOT NULL,
	             description NVARCHAR(255),
	             created_at DATETIME2 DEFAULT SYSUTCDATETIME(),
	             revoked_at DATETIME2 NULL
	         )`); err != nil {
		t.Logf("setup MSSQL full schema: tenant_api_keys warning: %v", err)
	}
	// Migration: add columns that may be missing from older test databases.
	migrations := []string{
		`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE name = 'event_count' AND object_id = OBJECT_ID('workflow_instances'))
		 ALTER TABLE workflow_instances ADD event_count BIGINT NOT NULL DEFAULT 0`,
		`IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE name = 'allowed_signals' AND object_id = OBJECT_ID('workflow_instances'))
		 ALTER TABLE workflow_instances ADD allowed_signals NVARCHAR(MAX) NULL`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			t.Logf("setup MSSQL full schema: migration warning: %v", err)
		}
	}

	migrateMSSQLIdempotencyTenantID(t, db)
	migrateMSSQLWorkflowDefsTenantID(t, db)
	migrateMSSQLScheduleInputConstraint(t, db)
	migrateMSSQLQueryStateConstraint(t, db)
	migrateMSSQLEventIntentAt(t, db)
	migrateMSSQLInstancesTenantID(t, db)

	// Indexes. These used to live in SetupFullSchema, so the schema you got
	// depended on which entry point the test called. Both now route here.
	// IMPROVEMENT-PLAN 2.60b.
	//
	// Best-effort: several columns are NVARCHAR(MAX) in this schema and cannot
	// be index keys, which execMSSQLBestEffort tolerates deliberately.
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

// migrateMSSQLInstancesTenantID brings an already-existing test database up to
// the `tenant_id UNIQUEIDENTIFIER NOT NULL DEFAULT '000…'` that
// migrations/mssql/001_schema.sql declares for workflow_instances.
//
// Only workflow_instances is corrected here, because that is the table
// IMPROVEMENT-PLAN 3.11's tenant predicates read. Five other tables in this
// file still declare tenant_id nullable where the shipped schema does not
// (event_history, workflow_signals, workflow_schedules, concurrency_keys,
// workflow_defs' neighbours); aligning them belongs with the 2.71 residual,
// which is about this same file disagreeing with what ships.
func migrateMSSQLInstancesTenantID(t *testing.T, db *sql.DB) {
	t.Helper()

	if _, err := db.Exec(`UPDATE dbo.workflow_instances
		SET tenant_id = '00000000-0000-0000-0000-000000000000'
		WHERE tenant_id IS NULL`); err != nil {
		t.Fatalf("setup MSSQL full schema: backfill workflow_instances.tenant_id: %v", err)
	}
	if _, err := db.Exec(`
		IF EXISTS (
		    SELECT 1 FROM sys.columns
		    WHERE object_id = OBJECT_ID(N'dbo.workflow_instances')
		      AND name = N'tenant_id' AND is_nullable = 1
		)
		    ALTER TABLE dbo.workflow_instances ALTER COLUMN tenant_id UNIQUEIDENTIFIER NOT NULL`); err != nil {
		t.Fatalf("setup MSSQL full schema: workflow_instances.tenant_id NOT NULL: %v", err)
	}
	if _, err := db.Exec(`
		IF NOT EXISTS (
		    SELECT 1 FROM sys.default_constraints
		    WHERE parent_object_id = OBJECT_ID(N'dbo.workflow_instances')
		      AND parent_column_id = (
		          SELECT column_id FROM sys.columns
		          WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'tenant_id'
		      )
		)
		    ALTER TABLE dbo.workflow_instances
		        ADD CONSTRAINT df_workflow_instances_tenant_id
		        DEFAULT '00000000-0000-0000-0000-000000000000' FOR tenant_id`); err != nil {
		t.Fatalf("setup MSSQL full schema: workflow_instances.tenant_id default: %v", err)
	}
}

// migrateMSSQLEventIntentAt adds event_history.intent_at to an already-existing
// test database, for the reason in migrateMySQLEventIntentAt: the CREATE TABLE
// is guarded on sys.tables, so a table built before 1.4 phase D never gains the
// column and every LoadEventHistory fails on it.
func migrateMSSQLEventIntentAt(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		IF NOT EXISTS (
		    SELECT 1 FROM sys.columns
		    WHERE object_id = OBJECT_ID(N'dbo.event_history')
		      AND name = N'intent_at'
		)
		    ALTER TABLE dbo.event_history ADD intent_at DATETIMEOFFSET NULL`); err != nil {
		t.Fatalf("setup MSSQL full schema: add event_history.intent_at: %v", err)
	}
}

// migrateMSSQLIdempotencyTenantID brings an already-existing test database up
// to the (key_hash, tenant_id) primary key that
// migrations/mssql/010_idempotency_keys_tenant_id.sql introduces.
//
// The CREATE TABLE above cannot do this on its own: it is guarded on
// sys.tables against a shared, long-lived test database, so a table created by
// an earlier checkout keeps its old shape forever and the new column simply
// never appears. That is the failure mode IMPROVEMENT-PLAN 2.60b describes.
//
// key_hash is narrowed to VARBINARY(32) on the way past, which is what
// migrations/mssql/001_schema.sql has always declared. This file used to say
// VARBINARY(900) -- harmless while the key was the whole primary key, but a
// composite key of 900 + 16 bytes exceeds SQL Server's 900-byte index key
// limit and the constraint is rejected outright. The values stored are
// SHA-256 digests, so 32 is not a truncation.
//
// Errors fail the test rather than being logged: with the column missing,
// StartNewRun's SQL names a column that is not there, and a warning here
// would turn that into a confusing failure elsewhere.
func migrateMSSQLIdempotencyTenantID(t *testing.T, db *sql.DB) {
	t.Helper()

	// Drop a pre-010 single-column primary key by its catalogue name: this
	// file used to create the table with an inline PRIMARY KEY, which SQL
	// Server names for itself, so the constraint is not reliably called
	// pk_idempotency_keys.
	if _, err := db.Exec(`
		DECLARE @pk_name SYSNAME = (
		    SELECT kc.name
		    FROM sys.key_constraints kc
		    WHERE kc.parent_object_id = OBJECT_ID(N'dbo.idempotency_keys')
		      AND kc.type = 'PK'
		      AND (
		          SELECT COUNT(*)
		          FROM sys.index_columns ic
		          WHERE ic.object_id = kc.parent_object_id
		            AND ic.index_id = kc.unique_index_id
		      ) = 1
		);
		IF @pk_name IS NOT NULL
		    EXEC('ALTER TABLE dbo.idempotency_keys DROP CONSTRAINT [' + @pk_name + ']')`); err != nil {
		t.Fatalf("setup MSSQL full schema: drop idempotency_keys primary key: %v", err)
	}

	if _, err := db.Exec(`
		IF EXISTS (
		    SELECT 1 FROM sys.columns
		    WHERE object_id = OBJECT_ID(N'dbo.idempotency_keys')
		      AND name = N'key_hash' AND max_length <> 32
		)
		    ALTER TABLE dbo.idempotency_keys ALTER COLUMN key_hash VARBINARY(32) NOT NULL`); err != nil {
		t.Fatalf("setup MSSQL full schema: narrow idempotency_keys.key_hash: %v", err)
	}

	if _, err := db.Exec(`
		IF NOT EXISTS (
		    SELECT 1 FROM sys.columns
		    WHERE object_id = OBJECT_ID(N'dbo.idempotency_keys')
		      AND name = N'tenant_id'
		)
		    ALTER TABLE dbo.idempotency_keys
		        ADD tenant_id UNIQUEIDENTIFIER NOT NULL
		        CONSTRAINT df_idempotency_keys_tenant_id
		        DEFAULT '00000000-0000-0000-0000-000000000000'`); err != nil {
		t.Fatalf("setup MSSQL full schema: add idempotency_keys.tenant_id: %v", err)
	}

	if _, err := db.Exec(`
		IF NOT EXISTS (
		    SELECT 1 FROM sys.key_constraints
		    WHERE parent_object_id = OBJECT_ID(N'dbo.idempotency_keys')
		      AND type = 'PK'
		)
		    ALTER TABLE dbo.idempotency_keys
		        ADD CONSTRAINT pk_idempotency_keys PRIMARY KEY (key_hash, tenant_id)`); err != nil {
		t.Fatalf("setup MSSQL full schema: widen idempotency_keys primary key: %v", err)
	}
}

// migrateMSSQLQueryStateConstraint adds the CHECK constraint that
// migrations/mssql/001_schema.sql has always declared on
// workflow_instances.query_state.
//
// Its absence hid IMPROVEMENT-PLAN 3.17: every dialect wrote the JSON value
// `null` there for a workflow with no query handlers, because json.Marshal of
// a nil map returns `null` rather than nil and the guard meant to substitute
// `{}` tested for nil. PostgreSQL and MySQL accept a JSON null; SQL Server's
// shipped schema does not, so CompleteWorkflow, FailWorkflow and ContinueAsNew
// all failed there -- and this schema, having no constraint, showed nothing.
//
// Existing rows are repaired first: ALTER TABLE ADD CONSTRAINT validates them
// and would fail on the `null`s a long-lived test database already holds.
func migrateMSSQLQueryStateConstraint(t *testing.T, db *sql.DB) {
	t.Helper()

	if _, err := db.Exec(`UPDATE dbo.workflow_instances
		SET query_state = '{}'
		WHERE query_state IS NULL OR ISJSON(query_state) <> 1`); err != nil {
		t.Fatalf("setup MSSQL full schema: repair workflow_instances.query_state: %v", err)
	}
	if _, err := db.Exec(`
		IF NOT EXISTS (
		    SELECT 1 FROM sys.check_constraints
		    WHERE name = N'ck_workflow_instances_query_state'
		      AND parent_object_id = OBJECT_ID(N'dbo.workflow_instances')
		)
		    ALTER TABLE dbo.workflow_instances
		        ADD CONSTRAINT ck_workflow_instances_query_state CHECK (ISJSON(query_state) = 1)`); err != nil {
		t.Fatalf("setup MSSQL full schema: add workflow_instances query_state constraint: %v", err)
	}
}

// migrateMSSQLScheduleInputConstraint adds the CHECK constraint that
// migrations/mssql/001_schema.sql has always declared on
// workflow_schedules.input.
//
// Without it, this schema accepted a value the shipped one rejects, and that
// gap hid a defect that broke every scheduled workflow on SQL Server:
// json.RawMessage binds as VARBINARY, so the column received the binary
// rendering of the JSON and `ISJSON(input) = 1` refused the row.
// IMPROVEMENT-PLAN 3.16.
//
// Rows already in a long-lived test database may hold that malformed value, so
// they are repaired before the constraint goes on -- ALTER TABLE ADD
// CONSTRAINT validates existing rows and would fail on them.
func migrateMSSQLScheduleInputConstraint(t *testing.T, db *sql.DB) {
	t.Helper()

	if _, err := db.Exec(`UPDATE dbo.workflow_schedules
		SET input = '{}'
		WHERE ISJSON(input) <> 1`); err != nil {
		t.Fatalf("setup MSSQL full schema: repair workflow_schedules.input: %v", err)
	}
	if _, err := db.Exec(`
		IF NOT EXISTS (
		    SELECT 1 FROM sys.check_constraints
		    WHERE name = N'ck_workflow_schedules_input'
		      AND parent_object_id = OBJECT_ID(N'dbo.workflow_schedules')
		)
		    ALTER TABLE dbo.workflow_schedules
		        ADD CONSTRAINT ck_workflow_schedules_input CHECK (ISJSON(input) = 1)`); err != nil {
		t.Fatalf("setup MSSQL full schema: add workflow_schedules input constraint: %v", err)
	}
}

// migrateMSSQLWorkflowDefsTenantID brings an already-existing test database up
// to the `tenant_id UNIQUEIDENTIFIER NOT NULL DEFAULT '000…'` that
// migrations/mssql/001_schema.sql has always declared, and that the CREATE
// TABLE above now matches.
//
// Same reason as migrateMSSQLIdempotencyTenantID: the CREATE is guarded on
// sys.tables against a shared, long-lived database, so a table built by an
// earlier checkout keeps its nullable column forever. Existing NULLs are
// backfilled to the default tenant, which is what the shipped schema would
// have written for those rows.
func migrateMSSQLWorkflowDefsTenantID(t *testing.T, db *sql.DB) {
	t.Helper()

	if _, err := db.Exec(`UPDATE dbo.workflow_defs
		SET tenant_id = '00000000-0000-0000-0000-000000000000'
		WHERE tenant_id IS NULL`); err != nil {
		t.Fatalf("setup MSSQL full schema: backfill workflow_defs.tenant_id: %v", err)
	}

	if _, err := db.Exec(`
		IF EXISTS (
		    SELECT 1 FROM sys.columns
		    WHERE object_id = OBJECT_ID(N'dbo.workflow_defs')
		      AND name = N'tenant_id' AND is_nullable = 1
		)
		    ALTER TABLE dbo.workflow_defs ALTER COLUMN tenant_id UNIQUEIDENTIFIER NOT NULL`); err != nil {
		t.Fatalf("setup MSSQL full schema: workflow_defs.tenant_id NOT NULL: %v", err)
	}

	if _, err := db.Exec(`
		IF NOT EXISTS (
		    SELECT 1 FROM sys.default_constraints
		    WHERE parent_object_id = OBJECT_ID(N'dbo.workflow_defs')
		      AND parent_column_id = (
		          SELECT column_id FROM sys.columns
		          WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'tenant_id'
		      )
		)
		    ALTER TABLE dbo.workflow_defs
		        ADD CONSTRAINT df_workflow_defs_tenant_id
		        DEFAULT '00000000-0000-0000-0000-000000000000' FOR tenant_id`); err != nil {
		t.Fatalf("setup MSSQL full schema: workflow_defs.tenant_id default: %v", err)
	}
}

// CleanupMSSQLTestData removes all test data from the MSSQL tables.
// Uses DELETE with table existence checks. Order respects FK constraints.
func CleanupMSSQLTestData(t *testing.T, db *sql.DB) {
	t.Helper()

	// Order matters due to FK constraints — delete child tables first.
	tables := []string{
		"tenant_api_keys",
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
