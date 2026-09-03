-- cleat migration 039 (mssql): put the tenant in the schedule key
--
-- D7, decided 2026-09-02: names are per-tenant. IMPROVEMENT-PLAN 3.77 step 3.
-- See migrations/postgres/036_workflow_schedules_tenant_in_key.sql for the
-- reasoning and for why there are no foreign keys to drop here.

-- ── The primary key ──────────────────────────────────────────────────────────
--
-- Named in 001_schema.sql, so unlike MySQL's InnoDB-generated foreign key names
-- there is nothing to look up. It is CLUSTERED, which is what makes the DROP
-- and re-ADD a table rebuild rather than an index swap -- acceptable here
-- because the table is small by construction (one row per schedule, bounded by
-- the per-tenant schedule quota) and this is the only chance to do it before
-- deployments have data.
--
-- Verified against a live database rather than read off the schema file:
--
--     SELECT i.name, i.type_desc, i.is_primary_key FROM sys.indexes i
--     WHERE i.object_id = OBJECT_ID('dbo.workflow_schedules') AND i.type > 0;
--     -> pk_workflow_schedules  | CLUSTERED    | 1
--        idx_schedules_tenant_enabled | NONCLUSTERED | 0
--
-- Guarded on the current shape: index_columns has one row per key column, so a
-- primary key of one column is the old (name) shape.
IF EXISTS (
    SELECT 1
    FROM sys.indexes i
    WHERE i.object_id = OBJECT_ID(N'dbo.workflow_schedules')
      AND i.is_primary_key = 1
      AND (SELECT COUNT(*) FROM sys.index_columns ic
           WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id) = 1
)
BEGIN
    ALTER TABLE dbo.workflow_schedules DROP CONSTRAINT pk_workflow_schedules;
    ALTER TABLE dbo.workflow_schedules
        ADD CONSTRAINT pk_workflow_schedules PRIMARY KEY (tenant_id, name);
END
GO

-- ── What deliberately does NOT change ────────────────────────────────────────
--
-- The security policy. dbo.fn_tenant_filter takes tenant_id and is bound to
-- workflow_schedules already; widening the primary key does not touch what the
-- predicate tests. Note that this is NOT the same as saying the table is
-- isolated -- the predicate admits any dbo.cleat_admin connection outright, so
-- the Go-level `AND tenant_id` on each schedule statement is what actually
-- separates tenants on a multi-tenant deployment. See IMPROVEMENT-PLAN 3.86;
-- those predicates are a prerequisite of this migration and land before it.
