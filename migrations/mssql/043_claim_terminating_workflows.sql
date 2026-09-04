-- ===========================================================================
-- 043: SQL Server's claim indexes, widened to match the claim
--
-- IMPROVEMENT-PLAN 3.75 step 2, decision D6. The claim now accepts
-- 'terminating' as well as 'ready' (engine/mssql_lifecycle.go), and every
-- filtered index SQL Server has for finding runnable work is filtered on
-- `status = 'ready'` -- 001_schema.sql:515 and :561, and 035's two. A filtered
-- index is only usable when the query's predicate implies its filter, and
-- `status IN ('ready','terminating')` does not imply `status = 'ready'`, so
-- without this the dispatch loop's claim loses every index it has.
--
-- See migrations/postgres/040 for why the predicate is widened rather than a
-- second filtered index added per pair, and why the new indexes are created
-- before the old ones are dropped.
--
-- SQL Server has no CREATE INDEX ... IF NOT EXISTS, so each statement carries
-- the sys.indexes guard this schema uses everywhere for idempotency.
-- ===========================================================================

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_claimable' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_claimable
        ON dbo.workflow_instances(status, next_wake_at)
        WHERE status IN ('ready', 'terminating');

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_claimable' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_tenant_claimable
        ON dbo.workflow_instances(tenant_id, status, next_wake_at)
        WHERE status IN ('ready', 'terminating');

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_queue_claimable' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_tenant_queue_claimable
        ON dbo.workflow_instances(tenant_id, task_queue, status, priority ASC, next_wake_at)
        WHERE status IN ('ready', 'terminating');

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_claim_order' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_claim_order
        ON dbo.workflow_instances (tenant_id, task_queue, priority ASC, created_at)
        WHERE status IN ('ready', 'terminating');

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_ready ON dbo.workflow_instances;

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_tenant_ready ON dbo.workflow_instances;

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_queue_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_tenant_queue_ready ON dbo.workflow_instances;

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_workflow_instances_ready_claim' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_workflow_instances_ready_claim ON dbo.workflow_instances;
