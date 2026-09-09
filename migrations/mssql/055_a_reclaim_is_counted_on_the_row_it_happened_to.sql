-- cleat migration 055 (mssql): a reclaim is counted on the row it happened to
--
-- RENUMBERED from 054 to 055. It shipped as 054 in #1055 and another
-- file already held that number on develop -- the runner parses the numeric
-- prefix into an int and keys schema_migrations on it, so the two collapsed to
-- one version. A database that had already recorded 054 from the other file
-- skipped this one FOREVER, silently, with the run reporting ok. Safe to apply
-- twice: every statement below is guarded, so a database that did get this as
-- 054 re-applies it as 055 and changes nothing.
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
