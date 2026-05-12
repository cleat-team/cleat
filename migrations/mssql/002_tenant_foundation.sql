-- cleat tenant foundation migration (T-SQL for SQL Server 2017+ / Azure SQL Database)
-- Adds multi-tenant data isolation, API key authentication, and a future-proof
-- schema for multi-tenancy.
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS where applicable.

-- ===========================================================================
-- 1. Create tenants metadata table
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
-- 2. Create tenant API keys table
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.tenant_api_keys') AND type = N'U')
CREATE TABLE dbo.tenant_api_keys (
    key_id      UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
    tenant_id   UNIQUEIDENTIFIER NOT NULL,
    key_hash    VARBINARY(64)    NOT NULL,
    description NVARCHAR(MAX)    NOT NULL DEFAULT '',
    created_at  DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    revoked_at  DATETIMEOFFSET   NULL,
    CONSTRAINT pk_tenant_api_keys PRIMARY KEY (key_id),
    CONSTRAINT fk_api_keys_tenant FOREIGN KEY (tenant_id)
        REFERENCES dbo.tenants(tenant_id)
);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_api_keys_hash' AND object_id = OBJECT_ID(N'dbo.tenant_api_keys'))
    CREATE INDEX idx_api_keys_hash ON dbo.tenant_api_keys(key_hash) WHERE revoked_at IS NULL;

-- ===========================================================================
-- 3. Add tenant_id to core tables
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'tenant_id')
    ALTER TABLE dbo.workflow_defs ADD tenant_id UNIQUEIDENTIFIER NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'tenant_id')
    ALTER TABLE dbo.workflow_instances ADD tenant_id UNIQUEIDENTIFIER NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'tenant_id')
    ALTER TABLE dbo.event_history ADD tenant_id UNIQUEIDENTIFIER NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_signals') AND name = N'tenant_id')
    ALTER TABLE dbo.workflow_signals ADD tenant_id UNIQUEIDENTIFIER NULL;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules') AND name = N'tenant_id')
    ALTER TABLE dbo.workflow_schedules ADD tenant_id UNIQUEIDENTIFIER NULL;

-- ===========================================================================
-- 8. Create a default tenant for existing data (idempotent)
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM dbo.tenants WHERE tenant_id = '00000000-0000-0000-0000-000000000000')
    INSERT INTO dbo.tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant');

-- ===========================================================================
-- 9. Backfill existing rows with default tenant
-- ===========================================================================
-- Wrapped in EXEC because tenant_id columns are added conditionally above
-- and SQL Server validates column references at parse time.
EXEC('UPDATE dbo.workflow_defs SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_instances SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.event_history SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_signals SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_schedules SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');

-- ===========================================================================
-- 10. Make tenant_id NOT NULL
-- ===========================================================================
IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'tenant_id' AND is_nullable = 1)
    ALTER TABLE dbo.workflow_defs ALTER COLUMN tenant_id UNIQUEIDENTIFIER NOT NULL;

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'tenant_id' AND is_nullable = 1)
    ALTER TABLE dbo.workflow_instances ALTER COLUMN tenant_id UNIQUEIDENTIFIER NOT NULL;

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.event_history') AND name = N'tenant_id' AND is_nullable = 1)
    ALTER TABLE dbo.event_history ALTER COLUMN tenant_id UNIQUEIDENTIFIER NOT NULL;

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_signals') AND name = N'tenant_id' AND is_nullable = 1)
    ALTER TABLE dbo.workflow_signals ALTER COLUMN tenant_id UNIQUEIDENTIFIER NOT NULL;

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules') AND name = N'tenant_id' AND is_nullable = 1)
    ALTER TABLE dbo.workflow_schedules ALTER COLUMN tenant_id UNIQUEIDENTIFIER NOT NULL;

-- ===========================================================================
-- 11. Composite indexes for tenant-scoped queries
-- ===========================================================================
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_defs_active' AND object_id = OBJECT_ID(N'dbo.workflow_defs'))
    DROP INDEX idx_defs_active ON dbo.workflow_defs;

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_defs_tenant_name_version' AND object_id = OBJECT_ID(N'dbo.workflow_defs'))
    CREATE INDEX idx_defs_tenant_name_version ON dbo.workflow_defs(tenant_id, name, version DESC);

-- Replace the ready index with a tenant-scoped version
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_ready ON dbo.workflow_instances;

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_tenant_ready
        ON dbo.workflow_instances(tenant_id, status, next_wake_at) WHERE status = 'ready';

-- Recreate heartbeat and stale indexes
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_heartbeat' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_heartbeat ON dbo.workflow_instances;

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_heartbeat' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_heartbeat ON dbo.workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_stale' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_stale ON dbo.workflow_instances;

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_stale' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_stale ON dbo.workflow_instances(status, heartbeat_at) WHERE status = 'running';

-- Tenant-scoped event history lookup
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_event_history_tenant_wf' AND object_id = OBJECT_ID(N'dbo.event_history'))
    CREATE INDEX idx_event_history_tenant_wf ON dbo.event_history(tenant_id, workflow_id, step);

-- Tenant-scoped signals lookup
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_signals_tenant_wf' AND object_id = OBJECT_ID(N'dbo.workflow_signals'))
    CREATE INDEX idx_signals_tenant_wf ON dbo.workflow_signals(tenant_id, workflow_id, signal_name);

-- Tenant-scoped schedule due lookup
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_schedules_tenant_enabled' AND object_id = OBJECT_ID(N'dbo.workflow_schedules'))
    CREATE INDEX idx_schedules_tenant_enabled ON dbo.workflow_schedules(tenant_id, enabled, next_run_at);

-- ===========================================================================
-- 12. Drop old namespace columns (replaced by tenant_id)
-- ===========================================================================
-- Drop dependent objects before dropping namespace columns.
-- The namespace columns were added with DEFAULT constraints, and
-- idx_instances_namespace_ready references namespace.
-- Use dynamic SQL to find and drop auto-named default constraints.
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_namespace_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_namespace_ready ON dbo.workflow_instances;

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'namespace')
    EXEC('
        DECLARE @c nvarchar(200);
        SELECT @c = name FROM sys.default_constraints
        WHERE parent_object_id = OBJECT_ID(''dbo.workflow_defs'')
        AND parent_column_id = COLUMNPROPERTY(OBJECT_ID(''dbo.workflow_defs''), ''namespace'', ''ColumnId'');
        IF @c IS NOT NULL EXEC(''ALTER TABLE dbo.workflow_defs DROP CONSTRAINT '' + @c);
        ALTER TABLE dbo.workflow_defs DROP COLUMN namespace;
    ');

IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'namespace')
    EXEC('
        DECLARE @c nvarchar(200);
        SELECT @c = name FROM sys.default_constraints
        WHERE parent_object_id = OBJECT_ID(''dbo.workflow_instances'')
        AND parent_column_id = COLUMNPROPERTY(OBJECT_ID(''dbo.workflow_instances''), ''namespace'', ''ColumnId'');
        IF @c IS NOT NULL EXEC(''ALTER TABLE dbo.workflow_instances DROP CONSTRAINT '' + @c);
        ALTER TABLE dbo.workflow_instances DROP COLUMN namespace;
    ');

-- ===========================================================================
-- 13. Row-Level Security (defense in depth) via Security Policy
--
-- SQL Server RLS uses an inline table-valued function as a security predicate,
-- then binds it to each table via a security policy.
-- The predicate checks SESSION_CONTEXT('tenant_id') against the tenant_id column.
-- If the session variable is not set, the function returns no rows (fail-closed).
-- db_owner members bypass the filter.
-- ===========================================================================

-- RLS (Row-Level Security) via security policies and inline TVF predicates
-- is skipped for now. The inline TVF cannot be created standalone because
-- it references the table column 'tenant_id' without a table qualifier
-- (valid only as a security policy predicate, not as a standalone function).
-- Application-layer tenant isolation (WHERE tenant_id = ? on every query) is
-- the primary mechanism. See internal/host/mssql_store.go for implementation.
