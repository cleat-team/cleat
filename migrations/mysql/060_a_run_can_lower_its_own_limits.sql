-- cleat migration 060 (mysql): a run can lower its own limits
--
-- cleat#1187. The per-run tier of the three bounds tenant_settings (039)
-- already lets a TENANT tighten. Columns rather than a request field because
-- the value must survive re-claim and replay: a run whose worker dies is
-- claimed again, and a durable execution replays its whole history, so an
-- override living only in the start request would silently change a running
-- workflow's limits the first time either happened.
--
-- NULLABLE is load-bearing. NULL means "no override, use the tier above"; a
-- value means "at most this". Zero cannot serve for either, because zero is how
-- ClampToCeiling already spells "unset" -- so a 0 stored for "no override"
-- would be indistinguishable from a 0 meaning "unbounded", and the second is
-- the escalation the clamp exists to prevent. That is what the CHECKs forbid:
-- not bad input, but the one shape clamping cannot save us from.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances'
               AND COLUMN_NAME = 'run_wasm_instance_timeout_ms');
SET @sql := IF(@col = 0,
    'ALTER TABLE workflow_instances
        ADD COLUMN run_wasm_instance_timeout_ms BIGINT NULL,
        ADD COLUMN run_wasm_wall_clock_ceiling_ms BIGINT NULL,
        ADD COLUMN run_host_retry_budget_ms BIGINT NULL,
        ADD CONSTRAINT ck_wi_run_instance_timeout_positive CHECK (run_wasm_instance_timeout_ms IS NULL OR run_wasm_instance_timeout_ms > 0),
        ADD CONSTRAINT ck_wi_run_wall_clock_positive CHECK (run_wasm_wall_clock_ceiling_ms IS NULL OR run_wasm_wall_clock_ceiling_ms > 0),
        ADD CONSTRAINT ck_wi_run_retry_budget_positive CHECK (run_host_retry_budget_ms IS NULL OR run_host_retry_budget_ms > 0)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
