SET @s = IF((SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances' AND COLUMN_NAME = 'priority') = 0, 'ALTER TABLE workflow_instances ADD COLUMN priority INTEGER NOT NULL DEFAULT 0', 'SELECT 1');
PREPARE stmt FROM @s;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
DROP INDEX idx_instances_tenant_queue_ready ON workflow_instances;
CREATE INDEX idx_instances_tenant_queue_ready ON workflow_instances(tenant_id, task_queue, status, priority, next_wake_at);
