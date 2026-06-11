-- 008_update_requests_tenant_id.sql: Add tenant_id column to workflow_update_requests
-- for tenant-scoped filtering (consistent with Postgres schema).

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE name = N'tenant_id' AND object_id = OBJECT_ID(N'dbo.workflow_update_requests'))
    ALTER TABLE dbo.workflow_update_requests ADD tenant_id NVARCHAR(36) NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';
