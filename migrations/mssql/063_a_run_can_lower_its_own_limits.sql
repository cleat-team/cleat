-- cleat migration 063 (mssql): a run can lower its own limits
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

IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID('dbo.workflow_instances')
                 AND name = 'run_wasm_instance_timeout_ms')
BEGIN
    ALTER TABLE dbo.workflow_instances
        ADD run_wasm_instance_timeout_ms BIGINT NULL,
            run_wasm_wall_clock_ceiling_ms BIGINT NULL,
            run_host_retry_budget_ms BIGINT NULL;
END
GO
IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = 'ck_wi_run_instance_timeout_positive')
    ALTER TABLE dbo.workflow_instances ADD CONSTRAINT ck_wi_run_instance_timeout_positive
        CHECK (run_wasm_instance_timeout_ms IS NULL OR run_wasm_instance_timeout_ms > 0);
GO
IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = 'ck_wi_run_wall_clock_positive')
    ALTER TABLE dbo.workflow_instances ADD CONSTRAINT ck_wi_run_wall_clock_positive
        CHECK (run_wasm_wall_clock_ceiling_ms IS NULL OR run_wasm_wall_clock_ceiling_ms > 0);
GO
IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = 'ck_wi_run_retry_budget_positive')
    ALTER TABLE dbo.workflow_instances ADD CONSTRAINT ck_wi_run_retry_budget_positive
        CHECK (run_host_retry_budget_ms IS NULL OR run_host_retry_budget_ms > 0);
GO
