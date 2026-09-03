-- cleat migration 040 (mssql): put the tenant in the deployment-tag key
--
-- D7, decided 2026-09-02. IMPROVEMENT-PLAN 3.77 step 4, the last of the three
-- tables. See migrations/postgres/037_workflow_tags_tenant_in_key.sql for the
-- reasoning and for why there are no foreign keys to drop -- fk_workflow_tags_def
-- already carries the tenant, widened by 038 when workflow_defs' key changed.

-- ── The primary key ──────────────────────────────────────────────────────────
--
-- Named in 001_schema.sql, and CLUSTERED, so the drop and re-add is a table
-- rebuild. Acceptable: the table holds one row per tag per workflow version.
--
-- Verified against a live database rather than read off the schema file:
--
--     SELECT i.name, i.type_desc, i.is_primary_key FROM sys.indexes i
--     WHERE i.object_id = OBJECT_ID('dbo.workflow_tags') AND i.type > 0;
--     -> pk_workflow_tags | CLUSTERED | 1        (and no other index)
--
-- Guarded on current shape: index_columns has one row per key column, so a
-- primary key of two columns is the old (workflow_name, tag) shape.
IF EXISTS (
    SELECT 1
    FROM sys.indexes i
    WHERE i.object_id = OBJECT_ID(N'dbo.workflow_tags')
      AND i.is_primary_key = 1
      AND (SELECT COUNT(*) FROM sys.index_columns ic
           WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id) = 2
)
BEGIN
    ALTER TABLE dbo.workflow_tags DROP CONSTRAINT pk_workflow_tags;
    ALTER TABLE dbo.workflow_tags
        ADD CONSTRAINT pk_workflow_tags PRIMARY KEY (tenant_id, workflow_name, tag);
END
GO

-- ── What deliberately does NOT change ────────────────────────────────────────
--
-- The security policy: dbo.fn_tenant_filter takes tenant_id and is bound to
-- workflow_tags already, and widening the primary key does not touch what the
-- predicate tests.
--
-- Note what that does NOT mean. The predicate admits any dbo.cleat_admin
-- connection outright, so on a multi-tenant deployment the Go-level
-- `AND tenant_id` on each tag statement is what actually separates tenants --
-- and before IMPROVEMENT-PLAN 3.86 those five statements had none, which let
-- one tenant repoint another's "stable" tag silently. Those predicates are a
-- prerequisite of this migration and land before it.
