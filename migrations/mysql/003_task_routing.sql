-- cleat MySQL migration 003: task routing
-- Adds task_queue columns for routing workflow types to different worker pools
-- (e.g., GPU workers, high-memory workers).
--
-- MySQL differences from PostgreSQL:
--   - ADD COLUMN IF NOT EXISTS becomes ADD COLUMN with safety comment
--   - DROP INDEX IF EXISTS not supported; ensure index exists before dropping
--   - Partial index (WHERE) omitted -- MySQL does not support them
--   - Idempotent: no IF NOT EXISTS on ALTER TABLE ADD COLUMN; verify column absence

-- Add task_queue to workflow_defs
-- ensure column does not exist before running
ALTER TABLE workflow_defs ADD COLUMN task_queue VARCHAR(255) NOT NULL DEFAULT 'default';

-- Add task_queue to workflow_instances
-- ensure column does not exist before running
ALTER TABLE workflow_instances ADD COLUMN task_queue VARCHAR(255) NOT NULL DEFAULT 'default';

-- Drop old ready indexes if they exist, create new one with task_queue
-- MySQL does not support IF EXISTS for DROP INDEX.
-- Ensure these indexes exist before running.
DROP INDEX idx_instances_ready ON workflow_instances;
DROP INDEX idx_instances_tenant_ready ON workflow_instances;

-- Note: Partial index WHERE status = 'ready' omitted. Application-level
-- filtering for status = 'ready' must be added to queries.
CREATE INDEX idx_instances_tenant_queue_ready ON workflow_instances(tenant_id, task_queue, status, next_wake_at);
