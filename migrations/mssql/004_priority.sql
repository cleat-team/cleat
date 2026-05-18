ALTER TABLE dbo.workflow_instances ADD priority INTEGER NOT NULL CONSTRAINT DF_instances_priority DEFAULT 0 WITH VALUES;
GO
DROP INDEX IF EXISTS idx_instances_tenant_queue_ready ON dbo.workflow_instances;
GO
CREATE INDEX idx_instances_tenant_queue_ready ON dbo.workflow_instances(tenant_id, task_queue, status, priority ASC, next_wake_at) WHERE status = 'ready';
GO
