-- cleat workflow definitions and instances schema
--
-- This is a bootstrap schema for local/dev use (e.g. docker-compose.cluster.yml
-- mounts it into postgres:/docker-entrypoint-initdb.d). It intentionally
-- creates the full, current table shapes directly rather than an incremental
-- history of ALTER TABLEs, so it stays a straightforward no-op superset when
-- engine/testutil.SetupFullSchema (the idempotent CREATE TABLE IF NOT EXISTS /
-- ALTER TABLE ADD COLUMN IF NOT EXISTS test-schema helper) runs against it.
--
-- The authoritative, versioned production schema lives in migrations/postgres/
-- (applied via the engine's own migration path); this file is kept in sync
-- with engine/testutil/schema.go's PostgreSQL dialect by hand and is not a
-- substitute for those migrations.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS workflow_defs (
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    wasm_bytes BYTEA NOT NULL,
    entry_points TEXT[] NOT NULL DEFAULT '{}',
    min_version INTEGER NOT NULL DEFAULT 0,
    max_history_length INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    abi_version INTEGER NOT NULL DEFAULT 1,
    plugin_deps JSONB NOT NULL DEFAULT '{}',
    deprecated BOOLEAN NOT NULL DEFAULT false,
    tenant_id UUID,
    task_queue TEXT NOT NULL DEFAULT 'default',
    dag_spec JSONB DEFAULT NULL,
    PRIMARY KEY (name, version)
);

CREATE TABLE IF NOT EXISTS workflow_instances (
    id TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    def_version INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'ready',
    input JSONB NOT NULL DEFAULT '{}',
    assigned_to TEXT,
    heartbeat_at TIMESTAMPTZ,
    next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    result JSONB,
    error_msg TEXT,
    error_code TEXT,
    error_op TEXT,
    parent_workflow_id TEXT,
    parent_close_policy TEXT DEFAULT 'ABANDON',
    trace_id TEXT,
    query_state JSONB DEFAULT '{}',
    task_queue TEXT NOT NULL DEFAULT 'default',
    cancellation_requested BOOLEAN NOT NULL DEFAULT false,
    cancellation_reason TEXT,
    sticky_worker_id TEXT,
    tenant_id UUID,
    compaction_state JSONB,
    compacted_at TIMESTAMPTZ,
    compaction_step INTEGER,
    plugin_vers JSONB NOT NULL DEFAULT '{}',
    event_count BIGINT NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 0,
    allowed_signals JSONB DEFAULT NULL,
    generation BIGINT NOT NULL DEFAULT 0
    -- No FK to workflow_defs(name, version): engine/testutil's schema (the
    -- authoritative test-schema shape this file mirrors) does not have one
    -- either, and tests insert workflow_instances rows without a matching
    -- workflow_defs row.
);

CREATE TABLE IF NOT EXISTS event_history (
    workflow_id TEXT NOT NULL,
    step INTEGER NOT NULL,
    event_type TEXT NOT NULL DEFAULT 'call',
    service TEXT,
    operation TEXT,
    request TEXT,
    response TEXT,
    error TEXT,
    duration_ms BIGINT,
    signal_names TEXT,
    timeout_ms BIGINT,
    signal_name TEXT,
    signal_payload TEXT,
    defer_description TEXT,
    defer_id TEXT,
    child_name TEXT,
    child_input TEXT,
    run_id TEXT,
    new_input TEXT,
    plugin_name TEXT,
    plugin_func TEXT,
    plugin_input TEXT,
    plugin_output TEXT,
    plugin_error TEXT,
    promise_name TEXT,
    promise_id TEXT,
    promise_result TEXT,
    promise_error TEXT,
    tenant_id UUID,
    payload JSONB,
    checksum TEXT,
    thread_id TEXT NOT NULL DEFAULT 'main',
    local_step INTEGER NOT NULL DEFAULT 0,
    global_seq BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, step),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS workflow_signals (
    workflow_id TEXT NOT NULL,
    signal_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID,
    PRIMARY KEY (workflow_id, signal_name)
);

CREATE TABLE IF NOT EXISTS workflow_schedules (
    name TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    entry_point TEXT NOT NULL DEFAULT '',
    cron_expression TEXT NOT NULL,
    input JSONB NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT true,
    next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_run_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID
);

CREATE TABLE IF NOT EXISTS concurrency_keys (
    key_hash BYTEA PRIMARY KEY,
    key_text TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    tenant_id TEXT
);

CREATE TABLE IF NOT EXISTS workflow_promises (
    workflow_id TEXT NOT NULL,
    promise_id TEXT NOT NULL,
    promise_name TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    tenant_id UUID,
    PRIMARY KEY (workflow_id, promise_id)
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key_hash BYTEA NOT NULL PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS workflow_update_requests (
    workflow_id TEXT NOT NULL,
    update_name TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    payload JSONB NOT NULL DEFAULT '{}',
    promise_id TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    tenant_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, update_name)
);

CREATE TABLE IF NOT EXISTS workflow_memory_samples (
    id BIGSERIAL PRIMARY KEY,
    def_name TEXT NOT NULL,
    sample_bytes BIGINT NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workflow_memory_stats (
    def_name TEXT PRIMARY KEY,
    mean_bytes DOUBLE PRECISION NOT NULL DEFAULT 0,
    sample_count INTEGER NOT NULL DEFAULT 0,
    alpha DOUBLE PRECISION NOT NULL DEFAULT 0.3,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Indexes
-- ---------------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS idx_defs_active ON workflow_defs(name, version DESC);
CREATE INDEX IF NOT EXISTS idx_instances_ready ON workflow_instances(status, next_wake_at) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_instances_sticky ON workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_instances_tenant_queue_ready ON workflow_instances(tenant_id, task_queue, status, priority ASC, next_wake_at) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_promises_status ON workflow_promises(workflow_id, status);
CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id);
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_expires ON idempotency_keys(expires_at);
CREATE INDEX IF NOT EXISTS idx_update_requests_pending ON workflow_update_requests(workflow_id, status);
CREATE INDEX IF NOT EXISTS idx_mem_samples_def ON workflow_memory_samples(def_name, recorded_at DESC);
