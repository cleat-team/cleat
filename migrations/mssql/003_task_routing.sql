-- cleat task routing migration (T-SQL for SQL Server 2017+ / Azure SQL Database)
-- Adds task_queue columns for routing workflow types to different worker pools
-- (e.g., GPU workers, high-memory workers).
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS where applicable.

-- ===========================================================================
-- Add task_queue to workflow_defs
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'task_queue')
    ALTER TABLE dbo.workflow_defs ADD task_queue NVARCHAR(MAX) NOT NULL DEFAULT 'default';

-- ===========================================================================
-- Add task_queue to workflow_instances
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'task_queue')
    ALTER TABLE dbo.workflow_instances ADD task_queue NVARCHAR(MAX) NOT NULL DEFAULT 'default';

-- ===========================================================================
-- Drop old ready indexes and create new one with task_queue
-- ===========================================================================
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_ready ON dbo.workflow_instances;

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_tenant_ready ON dbo.workflow_instances;

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_queue_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_tenant_queue_ready
        ON dbo.workflow_instances(tenant_id, task_queue, status, next_wake_at)
        WHERE status = 'ready';
