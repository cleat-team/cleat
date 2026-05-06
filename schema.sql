-- cleat workflow definitions and instances schema
-- Run this against your PostgreSQL database before deploying workflows.

CREATE TABLE IF NOT EXISTS workflow_defs (
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    wasm_bytes BYTEA NOT NULL,
    entry_points TEXT[] NOT NULL DEFAULT '{}',
    min_version INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version)
);

CREATE TABLE IF NOT EXISTS workflow_instances (
    id TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    def_version INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'ready',
    input JSONB NOT NULL DEFAULT '{}',
    assigned_to TEXT,
    heartbeat_at TIMESTAMPTZ,
    next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
);

CREATE TABLE IF NOT EXISTS event_history (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    step INTEGER NOT NULL,
    service TEXT NOT NULL,
    operation TEXT NOT NULL,
    request JSONB NOT NULL,
    response JSONB,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, step)
);

CREATE INDEX IF NOT EXISTS idx_instances_ready ON workflow_instances(status, next_wake_at) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_defs_active ON workflow_defs(name, version DESC);

-- ---------------------------------------------------------------------------
-- Migrations: extend event_history for multiple event types
-- ---------------------------------------------------------------------------
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS event_type TEXT NOT NULL DEFAULT 'call';
ALTER TABLE event_history ALTER COLUMN service DROP NOT NULL;
ALTER TABLE event_history ALTER COLUMN operation DROP NOT NULL;
ALTER TABLE event_history ALTER COLUMN request DROP NOT NULL;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS duration_ms BIGINT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS signal_names TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS timeout_ms BIGINT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS signal_name TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS signal_payload JSONB;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS defer_description TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS defer_id TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS child_name TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS child_input JSONB;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS run_id TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS new_input JSONB;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS plugin_name TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS plugin_func TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS plugin_input JSONB;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS plugin_output JSONB;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS plugin_error TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS promise_name TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS promise_id TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS promise_result TEXT;
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS promise_error TEXT;

-- ---------------------------------------------------------------------------
-- Migrations: extend workflow_instances
-- ---------------------------------------------------------------------------
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS cancellation_requested BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS cancellation_reason TEXT;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS result JSONB;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS error_msg TEXT;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS parent_workflow_id TEXT;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS query_state JSONB DEFAULT '{}';

-- Migration: add min_version column to workflow_defs
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS min_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT 'default';
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT 'default';
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS max_history_length INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS trace_id TEXT;

-- Index for zombie instance reaper (reclaim instances with stale heartbeats)
CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running';
-- Index for namespace-routed workflow claims
CREATE INDEX IF NOT EXISTS idx_instances_namespace_ready ON workflow_instances(namespace, status, next_wake_at) WHERE status = 'ready';

-- ---------------------------------------------------------------------------
-- New: workflow_signals table
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_signals (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    signal_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, signal_name)
);

-- ---------------------------------------------------------------------------
-- New: workflow_promises table
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_promises (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    promise_id TEXT NOT NULL,
    promise_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, promise_id)
);
CREATE INDEX IF NOT EXISTS idx_promises_status ON workflow_promises(workflow_id, status);

-- ---------------------------------------------------------------------------
-- Schedules: cron-based recurring workflow execution
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_schedules (
    name TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    entry_point TEXT NOT NULL DEFAULT '',
    cron_expression TEXT NOT NULL,
    input JSONB NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT true,
    next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_run_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- New: concurrency_keys table (Feature 5: Per-Key Concurrency Control)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS concurrency_keys (
    key_hash BYTEA PRIMARY KEY,
    key_text TEXT NOT NULL,
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id);

-- ---------------------------------------------------------------------------
-- Migration: add sticky_worker_id for sticky sessions (Feature 10)
-- ---------------------------------------------------------------------------
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS sticky_worker_id TEXT;
CREATE INDEX IF NOT EXISTS idx_instances_sticky ON workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- New: workflow_update_requests table (Feature 3: Update Handler)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_update_requests (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    update_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    promise_id TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, update_name)
);
CREATE INDEX IF NOT EXISTS idx_update_requests_pending ON workflow_update_requests(workflow_id, status);
