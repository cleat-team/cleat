-- cleat migration 066 (mysql): a workflow timeout is per-tenant and per-run
--
-- cleat#1117. See the postgres migration of the same name for the full rationale: a worker
-- process is not a tenancy boundary, so --max-workflow-duration applied one
-- tenant's operational policy to another's workflows. This is the MySQL half.
--
-- SCOPE: despite "workflow" in the name this bounds ONE EXECUTION SEGMENT, not
-- a workflow's whole lifetime -- pre-existing behaviour of the flag, unchanged
-- by cleat#1117, which is about who may set the bound rather than what it
-- spans. See the postgres migration of the same name.
--
-- NULL means "no override, resolve to the tier above"; the CHECK forbids
-- non-positive, because zero is how ClampToCeiling spells "unset" and a stored
-- 0 would be indistinguishable from "unbounded" -- the one direction the clamp
-- exists to refuse.
--
-- Guarded on the column's absence the same way 060 is: MySQL has no
-- ADD COLUMN IF NOT EXISTS, so the check is a prepared statement over
-- information_schema. Each table is guarded separately, because they can
-- legitimately be at different states on a database that half-applied.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenant_settings'
               AND COLUMN_NAME = 'max_workflow_duration_ms');
SET @sql := IF(@col = 0,
    'ALTER TABLE tenant_settings
        ADD COLUMN max_workflow_duration_ms BIGINT NULL,
        ADD CONSTRAINT ck_ts_max_workflow_duration_positive CHECK (max_workflow_duration_ms IS NULL OR max_workflow_duration_ms > 0)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances'
               AND COLUMN_NAME = 'run_max_workflow_duration_ms');
SET @sql := IF(@col = 0,
    'ALTER TABLE workflow_instances
        ADD COLUMN run_max_workflow_duration_ms BIGINT NULL,
        ADD CONSTRAINT ck_wi_run_max_workflow_duration_positive CHECK (run_max_workflow_duration_ms IS NULL OR run_max_workflow_duration_ms > 0)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
