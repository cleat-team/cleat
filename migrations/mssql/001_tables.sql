-- cleat mssql tables (consolidated)
-- All CREATE TABLE statements with final columns compiled from migrations 001-011, 013.
-- Idempotent: all statements use IF NOT EXISTS guards.
-- Target: fresh installs only.

-- ===========================================================================
-- Schema: admin
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.schemas WHERE name = N'admin')
    EXEC('CREATE SCHEMA admin');

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
        REFERENCES dbo.workflow_instances(id)
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
        REFERENCES dbo.workflow_instances(id),
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
    CONSTRAINT pk_concurrency_keys PRIMARY KEY (key_hash),
    CONSTRAINT fk_concurrency_keys_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id)
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
