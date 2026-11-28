-- cleat#4275: a run held in memory through a wait records the generation it is held at. See migrations/postgres/012 for the full reasoning; this is the same change on
-- workflow_instances, which is where SQL Server keeps the lease columns.
--
-- Idempotent: the column is added only when COL_LENGTH says it is absent.
--
-- No index: the held arm of the reclaim keeps the ordinary `heartbeat_at < DATEADD(...)` range predicate at the shorter interval, which the existing status/heartbeat_at index serves;
-- held_generation is filtered on the rows that range returns.
IF COL_LENGTH(N'dbo.workflow_instances', N'held_generation') IS NULL
    ALTER TABLE dbo.workflow_instances ADD held_generation BIGINT NULL;
