-- cleat migration 033 (mssql): indexes for completed-workflow retention and
-- the ListWorkflows Status filter.
--
-- UNVERIFIED. No SQL Server instance was available in this session (no
-- CLEAT_TEST_MSSQL DSN was assigned); this file was written to match the
-- shape 001_schema.sql already uses for this exact table (same guard idiom,
-- same filtered-index syntax it already relies on at line ~549) rather than
-- run and checked. Treat it as unverified until it has been applied against
-- a real SQL Server and exercised by engine/mssql_store_integration_test.go.
--
-- Companion to migrations/postgres/033_completed_workflow_retention_indexes.sql
-- and migrations/mysql/033_completed_workflow_retention_indexes.sql -- see
-- the PostgreSQL file for the full query-shape reasoning; this one covers
-- only what differs on this dialect.
--
-- 1. Retention (DeleteCompletedWorkflows, status IN ('done','failed','terminated')):
--    001_schema.sql declares
--    `idx_instances_terminal_completed ON dbo.workflow_instances(tenant_id, status, completed_at)
--     WHERE status IN ('done','failed')`, a SQL Server filtered index -- same
--    shape as PostgreSQL's partial index, same problem: a filtered index only
--    serves a query whose predicate is a provable subset of the index's own
--    filter, so the existing 2-status filter cannot serve
--    DeleteCompletedWorkflows' 3-status query ('terminated' added). Widened
--    below to match the PostgreSQL migration's reasoning exactly.
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_terminal_completed' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    DROP INDEX idx_instances_terminal_completed ON dbo.workflow_instances;

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_terminal_completed' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_terminal_completed ON dbo.workflow_instances(tenant_id, status, completed_at)
        WHERE status IN ('done', 'failed', 'terminated');

-- 2. ListWorkflows' Status filter (tenant_id = @p1 AND status = @p2 ORDER BY
--    created_at DESC): idx_instances_created_at is (tenant_id, created_at)
--    with no status column, so a status-filtered list still walks the
--    tenant's full created_at order filtering status row by row.
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_status_created' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_tenant_status_created ON dbo.workflow_instances(tenant_id, status, created_at DESC);

-- 3. Substring-search index on error_msg (ErrorContains filter): deliberately
--    not added. SQL Server's LIKE '%text%' is equally unindexable by a plain
--    B-tree as the other two dialects' case-insensitive substring match, and
--    SQL Server's only index type built for text search (a FULLTEXT index)
--    does word/phrase matching, not arbitrary substring matching -- the same
--    semantic mismatch migrations/mysql/033 declines for the same reason.
--    Left unindexed.
--
-- 4. InputContains / Search's input,result branches: same reasoning as the
--    other two dialect migrations -- unindexable free-text search over a
--    CAST(... AS NVARCHAR(MAX)) rendering of JSON, and the general Search
--    filter cannot benefit from indexing one branch while another remains a
--    table scan. Not added here either.
