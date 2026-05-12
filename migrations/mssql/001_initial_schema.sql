-- cleat initial schema (T-SQL for SQL Server 2017+ / Azure SQL Database)
-- Core tables for workflow definitions, instances, event history,
-- signals, promises, schedules, concurrency control, and updates.
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS checks.

-- ===========================================================================
-- Table: workflow_defs
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND type = N'U')
CREATE TABLE dbo.workflow_defs (
    name            NVARCHAR(255)   NOT NULL,
    version         INT             NOT NULL,
    wasm_bytes      VARBINARY(MAX)  NOT NULL,
    entry_points    NVARCHAR(MAX)   NOT NULL DEFAULT '[]',
    min_version     INT             NOT NULL DEFAULT 0,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_workflow_defs PRIMARY KEY (name, version)
);

-- ===========================================================================
-- Table: workflow_instances
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND type = N'U')
CREATE TABLE dbo.workflow_instances (
    id              NVARCHAR(64)    NOT NULL,
    def_name        NVARCHAR(255)   NOT NULL,
    def_version     INT             NOT NULL,
    status          NVARCHAR(50)    NOT NULL DEFAULT 'ready',
    input           NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    assigned_to     NVARCHAR(255)   NULL,
    heartbeat_at    DATETIMEOFFSET  NULL,
    next_wake_at    DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    completed_at    DATETIMEOFFSET  NULL,
    CONSTRAINT pk_workflow_instances PRIMARY KEY (id),
    CONSTRAINT fk_instances_def FOREIGN KEY (def_name, def_version)
        REFERENCES dbo.workflow_defs(name, version),
    CONSTRAINT ck_workflow_instances_input CHECK (ISJSON(input) = 1)
);

-- ===========================================================================
-- Table: event_history
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.event_history') AND type = N'U')
CREATE TABLE dbo.event_history (
    workflow_id     NVARCHAR(64)    NOT NULL,
    step            INT             NOT NULL,
    service         NVARCHAR(MAX)   NOT NULL,
    operation       NVARCHAR(MAX)   NOT NULL,
    request         NVARCHAR(MAX)   NOT NULL,
    response        NVARCHAR(MAX)   NULL,
    error           NVARCHAR(MAX)   NULL,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_event_history PRIMARY KEY (workflow_id, step),
    CONSTRAINT fk_event_history_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id),
    CONSTRAINT ck_event_history_request CHECK (ISJSON(request) = 1),
    CONSTRAINT ck_event_history_response CHECK (response IS NULL OR ISJSON(response) = 1)
);

-- ===========================================================================
-- Indexes
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_ready ON dbo.workflow_instances(status, next_wake_at) WHERE status = 'ready';

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_heartbeat' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_heartbeat ON dbo.workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_defs_active' AND object_id = OBJECT_ID(N'dbo.workflow_defs'))
    CREATE INDEX idx_defs_active ON dbo.workflow_defs(name, version DESC);

-- ===========================================================================
-- Migrations: extend event_history for multiple event types
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'event_type')
    ALTER TABLE dbo.event_history ADD event_type NVARCHAR(MAX) NOT NULL DEFAULT 'call';

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'service' AND is_nullable = 0)
    ALTER TABLE dbo.event_history ALTER COLUMN service NVARCHAR(MAX) NULL;

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'operation' AND is_nullable = 0)
    ALTER TABLE dbo.event_history ALTER COLUMN operation NVARCHAR(MAX) NULL;

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'request' AND is_nullable = 0)
    ALTER TABLE dbo.event_history ALTER COLUMN request NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'duration_ms')
    ALTER TABLE dbo.event_history ADD duration_ms BIGINT NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'signal_names')
    ALTER TABLE dbo.event_history ADD signal_names NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'timeout_ms')
    ALTER TABLE dbo.event_history ADD timeout_ms BIGINT NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'signal_name')
    ALTER TABLE dbo.event_history ADD signal_name NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'signal_payload')
    ALTER TABLE dbo.event_history ADD signal_payload NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'defer_description')
    ALTER TABLE dbo.event_history ADD defer_description NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'defer_id')
    ALTER TABLE dbo.event_history ADD defer_id NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'child_name')
    ALTER TABLE dbo.event_history ADD child_name NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'child_input')
    ALTER TABLE dbo.event_history ADD child_input NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'run_id')
    ALTER TABLE dbo.event_history ADD run_id NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'new_input')
    ALTER TABLE dbo.event_history ADD new_input NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'plugin_name')
    ALTER TABLE dbo.event_history ADD plugin_name NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'plugin_func')
    ALTER TABLE dbo.event_history ADD plugin_func NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'plugin_input')
    ALTER TABLE dbo.event_history ADD plugin_input NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'plugin_output')
    ALTER TABLE dbo.event_history ADD plugin_output NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'plugin_error')
    ALTER TABLE dbo.event_history ADD plugin_error NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'promise_name')
    ALTER TABLE dbo.event_history ADD promise_name NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'promise_id')
    ALTER TABLE dbo.event_history ADD promise_id NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'promise_result')
    ALTER TABLE dbo.event_history ADD promise_result NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'promise_error')
    ALTER TABLE dbo.event_history ADD promise_error NVARCHAR(MAX) NULL;

-- ===========================================================================
-- Migrations: extend workflow_instances
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'cancellation_requested')
    ALTER TABLE dbo.workflow_instances ADD cancellation_requested BIT NOT NULL DEFAULT 0;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'cancellation_reason')
    ALTER TABLE dbo.workflow_instances ADD cancellation_reason NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'result')
    ALTER TABLE dbo.workflow_instances ADD result NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'error_msg')
    ALTER TABLE dbo.workflow_instances ADD error_msg NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'error_code')
    ALTER TABLE dbo.workflow_instances ADD error_code NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'error_op')
    ALTER TABLE dbo.workflow_instances ADD error_op NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'parent_workflow_id')
    ALTER TABLE dbo.workflow_instances ADD parent_workflow_id NVARCHAR(MAX) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'parent_close_policy')
    ALTER TABLE dbo.workflow_instances ADD parent_close_policy NVARCHAR(MAX) NULL DEFAULT 'ABANDON';

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'query_state')
    ALTER TABLE dbo.workflow_instances ADD query_state NVARCHAR(MAX) NULL DEFAULT '{}';

-- ===========================================================================
-- Migration columns: min_version (already exists), namespace, max_history_length, trace_id
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'namespace')
    ALTER TABLE dbo.workflow_defs ADD namespace NVARCHAR(255) NOT NULL DEFAULT 'default';

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'namespace')
    ALTER TABLE dbo.workflow_instances ADD namespace NVARCHAR(255) NOT NULL DEFAULT 'default';

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'max_history_length')
    ALTER TABLE dbo.workflow_defs ADD max_history_length INT NOT NULL DEFAULT 0;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'trace_id')
    ALTER TABLE dbo.workflow_instances ADD trace_id NVARCHAR(MAX) NULL;

-- ===========================================================================
-- Index for zombie instance reaper (reclaim instances with stale heartbeats)
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_stale' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_stale ON dbo.workflow_instances(status, heartbeat_at) WHERE status = 'running';

-- ===========================================================================
-- Index for namespace-routed workflow claims
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_namespace_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_namespace_ready ON dbo.workflow_instances(namespace, status, next_wake_at) WHERE status = 'ready';

-- ===========================================================================
-- Table: workflow_signals
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_signals') AND type = N'U')
CREATE TABLE dbo.workflow_signals (
    workflow_id     NVARCHAR(64)    NOT NULL,
    signal_name     NVARCHAR(255)   NOT NULL,
    payload         NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    delivered_at    DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_workflow_signals PRIMARY KEY (workflow_id, signal_name),
    CONSTRAINT fk_signals_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id),
    CONSTRAINT ck_workflow_signals_payload CHECK (ISJSON(payload) = 1)
);

-- ===========================================================================
-- Table: workflow_promises
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_promises') AND type = N'U')
CREATE TABLE dbo.workflow_promises (
    workflow_id     NVARCHAR(64)    NOT NULL,
    promise_id      NVARCHAR(64)    NOT NULL,
    promise_name    NVARCHAR(MAX)   NOT NULL,
    status          NVARCHAR(50)    NOT NULL DEFAULT 'pending',
    result          NVARCHAR(MAX)   NULL,
    error_msg       NVARCHAR(MAX)   NULL,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    resolved_at     DATETIMEOFFSET  NULL,
    CONSTRAINT pk_workflow_promises PRIMARY KEY (workflow_id, promise_id),
    CONSTRAINT fk_promises_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id),
    CONSTRAINT ck_workflow_promises_result CHECK (result IS NULL OR ISJSON(result) = 1)
);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_promises_status' AND object_id = OBJECT_ID(N'dbo.workflow_promises'))
    CREATE INDEX idx_promises_status ON dbo.workflow_promises(workflow_id, status);

-- ===========================================================================
-- Table: workflow_schedules
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules') AND type = N'U')
CREATE TABLE dbo.workflow_schedules (
    name            NVARCHAR(255)   NOT NULL,
    def_name        NVARCHAR(MAX)   NOT NULL,
    entry_point     NVARCHAR(MAX)   NOT NULL DEFAULT '',
    cron_expression NVARCHAR(MAX)   NOT NULL,
    input           NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    enabled         BIT             NOT NULL DEFAULT 1,
    next_run_at     DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    last_run_at     DATETIMEOFFSET  NULL,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_workflow_schedules PRIMARY KEY (name),
    CONSTRAINT ck_workflow_schedules_input CHECK (ISJSON(input) = 1)
);

-- ===========================================================================
-- Table: concurrency_keys
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.concurrency_keys') AND type = N'U')
CREATE TABLE dbo.concurrency_keys (
    key_hash        VARBINARY(32)   NOT NULL,
    key_text        NVARCHAR(MAX)   NOT NULL,
    workflow_id     NVARCHAR(64)    NOT NULL,
    acquired_at     DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    expires_at      DATETIMEOFFSET  NOT NULL,
    CONSTRAINT pk_concurrency_keys PRIMARY KEY (key_hash),
    CONSTRAINT fk_concurrency_keys_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id)
);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_concurrency_keys_workflow' AND object_id = OBJECT_ID(N'dbo.concurrency_keys'))
    CREATE INDEX idx_concurrency_keys_workflow ON dbo.concurrency_keys(workflow_id);

-- ===========================================================================
-- Migration: add dag_spec JSONB to workflow_defs for DAG visualization (Wave 3)
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'dag_spec')
    ALTER TABLE dbo.workflow_defs ADD dag_spec NVARCHAR(MAX) NULL;

-- ===========================================================================
-- Migration: add sticky_worker_id for sticky sessions (Feature 10)
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'sticky_worker_id')
    ALTER TABLE dbo.workflow_instances ADD sticky_worker_id NVARCHAR(255) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_sticky' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_sticky ON dbo.workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL;

-- ===========================================================================
-- Table: workflow_update_requests
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_update_requests') AND type = N'U')
CREATE TABLE dbo.workflow_update_requests (
    workflow_id     NVARCHAR(64)    NOT NULL,
    update_name     NVARCHAR(255)   NOT NULL,
    payload         NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    promise_id      NVARCHAR(MAX)   NULL,
    status          NVARCHAR(50)    NOT NULL DEFAULT 'pending',
    result          NVARCHAR(MAX)   NULL,
    error_msg       NVARCHAR(MAX)   NULL,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    completed_at    DATETIMEOFFSET  NULL,
    CONSTRAINT pk_workflow_update_requests PRIMARY KEY (workflow_id, update_name),
    CONSTRAINT fk_update_requests_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id),
    CONSTRAINT ck_workflow_update_requests_payload CHECK (ISJSON(payload) = 1)
);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_update_requests_pending' AND object_id = OBJECT_ID(N'dbo.workflow_update_requests'))
    CREATE INDEX idx_update_requests_pending ON dbo.workflow_update_requests(workflow_id, status);
