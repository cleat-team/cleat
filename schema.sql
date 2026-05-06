-- cleat workflow definitions and instances schema
-- Run this against your PostgreSQL database before deploying workflows.

CREATE TABLE IF NOT EXISTS workflow_defs (
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    wasm_bytes BYTEA NOT NULL,
    entry_points TEXT[] NOT NULL DEFAULT '{}',
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

-- ---------------------------------------------------------------------------
-- Migrations: extend workflow_instances
-- ---------------------------------------------------------------------------
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS cancellation_requested BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS cancellation_reason TEXT;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS result JSONB;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS error_msg TEXT;

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
