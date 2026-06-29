-- cleat mssql schema (consolidated)
-- All CREATE TABLE, indexes, fn_tenant_filter, and security policies compiled from
-- migrations 001-011 (excluding no-op 008_rls_fail_closed).
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS guards.
-- Target: fresh installs only.

-- ===========================================================================
-- Schema: admin
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.schemas WHERE name = N'admin')
    EXEC('CREATE SCHEMA admin');

GO

-- ===========================================================================
-- Function: dbo.fn_tenant_filter
-- Inline TVF used by SECURITY POLICY (native MSSQL RLS).
-- Returns a row only when the table's tenant_id matches the session-scoped
-- tenant_id set via sp_set_session_context 'tenant_id'.
-- ===========================================================================
CREATE OR ALTER FUNCTION dbo.fn_tenant_filter(@tenant_id UNIQUEIDENTIFIER)
RETURNS TABLE
WITH SCHEMABINDING
AS
RETURN SELECT 1 AS access
    WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER);
GO

-- ===========================================================================
-- Table: admin.tenants
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.tenants') AND type = N'U')
CREATE TABLE admin.tenants (
    tenant_id    UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
    name         NVARCHAR(255)    NOT NULL,
    display_name NVARCHAR(MAX)    NOT NULL DEFAULT '',
    created_at   DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    suspended    BIT              NOT NULL DEFAULT 0,
    CONSTRAINT pk_admin_tenants PRIMARY KEY (tenant_id),
    CONSTRAINT uq_admin_tenants_name UNIQUE (name)
);

-- ===========================================================================
-- Table: admin.tenant_api_keys
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.tenant_api_keys') AND type = N'U')
CREATE TABLE admin.tenant_api_keys (
    key_id      UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
    tenant_id   UNIQUEIDENTIFIER NOT NULL,
    key_hash    VARBINARY(32)    NOT NULL,
    description NVARCHAR(MAX)    NOT NULL DEFAULT '',
    created_at  DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    revoked_at  DATETIMEOFFSET   NULL,
    CONSTRAINT pk_admin_tenant_api_keys PRIMARY KEY (key_id),
    CONSTRAINT fk_admin_api_keys_tenant FOREIGN KEY (tenant_id)
        REFERENCES admin.tenants(tenant_id)
);

-- ===========================================================================
-- Table: admin.plugin_tables
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.plugin_tables') AND type = N'U')
CREATE TABLE admin.plugin_tables (
    plugin_name NVARCHAR(200) NOT NULL,
    table_name  NVARCHAR(200) NOT NULL,
    CONSTRAINT pk_admin_plugin_tables PRIMARY KEY (plugin_name, table_name)
);

-- ===========================================================================
-- Table: admin.tenant_roles
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.tenant_roles') AND type = N'U')
CREATE TABLE admin.tenant_roles (
    tenant_id  UNIQUEIDENTIFIER NOT NULL,
    role_name  NVARCHAR(MAX)    NOT NULL,
    password   NVARCHAR(MAX)    NOT NULL,
    created_at DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_admin_tenant_roles PRIMARY KEY (tenant_id),
    CONSTRAINT fk_admin_roles_tenant FOREIGN KEY (tenant_id)
        REFERENCES admin.tenants(tenant_id)
);

-- ===========================================================================
-- Table: dbo.workflow_defs
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND type = N'U')
CREATE TABLE dbo.workflow_defs (
    name            NVARCHAR(255)   NOT NULL,
    version         INT             NOT NULL,
    wasm_bytes      VARBINARY(MAX)  NOT NULL,
    entry_points    NVARCHAR(MAX)   NOT NULL DEFAULT '[]',
    min_version     INT             NOT NULL DEFAULT 0,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    dag_spec        NVARCHAR(MAX)   NULL,
    max_history_length INT         NOT NULL DEFAULT 0,
    tenant_id       UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    task_queue      NVARCHAR(255)   NOT NULL DEFAULT 'default',
    abi_version     INT             NOT NULL DEFAULT 1,
    plugin_deps     NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    deprecated      BIT             NOT NULL DEFAULT 0,
    CONSTRAINT pk_workflow_defs PRIMARY KEY (name, version)
);

-- ===========================================================================
-- Table: dbo.plugin_defs
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.plugin_defs') AND type = N'U')
CREATE TABLE dbo.plugin_defs (
    name         NVARCHAR(255)   NOT NULL,
    version      NVARCHAR(64)    NOT NULL,
    wasm_bytes   VARBINARY(MAX)  NULL,
    config       NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    created_at   DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    deprecated   BIT             NOT NULL DEFAULT 0,
    CONSTRAINT pk_plugin_defs PRIMARY KEY (name, version),
    CONSTRAINT ck_plugin_defs_config CHECK (ISJSON(config) = 1)
);

-- ===========================================================================
-- Table: dbo.workflow_instances
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND type = N'U')
CREATE TABLE dbo.workflow_instances (
    id              NVARCHAR(255)   NOT NULL,
    def_name        NVARCHAR(255)   NOT NULL,
    def_version     INT             NOT NULL,
    status          NVARCHAR(32)    NOT NULL DEFAULT 'ready',
    input           NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    assigned_to     NVARCHAR(255)   NULL,
    heartbeat_at    DATETIMEOFFSET  NULL,
    next_wake_at    DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    completed_at    DATETIMEOFFSET  NULL,
    cancellation_requested BIT     NOT NULL DEFAULT 0,
    cancellation_reason NVARCHAR(MAX) NULL,
    result          NVARCHAR(MAX)   NULL,
    error_msg       NVARCHAR(MAX)   NULL,
    error_code      NVARCHAR(255)   NULL,
    error_op        NVARCHAR(255)   NULL,
    parent_workflow_id NVARCHAR(255) NULL,
    parent_close_policy NVARCHAR(32) NULL DEFAULT 'ABANDON',
    query_state     NVARCHAR(MAX)   NULL DEFAULT '{}',
    trace_id        NVARCHAR(255)   NULL,
    sticky_worker_id NVARCHAR(255)  NULL,
    tenant_id       UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    task_queue      NVARCHAR(255)   NOT NULL DEFAULT 'default',
    compaction_state NVARCHAR(MAX)  NULL,
    compacted_at    DATETIMEOFFSET  NULL,
    compaction_step INT             NULL,
    plugin_vers     NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    event_count     BIGINT          NOT NULL DEFAULT 0,
    allowed_signals NVARCHAR(MAX)   NULL,
    generation      BIGINT          NOT NULL DEFAULT 0,
    priority        INTEGER         NOT NULL DEFAULT 0,
    CONSTRAINT pk_workflow_instances PRIMARY KEY (id),
    CONSTRAINT fk_instances_def FOREIGN KEY (def_name, def_version)
        REFERENCES dbo.workflow_defs(name, version),
    CONSTRAINT ck_workflow_instances_input CHECK (ISJSON(input) = 1),
    CONSTRAINT ck_workflow_instances_result CHECK (result IS NULL OR ISJSON(result) = 1),
    CONSTRAINT ck_workflow_instances_query_state CHECK (query_state IS NULL OR ISJSON(query_state) = 1)
);

-- ===========================================================================
-- Table: dbo.event_history
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.event_history') AND type = N'U')
CREATE TABLE dbo.event_history (
    workflow_id     NVARCHAR(255)   NOT NULL,
    step            INT             NOT NULL,
    service         NVARCHAR(255)   NULL,
    operation       NVARCHAR(255)   NULL,
    request         NVARCHAR(255)   NULL,
    response        NVARCHAR(MAX)   NULL,
    error           NVARCHAR(MAX)   NULL,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    event_type      NVARCHAR(255)   NOT NULL DEFAULT 'call',
    duration_ms     BIGINT          NULL,
    signal_names    NVARCHAR(512)   NULL,
    timeout_ms      BIGINT          NULL,
    signal_name     NVARCHAR(255)   NULL,
    signal_payload  NVARCHAR(MAX)   NULL,
    defer_description NVARCHAR(512) NULL,
    defer_id        NVARCHAR(255)   NULL,
    child_name      NVARCHAR(255)   NULL,
    child_input     NVARCHAR(MAX)   NULL,
    run_id          NVARCHAR(255)   NULL,
    new_input       NVARCHAR(MAX)   NULL,
    plugin_name     NVARCHAR(255)   NULL,
    plugin_func     NVARCHAR(255)   NULL,
    plugin_input    NVARCHAR(MAX)   NULL,
    plugin_output   NVARCHAR(MAX)   NULL,
    plugin_error    NVARCHAR(MAX)   NULL,
    promise_name    NVARCHAR(255)   NULL,
    promise_id      NVARCHAR(255)   NULL,
    promise_result  NVARCHAR(MAX)   NULL,
    promise_error   NVARCHAR(MAX)   NULL,
    tenant_id       UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    payload         NVARCHAR(MAX)   NULL,
    checksum        NVARCHAR(MAX)   NULL,
    thread_id       NVARCHAR(255)   NOT NULL DEFAULT 'main',
    local_step      INT             NOT NULL DEFAULT 0,
    global_seq      BIGINT          NOT NULL DEFAULT 0,
    CONSTRAINT pk_event_history PRIMARY KEY (workflow_id, step),
    CONSTRAINT fk_event_history_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE
);

-- ===========================================================================
-- Table: dbo.workflow_signals
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_signals') AND type = N'U')
CREATE TABLE dbo.workflow_signals (
    workflow_id     NVARCHAR(255)   NOT NULL,
    signal_name     NVARCHAR(255)   NOT NULL,
    payload         NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    delivered_at    DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    tenant_id       UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    CONSTRAINT pk_workflow_signals PRIMARY KEY (workflow_id, signal_name),
    CONSTRAINT fk_signals_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE,
    CONSTRAINT ck_workflow_signals_payload CHECK (ISJSON(payload) = 1)
);

-- ===========================================================================
-- Table: dbo.workflow_promises
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_promises') AND type = N'U')
CREATE TABLE dbo.workflow_promises (
    workflow_id     NVARCHAR(255)   NOT NULL,
    promise_id      NVARCHAR(64)    NOT NULL,
    promise_name    NVARCHAR(255)   NOT NULL,
    tenant_id       NVARCHAR(255)   NOT NULL,
    priority        INTEGER         NOT NULL DEFAULT 0,
    status          NVARCHAR(50)    NOT NULL DEFAULT 'pending',
    result          NVARCHAR(MAX)   NULL,
    error_msg       NVARCHAR(MAX)   NULL,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    resolved_at     DATETIMEOFFSET  NULL,
    CONSTRAINT pk_workflow_promises PRIMARY KEY (workflow_id, promise_id),
    CONSTRAINT fk_promises_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE,
    CONSTRAINT ck_workflow_promises_result CHECK (result IS NULL OR ISJSON(result) = 1)
);

-- ===========================================================================
-- Table: dbo.workflow_schedules
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules') AND type = N'U')
CREATE TABLE dbo.workflow_schedules (
    name            NVARCHAR(255)   NOT NULL,
    def_name        NVARCHAR(255)   NOT NULL,
    entry_point     NVARCHAR(255)   NOT NULL DEFAULT '',
    cron_expression NVARCHAR(255)   NOT NULL,
    input           NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    enabled         BIT             NOT NULL DEFAULT 1,
    next_run_at     DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    last_run_at     DATETIMEOFFSET  NULL,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    tenant_id       UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    CONSTRAINT pk_workflow_schedules PRIMARY KEY (name),
    CONSTRAINT ck_workflow_schedules_input CHECK (ISJSON(input) = 1)
);

-- ===========================================================================
-- Table: dbo.concurrency_keys
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.concurrency_keys') AND type = N'U')
CREATE TABLE dbo.concurrency_keys (
    key_hash        VARBINARY(32)   NOT NULL,
    key_text        NVARCHAR(MAX)   NOT NULL,
    workflow_id     NVARCHAR(255)   NOT NULL,
    acquired_at     DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    expires_at      DATETIMEOFFSET  NOT NULL,
    tenant_id       UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    CONSTRAINT pk_concurrency_keys PRIMARY KEY (key_hash),
    CONSTRAINT fk_concurrency_keys_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE
);

-- ===========================================================================
-- Table: dbo.idempotency_keys
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.idempotency_keys') AND type = N'U')
CREATE TABLE dbo.idempotency_keys (
    key_hash    VARBINARY(32)    NOT NULL,
    workflow_id NVARCHAR(255)    NOT NULL,
    result      NVARCHAR(MAX)    NULL,
    error_msg   NVARCHAR(MAX)    NULL,
    created_at  DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    expires_at  DATETIMEOFFSET   NOT NULL DEFAULT DATEADD(DAY, 7, SYSUTCDATETIME()),
    CONSTRAINT pk_idempotency_keys PRIMARY KEY (key_hash),
    CONSTRAINT ck_idempotency_keys_result CHECK (result IS NULL OR ISJSON(result) = 1)
);

-- ===========================================================================
-- Table: dbo.workflow_update_requests
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_update_requests') AND type = N'U')
CREATE TABLE dbo.workflow_update_requests (
    workflow_id     NVARCHAR(255)   NOT NULL,
    update_name     NVARCHAR(255)   NOT NULL,
    tenant_id       NVARCHAR(36)    NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    priority        INTEGER         NOT NULL DEFAULT 0,
    payload         NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    promise_id      NVARCHAR(MAX)   NULL,
    status          NVARCHAR(50)    NOT NULL DEFAULT 'pending',
    result          NVARCHAR(MAX)   NULL,
    error_msg       NVARCHAR(MAX)   NULL,
    created_at      DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    completed_at    DATETIMEOFFSET  NULL,
    CONSTRAINT pk_workflow_update_requests PRIMARY KEY (workflow_id, update_name),
    CONSTRAINT fk_update_requests_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE,
    CONSTRAINT ck_workflow_update_requests_payload CHECK (ISJSON(payload) = 1)
);

-- ===========================================================================
-- Table: dbo.tenants
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.tenants') AND type = N'U')
CREATE TABLE dbo.tenants (
    tenant_id   UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
    name        NVARCHAR(255)    NOT NULL,
    display_name NVARCHAR(MAX)   NOT NULL DEFAULT '',
    created_at  DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    suspended   BIT              NOT NULL DEFAULT 0,
    CONSTRAINT pk_tenants PRIMARY KEY (tenant_id),
    CONSTRAINT uq_tenants_name UNIQUE (name)
);

-- ===========================================================================
-- Table: dbo.tenant_api_keys
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.tenant_api_keys') AND type = N'U')
CREATE TABLE dbo.tenant_api_keys (
    key_id      UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
    tenant_id   UNIQUEIDENTIFIER NOT NULL,
    key_hash    VARBINARY(32)    NOT NULL,
    description NVARCHAR(MAX)    NOT NULL DEFAULT '',
    created_at  DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    revoked_at  DATETIMEOFFSET   NULL,
    CONSTRAINT pk_tenant_api_keys PRIMARY KEY (key_id),
    CONSTRAINT fk_api_keys_tenant FOREIGN KEY (tenant_id)
        REFERENCES dbo.tenants(tenant_id)
);

-- ===========================================================================
-- Table: dbo.workflow_memory_samples
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_memory_samples') AND type = N'U')
CREATE TABLE dbo.workflow_memory_samples (
    id            BIGINT IDENTITY(1,1) NOT NULL,
    def_name      NVARCHAR(255)        NOT NULL,
    sample_bytes  BIGINT               NOT NULL,
    recorded_at   DATETIMEOFFSET       NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_workflow_memory_samples PRIMARY KEY (id)
);

-- ===========================================================================
-- Table: dbo.workflow_memory_stats
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_memory_stats') AND type = N'U')
CREATE TABLE dbo.workflow_memory_stats (
    def_name      NVARCHAR(255)  NOT NULL,
    mean_bytes    FLOAT(53)      NOT NULL DEFAULT 0,
    sample_count  INT            NOT NULL DEFAULT 0,
    alpha         FLOAT(53)      NOT NULL DEFAULT 0.3,
    updated_at    DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_workflow_memory_stats PRIMARY KEY (def_name)
);

-- ===========================================================================
-- Table: dbo.workflow_tags (deployment tags for workflow versioning)
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_tags') AND type = N'U')
CREATE TABLE dbo.workflow_tags (
    workflow_name NVARCHAR(255)   NOT NULL,
    version       INT             NOT NULL,
    tag           NVARCHAR(255)   NOT NULL,
    canary_weight INT             NOT NULL DEFAULT 0,
    created_at    DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    tenant_id     UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    CONSTRAINT pk_workflow_tags PRIMARY KEY (workflow_name, tag),
    CONSTRAINT fk_workflow_tags_def FOREIGN KEY (workflow_name, version)
        REFERENCES dbo.workflow_defs(name, version)
);

-- ===========================================================================
-- Table: dbo.workflow_routing (traffic routing for workflow versioning)
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_routing') AND type = N'U')
CREATE TABLE dbo.workflow_routing (
    id              UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
    workflow_name   NVARCHAR(255)    NOT NULL,
    target_version  INT              NOT NULL,
    weight          FLOAT(53)        NOT NULL DEFAULT 1.0,
    created_at      DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    tenant_id       UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    CONSTRAINT pk_workflow_routing PRIMARY KEY (id),
    CONSTRAINT fk_workflow_routing_def FOREIGN KEY (workflow_name, target_version)
        REFERENCES dbo.workflow_defs(name, version),
    CONSTRAINT ck_workflow_routing_weight CHECK (weight >= 0 AND weight <= 1)
);

-- ===========================================================================
-- Row-Level Security: SECURITY POLICY using fn_tenant_filter
-- Applies FILTER PREDICATE on every tenant-scoped table.
-- Requires sp_set_session_context 'tenant_id' on each connection.
-- ===========================================================================

-- Drop existing policies (idempotent: no-op if they do not exist).
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Defs')
    DROP SECURITY POLICY dbo.TenantFilter_Defs;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Instances')
    DROP SECURITY POLICY dbo.TenantFilter_Instances;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_EventHistory')
    DROP SECURITY POLICY dbo.TenantFilter_EventHistory;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Signals')
    DROP SECURITY POLICY dbo.TenantFilter_Signals;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Schedules')
    DROP SECURITY POLICY dbo.TenantFilter_Schedules;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Tags')
    DROP SECURITY POLICY dbo.TenantFilter_Tags;
IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Routing')
    DROP SECURITY POLICY dbo.TenantFilter_Routing;

-- Recreate security policies on all tenant-scoped tables.
GO
CREATE SECURITY POLICY dbo.TenantFilter_Defs
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_defs
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Instances
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_instances
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_EventHistory
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.event_history
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Signals
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_signals
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Schedules
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_schedules
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Tags
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_tags
    WITH (STATE = ON);

GO
CREATE SECURITY POLICY dbo.TenantFilter_Routing
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_routing
    WITH (STATE = ON);

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

-- Tenant + task-queue-scoped ready instance claims (includes priority)
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_queue_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_tenant_queue_ready
        ON dbo.workflow_instances(tenant_id, task_queue, status, priority ASC, next_wake_at)
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

-- ClaimWorkflows ORDER BY: covers tenant+task_queue filter and priority+created_at sort
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_workflow_instances_ready_claim' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_workflow_instances_ready_claim
        ON dbo.workflow_instances (tenant_id, task_queue, priority ASC, created_at)
        WHERE status = 'ready';

-- Canary weight migration (idempotent)
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_tags') AND name = N'canary_weight')
    ALTER TABLE dbo.workflow_tags ADD canary_weight INT NOT NULL DEFAULT 0;
GO
