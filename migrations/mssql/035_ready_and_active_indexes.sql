-- cleat migration 035 (mssql): add idx_instances_ready, idx_defs_active,
-- idx_instances_tenant_ready for dialect parity with PostgreSQL/MySQL
--
-- Finding S10. Re-derived which indexes each dialect actually ships (not
-- trusted from the review) with:
--
--   grep -n 'CREATE.*INDEX.*(workflow_instances\|workflow_defs)' migrations/{postgres,mysql,mssql}/*.sql
--
-- PostgreSQL's 001_schema.sql carries all three (idx_instances_ready on
-- (status, next_wake_at) WHERE status='ready'; idx_defs_active on
-- (name, version DESC); idx_instances_tenant_ready on
-- (tenant_id, status, next_wake_at) WHERE status='ready'). MySQL carries all
-- three too, unfiltered (no partial-index support). SQL Server's
-- 001_schema.sql has neither name anywhere -- confirmed absent, not merely
-- undocumented.
--
-- SQL Server supports filtered indexes (WHERE on CREATE INDEX), so the
-- partial shape translates directly; this migration uses it for the two
-- indexes that carry one on the other dialects.
--
-- What actually happens once these exist, checked with SET SHOWPLAN_ALL ON
-- against a live SQL Server seeded with 3,000 workflow_instances rows / 50
-- workflow_defs rows and fresh statistics (UPDATE STATISTICS), one result per
-- index:
--
--   * idx_instances_tenant_ready: the query the RLS-scoped read actually
--     runs, `WHERE tenant_id = @p AND status = 'ready' AND next_wake_at <=
--     SYSUTCDATETIME()`, gets a plain Index Seek on this index with no key
--     lookup -- tenant_id is a leading key column and id (the clustering
--     key) rides along for free, so the index alone answers the query and
--     the mandatory RLS residual predicate
--     (tenant_id = SESSION_CONTEXT(...) OR IS_ROLEMEMBER('cleat_admin')=1)
--     both. This one is genuinely load-bearing.
--
--   * idx_defs_active: the production query
--     (engine/mssql_deployment.go: `SELECT ... FROM workflow_defs WHERE
--     name = @p1 ORDER BY version DESC`) does NOT use it. pk_workflow_defs
--     is PRIMARY KEY (name, version) and SQL Server clusters a PK by default
--     unless told NONCLUSTERED (verified: `SELECT name, type_desc FROM
--     sys.indexes WHERE object_id = OBJECT_ID('dbo.workflow_defs')` shows
--     pk_workflow_defs as CLUSTERED), so the optimizer answers this exact
--     shape with a backward Clustered Index Seek on the PK alone -- adding
--     idx_defs_active is redundant for the one query this table's own code
--     runs against it. Kept anyway for the stated goal (dialect parity: the
--     other two dialects ship it, and MySQL's InnoDB PK is clustered the
--     same way SQL Server's is, so it is arguably just as redundant there) and
--     because a future query shape (a multi-name IN-list scan, a covering
--     index avoiding the wasm_bytes/entry_points/dag_spec columns that ride
--     along on the clustered PK) could use it even though none does today.
--
--   * idx_instances_ready ((status, next_wake_at) WHERE status='ready', no
--     tenant_id column): also NOT chosen naturally for the equivalent
--     unscoped query, even with the index already present and statistics
--     current. The mandatory RLS predicate needs tenant_id on every row, and
--     this index does not carry it, so satisfying the predicate through it
--     would need a Key Lookup back to the clustered index per row; the
--     optimizer prefers the wider idx_instances_tenant_queue_ready (which
--     already has tenant_id), scanning it instead. Forcing this index with
--     WITH (INDEX(idx_instances_ready)) shows the plan the optimizer
--     rejected: Index Seek on idx_instances_ready + Key Lookup (LOOKUP) on
--     pk_workflow_instances per row to evaluate the RLS predicate -- valid,
--     but strictly more expensive than the plan chosen without the hint.
--     Kept for parity and for any future MSSQL code path that runs
--     unscoped (e.g. as a role member where IS_ROLEMEMBER('cleat_admin')=1
--     short-circuits the OR without needing tenant_id at all, a shape not
--     tested here).
--
-- Idempotent, matching the guard style the rest of migrations/mssql/ uses.

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_ready ON dbo.workflow_instances(status, next_wake_at) WHERE status = 'ready';
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_defs_active' AND object_id = OBJECT_ID(N'dbo.workflow_defs'))
    CREATE INDEX idx_defs_active ON dbo.workflow_defs(name, version DESC);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_instances_tenant_ready' AND object_id = OBJECT_ID(N'dbo.workflow_instances'))
    CREATE INDEX idx_instances_tenant_ready ON dbo.workflow_instances(tenant_id, status, next_wake_at) WHERE status = 'ready';
GO
