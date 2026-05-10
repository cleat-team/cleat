-- cleat task routing migration
-- Adds task_queue columns for routing workflow types to different worker pools
-- (e.g., GPU workers, high-memory workers).
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS where applicable.

-- Add task_queue to workflow_defs
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS task_queue TEXT NOT NULL DEFAULT 'default';

-- Add task_queue to workflow_instances
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS task_queue TEXT NOT NULL DEFAULT 'default';

-- Drop old ready index if it still exists, create new one with task_queue
DROP INDEX IF EXISTS idx_instances_ready;
DROP INDEX IF EXISTS idx_instances_tenant_ready;
CREATE INDEX IF NOT EXISTS idx_instances_tenant_queue_ready
    ON workflow_instances(tenant_id, task_queue, status, next_wake_at)
    WHERE status = 'ready';
