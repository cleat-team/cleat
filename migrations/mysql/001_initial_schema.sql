-- cleat MySQL migration 001: initial schema
-- Core tables for workflow definitions, instances, event history,
-- signals, promises, schedules, concurrency control, updates, and idempotency.
--
-- MySQL differences from PostgreSQL:
--   - TEXT columns used as primary keys are VARCHAR(255) (MySQL requires prefix or non-TEXT for PK)
--   - JSONB becomes JSON
--   - BYTEA becomes LONGBLOB
--   - TIMESTAMPTZ becomes TIMESTAMP(6) (stored as UTC, no timezone)
--   - BOOLEAN becomes TINYINT(1)
--   - Partial indexes (WHERE clause) are omitted — MySQL does not support them.
--     Application-level filtering must be done in queries.
--   - DESC in index definitions omitted; MySQL ignores direction for most purposes.
--   - UUIDs are generated in Go application code, not in SQL.
--   - gen_random_uuid() DEFAULT is omitted.
--   - now() becomes NOW(6) for microsecond precision.
--   - TEXT[] becomes JSON (stored as JSON array).
--   - Idempotent: CREATE TABLE IF NOT EXISTS, ALTER TABLE ADD COLUMN with safety comments.

CREATE TABLE IF NOT EXISTS workflow_defs (
    name               VARCHAR(255) NOT NULL,
    version            INTEGER NOT NULL,
    wasm_bytes         LONGBLOB NOT NULL,
    entry_points       JSON NOT NULL DEFAULT ('[]'),
    min_version        INTEGER NOT NULL DEFAULT 0,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (name, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS workflow_instances (
    id                 VARCHAR(255) NOT NULL,
    def_name           VARCHAR(255) NOT NULL,
    def_version        INTEGER NOT NULL,
    status             VARCHAR(50) NOT NULL DEFAULT 'ready',
    input              JSON NOT NULL DEFAULT ('{}'),
    assigned_to        VARCHAR(255),
    heartbeat_at       TIMESTAMP(6),
    next_wake_at       TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    completed_at       TIMESTAMP(6),
    PRIMARY KEY (id),
    FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS event_history (
    workflow_id        VARCHAR(255) NOT NULL,
    step               INTEGER NOT NULL,
    service            VARCHAR(255) NOT NULL,
    operation          VARCHAR(255) NOT NULL,
    request            JSON NOT NULL,
    response           JSON,
    error              TEXT,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (workflow_id, step),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Note: MySQL does not support partial indexes (WHERE clause). The following
-- indexes are full indexes on the listed columns. Application code must add
-- WHERE status = 'ready' to queries using idx_instances_ready.
CREATE INDEX idx_instances_ready ON workflow_instances(status, next_wake_at);

-- Note: Partial index WHERE status = 'running' omitted.
CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at);

-- Note: DESC omitted from index; ASC is sufficient in MySQL.
CREATE INDEX idx_defs_active ON workflow_defs(name, version);

-- ---------------------------------------------------------------------------
-- Migrations: extend event_history for multiple event types
-- ---------------------------------------------------------------------------
-- MySQL does not support IF NOT EXISTS for ADD COLUMN.
-- ensure column does not exist before running
ALTER TABLE event_history ADD COLUMN event_type VARCHAR(255) NOT NULL DEFAULT 'call';
ALTER TABLE event_history MODIFY COLUMN service VARCHAR(255) NULL;
ALTER TABLE event_history MODIFY COLUMN operation VARCHAR(255) NULL;
ALTER TABLE event_history MODIFY COLUMN request JSON NULL;
ALTER TABLE event_history ADD COLUMN duration_ms BIGINT;
ALTER TABLE event_history ADD COLUMN signal_names TEXT;
ALTER TABLE event_history ADD COLUMN timeout_ms BIGINT;
ALTER TABLE event_history ADD COLUMN signal_name VARCHAR(255);
ALTER TABLE event_history ADD COLUMN signal_payload JSON;
ALTER TABLE event_history ADD COLUMN defer_description TEXT;
ALTER TABLE event_history ADD COLUMN defer_id VARCHAR(255);
ALTER TABLE event_history ADD COLUMN child_name VARCHAR(255);
ALTER TABLE event_history ADD COLUMN child_input JSON;
ALTER TABLE event_history ADD COLUMN run_id VARCHAR(255);
ALTER TABLE event_history ADD COLUMN new_input JSON;
ALTER TABLE event_history ADD COLUMN plugin_name VARCHAR(255);
ALTER TABLE event_history ADD COLUMN plugin_func VARCHAR(255);
ALTER TABLE event_history ADD COLUMN plugin_input JSON;
ALTER TABLE event_history ADD COLUMN plugin_output JSON;
ALTER TABLE event_history ADD COLUMN plugin_error TEXT;
ALTER TABLE event_history ADD COLUMN promise_name VARCHAR(255);
ALTER TABLE event_history ADD COLUMN promise_id VARCHAR(255);
ALTER TABLE event_history ADD COLUMN promise_result TEXT;
ALTER TABLE event_history ADD COLUMN promise_error TEXT;

-- ---------------------------------------------------------------------------
-- Migrations: extend workflow_instances
-- ---------------------------------------------------------------------------
ALTER TABLE workflow_instances ADD COLUMN cancellation_requested TINYINT(1) NOT NULL DEFAULT 0;
ALTER TABLE workflow_instances ADD COLUMN cancellation_reason TEXT;
ALTER TABLE workflow_instances ADD COLUMN result JSON;
ALTER TABLE workflow_instances ADD COLUMN error_msg TEXT;
ALTER TABLE workflow_instances ADD COLUMN error_code VARCHAR(255);
ALTER TABLE workflow_instances ADD COLUMN error_op VARCHAR(255);
ALTER TABLE workflow_instances ADD COLUMN parent_workflow_id VARCHAR(255);
ALTER TABLE workflow_instances ADD COLUMN parent_close_policy VARCHAR(50) DEFAULT 'ABANDON';
ALTER TABLE workflow_instances ADD COLUMN query_state JSON DEFAULT ('{}');

-- Migration: add min_version/namespace/history columns to workflow_defs/instances
ALTER TABLE workflow_defs ADD COLUMN min_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_defs ADD COLUMN namespace VARCHAR(255) NOT NULL DEFAULT 'default';
ALTER TABLE workflow_instances ADD COLUMN namespace VARCHAR(255) NOT NULL DEFAULT 'default';
ALTER TABLE workflow_defs ADD COLUMN max_history_length INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_instances ADD COLUMN trace_id VARCHAR(255);

-- Index for zombie instance reaper (reclaim instances with stale heartbeats)
-- Note: Partial index WHERE status = 'running' omitted.
CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at);

-- Index for namespace-routed workflow claims
-- Note: Partial index WHERE status = 'ready' omitted.
CREATE INDEX idx_instances_namespace_ready ON workflow_instances(namespace, status, next_wake_at);

-- ---------------------------------------------------------------------------
-- New: workflow_signals table
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_signals (
    workflow_id        VARCHAR(255) NOT NULL,
    signal_name        VARCHAR(255) NOT NULL,
    payload            JSON NOT NULL DEFAULT ('{}'),
    delivered_at       TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (workflow_id, signal_name),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- New: workflow_promises table
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_promises (
    workflow_id        VARCHAR(255) NOT NULL,
    promise_id         VARCHAR(255) NOT NULL,
    promise_name       VARCHAR(255) NOT NULL,
    status             VARCHAR(50) NOT NULL DEFAULT 'pending',
    result             JSON,
    error_msg          TEXT,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    resolved_at        TIMESTAMP(6),
    PRIMARY KEY (workflow_id, promise_id),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_promises_status ON workflow_promises(workflow_id, status);

-- ---------------------------------------------------------------------------
-- Schedules: cron-based recurring workflow execution
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_schedules (
    name               VARCHAR(255) NOT NULL,
    def_name           VARCHAR(255) NOT NULL,
    entry_point        VARCHAR(255) NOT NULL DEFAULT '',
    cron_expression    TEXT NOT NULL,
    input              JSON NOT NULL DEFAULT ('{}'),
    enabled            TINYINT(1) NOT NULL DEFAULT 1,
    next_run_at        TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    last_run_at        TIMESTAMP(6),
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- New: concurrency_keys table (Feature 5: Per-Key Concurrency Control)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS concurrency_keys (
    key_hash           VARBINARY(64) PRIMARY KEY,
    key_text           TEXT NOT NULL,
    workflow_id        VARCHAR(255) NOT NULL,
    acquired_at        TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    expires_at         TIMESTAMP(6) NOT NULL,
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_concurrency_keys_workflow ON concurrency_keys(workflow_id);

-- ---------------------------------------------------------------------------
-- Migration: add dag_spec JSON to workflow_defs for DAG visualization
-- ---------------------------------------------------------------------------
-- ensure column does not exist before running
ALTER TABLE workflow_defs ADD COLUMN dag_spec JSON DEFAULT NULL;

-- ---------------------------------------------------------------------------
-- Migration: add sticky_worker_id for sticky sessions (Feature 10)
-- ---------------------------------------------------------------------------
-- ensure column does not exist before running
ALTER TABLE workflow_instances ADD COLUMN sticky_worker_id VARCHAR(255);

-- Note: Partial index WHERE sticky_worker_id IS NOT NULL omitted.
CREATE INDEX idx_instances_sticky ON workflow_instances(sticky_worker_id);

-- ---------------------------------------------------------------------------
-- New: workflow_update_requests table (Feature 3: Update Handler)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_update_requests (
    workflow_id        VARCHAR(255) NOT NULL,
    update_name        VARCHAR(255) NOT NULL,
    payload            JSON NOT NULL DEFAULT ('{}'),
    promise_id         VARCHAR(255),
    status             VARCHAR(50) NOT NULL DEFAULT 'pending',
    result             JSON,
    error_msg          TEXT,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    completed_at       TIMESTAMP(6),
    PRIMARY KEY (workflow_id, update_name),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_update_requests_pending ON workflow_update_requests(workflow_id, status);
