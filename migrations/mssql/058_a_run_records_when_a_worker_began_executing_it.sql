-- workflow_instances.started_at: when a worker first began executing this run.
--
-- cleat-team/cleat#1090. See migrations/postgres/055 for the reasoning; it is
-- the same column for the same reason on every dialect.
--
-- DATETIME2 to match this dialect's other timestamps -- heartbeat_at is stamped
-- with SYSUTCDATETIME() at claim, and DATETIME2 is what holds that without
-- rounding.
--
-- No backfill: a run claimed before this migration has no knowable start, and
-- NULL says "not recorded", which is true.
IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID('dbo.workflow_instances')
      AND name = 'started_at'
)
BEGIN
    ALTER TABLE workflow_instances ADD started_at DATETIME2 NULL;
END
