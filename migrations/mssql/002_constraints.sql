-- cleat mssql constraints and indexes (consolidated)
-- All CREATE INDEX statements with final versions compiled from migrations 001-011, 013.
-- Includes cleanup DDL for migration compatibility.
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS guards.

-- ===========================================================================
-- Indexes
-- ===========================================================================

-- Zombie reaper: reclaim instances with stale heartbeats
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_heartbeat' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_heartbeat ON dbo.workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_stale' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_stale ON dbo.workflow_instances(status, heartbeat_at) WHERE status = 'running';

-- Tenant-scoped definition lookups
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_defs_tenant_name_version' AND object_id = OBJECT_ID(N'dbo.workflow_defs'))
    CREATE INDEX idx_defs_tenant_name_version ON dbo.workflow_defs(tenant_id, name, version DESC);

-- Promise status lookups
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_promises_status' AND object_id = OBJECT_ID(N'dbo.workflow_promises'))
    CREATE INDEX idx_promises_status ON dbo.workflow_promises(workflow_id, status);

-- Concurrency key lookups by workflow
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_concurrency_keys_workflow' AND object_id = OBJECT_ID(N'dbo.concurrency_keys'))
    CREATE INDEX idx_concurrency_keys_workflow ON dbo.concurrency_keys(workflow_id);

-- Sticky worker assignments
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_sticky' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_sticky ON dbo.workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL;

-- Pending update requests
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_update_requests_pending' AND object_id = OBJECT_ID(N'dbo.workflow_update_requests'))
    CREATE INDEX idx_update_requests_pending ON dbo.workflow_update_requests(workflow_id, status);

-- Tenant API key lookups (unrevoked only)
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_api_keys_hash' AND object_id = OBJECT_ID(N'dbo.tenant_api_keys'))
    CREATE INDEX idx_api_keys_hash ON dbo.tenant_api_keys(key_hash) WHERE revoked_at IS NULL;

-- Tenant + task-queue-scoped ready instance claims
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_queue_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_tenant_queue_ready
        ON dbo.workflow_instances(tenant_id, task_queue, status, next_wake_at)
        WHERE status = 'ready';

-- Tenant-scoped event history lookups
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_event_history_tenant_wf' AND object_id = OBJECT_ID(N'dbo.event_history'))
    CREATE INDEX idx_event_history_tenant_wf ON dbo.event_history(tenant_id, workflow_id, step);

-- Tenant-scoped signal lookups
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_signals_tenant_wf' AND object_id = OBJECT_ID(N'dbo.workflow_signals'))
    CREATE INDEX idx_signals_tenant_wf ON dbo.workflow_signals(tenant_id, workflow_id, signal_name);

-- Tenant-scoped schedule due lookups
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_schedules_tenant_enabled' AND object_id = OBJECT_ID(N'dbo.workflow_schedules'))
    CREATE INDEX idx_schedules_tenant_enabled ON dbo.workflow_schedules(tenant_id, enabled, next_run_at);

-- Idempotency key lookups by workflow
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_idempotency_workflow_id' AND object_id = OBJECT_ID(N'dbo.idempotency_keys'))
    CREATE INDEX idx_idempotency_workflow_id ON dbo.idempotency_keys(workflow_id);

-- Idempotency key expiration cleanup
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_idempotency_expires' AND object_id = OBJECT_ID(N'dbo.idempotency_keys'))
    CREATE INDEX idx_idempotency_expires ON dbo.idempotency_keys(expires_at);

-- Memory sample lookups by definition
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_mem_samples_def' AND object_id = OBJECT_ID(N'dbo.workflow_memory_samples'))
    CREATE INDEX idx_mem_samples_def ON dbo.workflow_memory_samples (def_name, recorded_at DESC);

-- List workflows ordered by creation date
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_created_at' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_created_at ON dbo.workflow_instances(tenant_id, created_at DESC);

-- Terminal workflow cleanup queries
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_terminal_completed' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_terminal_completed ON dbo.workflow_instances(tenant_id, status, completed_at) WHERE status IN ('done','failed');

-- Concurrency key expiration reaper
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_concurrency_keys_expires' AND object_id = OBJECT_ID(N'dbo.concurrency_keys'))
    CREATE INDEX idx_concurrency_keys_expires ON dbo.concurrency_keys(expires_at);

-- Parent close policy enforcement
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_parent_policy' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_parent_policy ON dbo.workflow_instances(parent_workflow_id, parent_close_policy, status);

-- ===========================================================================
-- Cleanup: Drop CHECK constraints removed in migration 013
-- ===========================================================================
IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_event_history_request')
    ALTER TABLE dbo.event_history DROP CONSTRAINT ck_event_history_request;

IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_event_history_response')
    ALTER TABLE dbo.event_history DROP CONSTRAINT ck_event_history_response;

-- ===========================================================================
-- Cleanup: Drop namespace columns (for databases migrated from old schema)
-- ===========================================================================
IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'namespace')
    ALTER TABLE dbo.workflow_defs DROP COLUMN namespace;

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'namespace')
    ALTER TABLE dbo.workflow_instances DROP COLUMN namespace;

-- Add allowed_signals column for signal authorization (migration 014).
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'allowed_signals')
    ALTER TABLE dbo.workflow_instances ADD allowed_signals NVARCHAR(MAX) NULL;
