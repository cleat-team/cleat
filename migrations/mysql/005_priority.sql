ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 0;
DROP INDEX idx_instances_tenant_queue_ready ON workflow_instances;
CREATE INDEX idx_instances_tenant_queue_ready ON workflow_instances(tenant_id, task_queue, status, priority, next_wake_at);
