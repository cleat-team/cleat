-- cleat migration 038 (mssql): put the tenant in the workflow-definition key
--
-- D7, decided 2026-09-02: names are per-tenant. See
-- migrations/postgres/035_workflow_defs_tenant_in_key.sql for the reasoning,
-- and IMPROVEMENT-PLAN 3.77 for the four options and why this is option 4.
--
-- SQL Server needs no policy change, and the reason is worth recording because
-- it is the one place this migration differs between dialects.
-- dbo.fn_tenant_filter is
--
--     WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)
--
-- strict equality, with no default-tenant exception. PostgreSQL's
-- tenant_isolation_defs carried `OR tenant_id = '00000000-...'` so that
-- definitions predating per-tenant ownership stayed readable through the
-- adoption window; SQL Server never had that clause, so a default-tenant
-- definition has never been readable here by another tenant. Dropping the
-- PostgreSQL clause in 035 therefore brings the two into line rather than
-- moving them apart.
--
-- All three constraint names are declared explicitly in 001_schema.sql, so
-- unlike PostgreSQL and MySQL there is nothing to look up.

-- ── 1. Drop the foreign keys that point at the old key ───────────────────────
IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_instances_def')
    ALTER TABLE dbo.workflow_instances DROP CONSTRAINT fk_instances_def;
GO
IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_workflow_tags_def')
    ALTER TABLE dbo.workflow_tags DROP CONSTRAINT fk_workflow_tags_def;
GO
IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_workflow_routing_def')
    ALTER TABLE dbo.workflow_routing DROP CONSTRAINT fk_workflow_routing_def;
GO

-- ── 2. Swap the primary key ──────────────────────────────────────────────────
--
-- Guarded on the current column count: index_columns has one row per key
-- column, so a primary of two is the old (name, version) shape. Widening, so it
-- cannot fail on existing data.
IF EXISTS (
    SELECT 1
    FROM sys.indexes i
    JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
    WHERE i.object_id = OBJECT_ID(N'dbo.workflow_defs')
      AND i.is_primary_key = 1
    GROUP BY i.object_id, i.index_id
    HAVING COUNT(*) = 2
)
BEGIN
    ALTER TABLE dbo.workflow_defs DROP CONSTRAINT pk_workflow_defs;
    ALTER TABLE dbo.workflow_defs
        ADD CONSTRAINT pk_workflow_defs PRIMARY KEY (tenant_id, name, version);
END
GO

-- ── 3. Put the foreign keys back, with the tenant in them ────────────────────
--
-- No ON DELETE clause, matching what these carried before: NO ACTION, so
-- deleting a definition a running workflow still references is refused. That is
-- the protection worth keeping -- an operator cannot delete the code a workflow
-- is mid-replay of.
IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_instances_def')
    ALTER TABLE dbo.workflow_instances
        ADD CONSTRAINT fk_instances_def
        FOREIGN KEY (tenant_id, def_name, def_version)
        REFERENCES dbo.workflow_defs(tenant_id, name, version);
GO
IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_workflow_tags_def')
    ALTER TABLE dbo.workflow_tags
        ADD CONSTRAINT fk_workflow_tags_def
        FOREIGN KEY (tenant_id, workflow_name, version)
        REFERENCES dbo.workflow_defs(tenant_id, name, version);
GO
IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'fk_workflow_routing_def')
    ALTER TABLE dbo.workflow_routing
        ADD CONSTRAINT fk_workflow_routing_def
        FOREIGN KEY (tenant_id, workflow_name, target_version)
        REFERENCES dbo.workflow_defs(tenant_id, name, version);
GO
