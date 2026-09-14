-- cleat migration 070 (mssql): a workflow timeout is per-tenant and per-run
--
-- cleat#1117. See the postgres migration of the same name for the full rationale: a worker
-- process is not a tenancy boundary, so --max-workflow-duration applied one
-- tenant's operational policy to another's workflows. This is the SQL Server
-- half.
--
-- SCOPE: despite "workflow" in the name this bounds ONE EXECUTION SEGMENT, not
-- a workflow's whole lifetime -- pre-existing behaviour of the flag, unchanged
-- by cleat#1117, which is about who may set the bound rather than what it
-- spans. See the postgres migration of the same name.
--
-- NULL means "no override, resolve to the tier above"; the CHECK forbids
-- non-positive, because zero is how ClampToCeiling spells "unset" and a stored
-- 0 would be indistinguishable from "unbounded".
--
-- Column adds and constraint adds are separate batches, as in 063: a
-- constraint referring to a column added in the same batch does not compile,
-- because the batch is parsed before any of it runs.

IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID('dbo.tenant_settings')
                 AND name = 'max_workflow_duration_ms')
BEGIN
    ALTER TABLE dbo.tenant_settings
        ADD max_workflow_duration_ms BIGINT NULL;
END
GO
IF NOT EXISTS (SELECT 1 FROM sys.columns
               WHERE object_id = OBJECT_ID('dbo.workflow_instances')
                 AND name = 'run_max_workflow_duration_ms')
BEGIN
    ALTER TABLE dbo.workflow_instances
        ADD run_max_workflow_duration_ms BIGINT NULL;
END
GO
IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = 'ck_ts_max_workflow_duration_positive')
    ALTER TABLE dbo.tenant_settings ADD CONSTRAINT ck_ts_max_workflow_duration_positive
        CHECK (max_workflow_duration_ms IS NULL OR max_workflow_duration_ms > 0);
GO
IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = 'ck_wi_run_max_workflow_duration_positive')
    ALTER TABLE dbo.workflow_instances ADD CONSTRAINT ck_wi_run_max_workflow_duration_positive
        CHECK (run_max_workflow_duration_ms IS NULL OR run_max_workflow_duration_ms > 0);
GO
