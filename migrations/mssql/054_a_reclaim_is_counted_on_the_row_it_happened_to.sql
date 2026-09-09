-- cleat migration 054 (mssql): a reclaim is counted on the row it happened to
--
-- cleat#1008. See migrations/postgres/051 for the full reasoning: why
-- `generation` cannot serve (it counts claims, not reclaims, and a healthy
-- workflow reaches 12), and why this column is deliberately NOT a bound -- the
-- causes of repeated reclaim that survive the worker's default limits are all
-- infrastructure, and dead-lettering past a threshold would convert a node
-- redeploy into permanent workflow failure.

IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID(N'dbo.workflow_instances')
      AND name = N'reclaim_count'
)
    ALTER TABLE dbo.workflow_instances ADD reclaim_count BIGINT NOT NULL DEFAULT 0;
GO
