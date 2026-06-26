-- cleat MySQL consolidated schema (001)
-- Merge of: 001_tables, 002_constraints, 005_priority, 006_priority_promises_updates,
--            007_event_history_cascade, 007_fk_cascade, 008_workflow_tags_routing,
--            009_update_requests_tenant_id, 011_claim_workflows_index
--
-- CREATE TABLE IF NOT EXISTS guards idempotency.
-- CREATE INDEX has no IF NOT EXISTS in MySQL 8.0; re-runs error harmlessly.

-- ============================================================
-- Tables
-- ============================================================

CREATE TABLE IF NOT EXISTS workflow_defs (
    name               VARCHAR(255) NOT NULL,
    version            INTEGER NOT NULL,
    wasm_bytes         LONGBLOB NOT NULL,
    entry_points       JSON NOT NULL DEFAULT ('[]'),
    min_version        INTEGER NOT NULL DEFAULT 0,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    max_history_length INTEGER NOT NULL DEFAULT 0,
    dag_spec JSON DEFAULT NULL,
    tenant_id CHAR(36),
    task_queue VARCHAR(255) NOT NULL DEFAULT 'default',
    abi_version INTEGER NOT NULL DEFAULT 1,
    plugin_deps JSON NOT NULL DEFAULT ('{}'),
    deprecated TINYINT(1) NOT NULL DEFAULT 0,
    PRIMARY KEY (name, version)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS plugin_defs (
    name               VARCHAR(255) NOT NULL,
    version            VARCHAR(255) NOT NULL,
    wasm_bytes         LONGBLOB,
    config             JSON NOT NULL DEFAULT ('{}'),
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    deprecated         TINYINT(1) NOT NULL DEFAULT 0,
    PRIMARY KEY (name, version)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id          CHAR(36) NOT NULL,
    name               VARCHAR(255) NOT NULL,
    display_name       VARCHAR(255) NOT NULL DEFAULT '',
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    suspended          TINYINT(1) NOT NULL DEFAULT 0,
    UNIQUE KEY uq_tenants_name (name),
    PRIMARY KEY (tenant_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS tenant_api_keys (
    key_id             CHAR(36) NOT NULL,
    tenant_id          CHAR(36) NOT NULL,
    key_hash           VARBINARY(32) NOT NULL,
    description        VARCHAR(1024) NOT NULL DEFAULT '',
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    revoked_at         TIMESTAMP(6),
    FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id),
    PRIMARY KEY (key_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS tenant_roles (
    tenant_id          CHAR(36) NOT NULL,
    role_name          VARCHAR(255) NOT NULL,
    password           TEXT NOT NULL,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    UNIQUE KEY uq_tenant_roles_role_name (role_name),
    FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id),
    PRIMARY KEY (tenant_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS plugin_tables (
    plugin_name        VARCHAR(255) NOT NULL,
    table_name         VARCHAR(255) NOT NULL,
    PRIMARY KEY (plugin_name, table_name)
) ENGINE=InnoDB;

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
    cancellation_requested TINYINT(1) NOT NULL DEFAULT 0,
    cancellation_reason TEXT,
    result JSON,
    error_msg TEXT,
    error_code VARCHAR(255),
    error_op VARCHAR(255),
    parent_workflow_id VARCHAR(255),
    parent_close_policy VARCHAR(50) DEFAULT 'ABANDON',
    query_state JSON DEFAULT ('{}'),
    trace_id VARCHAR(255),
    sticky_worker_id VARCHAR(255),
    tenant_id CHAR(36) NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    task_queue VARCHAR(255) NOT NULL DEFAULT 'default',
    compaction_state JSON,
    compacted_at TIMESTAMP(6),
    compaction_step INTEGER,
    plugin_vers JSON NOT NULL DEFAULT ('{}'),
    event_count BIGINT NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 0,
    generation BIGINT NOT NULL DEFAULT 0,
    allowed_signals JSON DEFAULT NULL,
    FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version),
    PRIMARY KEY (id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS event_history (
    workflow_id        VARCHAR(255) NOT NULL,
    step               INTEGER NOT NULL,
    service VARCHAR(255) NOT NULL,
    operation VARCHAR(255) NOT NULL,
    request LONGTEXT NOT NULL,
    response LONGTEXT,
    error              TEXT,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    event_type VARCHAR(255) NOT NULL DEFAULT 'call',
    duration_ms BIGINT,
    signal_names TEXT,
    timeout_ms BIGINT,
    signal_name VARCHAR(255),
    signal_payload LONGTEXT,
    defer_description TEXT,
    defer_id VARCHAR(255),
    child_name VARCHAR(255),
    child_input LONGTEXT,
    run_id VARCHAR(255),
    new_input LONGTEXT,
    plugin_name VARCHAR(255),
    plugin_func VARCHAR(255),
    plugin_input LONGTEXT,
    plugin_output LONGTEXT,
    plugin_error TEXT,
    promise_name VARCHAR(255),
    promise_id VARCHAR(255),
    promise_result TEXT,
    promise_error TEXT,
    tenant_id CHAR(36),
    payload JSON,
    checksum TEXT,
    thread_id VARCHAR(255) NOT NULL DEFAULT 'main',
    local_step INTEGER NOT NULL DEFAULT 0,
    global_seq BIGINT NOT NULL DEFAULT 0,
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE,
    PRIMARY KEY (workflow_id, step)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS workflow_signals (
    workflow_id        VARCHAR(255) NOT NULL,
    signal_name        VARCHAR(255) NOT NULL,
    payload            JSON NOT NULL DEFAULT ('{}'),
    delivered_at       TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    tenant_id CHAR(36),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE,
    PRIMARY KEY (workflow_id, signal_name)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS workflow_promises (
    workflow_id        VARCHAR(255) NOT NULL,
    promise_id         VARCHAR(255) NOT NULL,
    promise_name       VARCHAR(255) NOT NULL,
    tenant_id          VARCHAR(255) NOT NULL,
    priority           INTEGER NOT NULL DEFAULT 0,
    status             VARCHAR(50) NOT NULL DEFAULT 'pending',
    result             JSON,
    error_msg          TEXT,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    resolved_at        TIMESTAMP(6),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE,
    PRIMARY KEY (workflow_id, promise_id)
) ENGINE=InnoDB;

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
    tenant_id CHAR(36),
    PRIMARY KEY (name)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS concurrency_keys (
    key_hash           VARBINARY(32) PRIMARY KEY,
    key_text           TEXT NOT NULL,
    workflow_id        VARCHAR(255) NOT NULL,
    acquired_at        TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    expires_at         TIMESTAMP(6) NOT NULL,
    tenant_id          CHAR(36),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key_hash           VARBINARY(32) NOT NULL,
    workflow_id        VARCHAR(255) NOT NULL,
    result             JSON,
    error_msg          TEXT,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    expires_at         TIMESTAMP(6) NOT NULL DEFAULT (NOW(6) + INTERVAL 7 DAY),
    PRIMARY KEY (key_hash)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS workflow_update_requests (
    workflow_id        VARCHAR(255) NOT NULL,
    update_name        VARCHAR(255) NOT NULL,
    tenant_id          CHAR(36) NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    priority           INTEGER NOT NULL DEFAULT 0,
    payload            JSON NOT NULL DEFAULT ('{}'),
    promise_id         VARCHAR(255),
    status             VARCHAR(50) NOT NULL DEFAULT 'pending',
    result             JSON,
    error_msg          TEXT,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    completed_at       TIMESTAMP(6),
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE,
    PRIMARY KEY (workflow_id, update_name)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS workflow_tags (
    workflow_name VARCHAR(255) NOT NULL,
    version INTEGER NOT NULL,
    tag VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    tenant_id CHAR(36),
    PRIMARY KEY (workflow_name, tag),
    FOREIGN KEY (workflow_name, version) REFERENCES workflow_defs(name, version)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS workflow_routing (
    id CHAR(36) NOT NULL,
    workflow_name VARCHAR(255) NOT NULL,
    target_version INTEGER NOT NULL,
    weight DOUBLE NOT NULL DEFAULT 1.0,
    created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    tenant_id CHAR(36),
    PRIMARY KEY (id),
    FOREIGN KEY (workflow_name, target_version) REFERENCES workflow_defs(name, version)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS workflow_memory_samples (
    id                 BIGINT NOT NULL AUTO_INCREMENT,
    def_name           VARCHAR(255) NOT NULL,
    sample_bytes       BIGINT NOT NULL,
    recorded_at        TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS workflow_memory_stats (
    def_name           VARCHAR(255) NOT NULL,
    mean_bytes         DOUBLE NOT NULL DEFAULT 0,
    sample_count       INTEGER NOT NULL DEFAULT 0,
    alpha              DOUBLE NOT NULL DEFAULT 0.3,
    updated_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (def_name)
) ENGINE=InnoDB;

-- ============================================================
-- Indexes (final definitions, no IF NOT EXISTS in MySQL 8.0)
-- ============================================================

CREATE INDEX idx_instances_ready ON workflow_instances(status, next_wake_at);
CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at);
CREATE INDEX idx_defs_active ON workflow_defs(name, version);
CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at);
CREATE INDEX idx_promises_status ON workflow_promises(workflow_id, status);
CREATE INDEX idx_concurrency_keys_workflow ON concurrency_keys(workflow_id);
CREATE INDEX idx_instances_sticky ON workflow_instances(sticky_worker_id);
CREATE INDEX idx_update_requests_pending ON workflow_update_requests(workflow_id, status);
CREATE INDEX idx_api_keys_hash ON tenant_api_keys(key_hash);
CREATE INDEX idx_defs_tenant_name_version ON workflow_defs(tenant_id, name, version);
CREATE INDEX idx_instances_tenant_ready ON workflow_instances(tenant_id, status, next_wake_at);
CREATE INDEX idx_event_history_tenant_wf ON event_history(tenant_id, workflow_id, step);
CREATE INDEX idx_signals_tenant_wf ON workflow_signals(tenant_id, workflow_id, signal_name);
CREATE INDEX idx_schedules_tenant_enabled ON workflow_schedules(tenant_id, enabled, next_run_at);
CREATE INDEX idx_instances_tenant_queue_ready ON workflow_instances(tenant_id, task_queue, status, priority, next_wake_at);
CREATE INDEX idx_idempotency_workflow_id ON idempotency_keys(workflow_id);
CREATE INDEX idx_idempotency_expires ON idempotency_keys(expires_at);
CREATE INDEX idx_mem_samples_def ON workflow_memory_samples(def_name, recorded_at);
CREATE INDEX idx_instances_created_at ON workflow_instances(tenant_id, created_at);
CREATE INDEX idx_instances_terminal_completed ON workflow_instances(tenant_id, status, completed_at);
CREATE INDEX idx_concurrency_keys_expires ON concurrency_keys(expires_at);
CREATE INDEX idx_instances_parent_policy ON workflow_instances(parent_workflow_id, parent_close_policy, status);
CREATE INDEX idx_workflow_instances_ready_claim ON workflow_instances(tenant_id, task_queue, status, priority, created_at);
