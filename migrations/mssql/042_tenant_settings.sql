-- cleat migration 042 (mssql): per-tenant execution limits.
--
-- IMPROVEMENT-PLAN 3.94 step 3. The PostgreSQL counterpart is
-- migrations/postgres/039_tenant_settings.sql and carries the full rationale
-- for the shape: NULL means "no override" and is distinct from zero, and the
-- CHECK constraints are a privilege boundary rather than input validation --
-- resolution clamps a tenant's value to the operator's flag, so a tenant may
-- only ever TIGHTEN a limit, and that property depends on the stored value
-- being positive. With zero allowed, a tenant writes 0, resolution reads
-- "unbounded", and the clamp hands back more than the operator granted.
--
-- ---------------------------------------------------------------------------
-- The FILTER PREDICATE is the whole isolation story on this dialect.
--
-- PostgreSQL has RLS on this table. SQL Server's equivalent is a security
-- policy over dbo.fn_tenant_filter, and 031_workflow_promises_security_policy
-- is the precedent for adding one in a later migration: CREATE SECURITY POLICY
-- needs only the function to exist already, so this file adds a ninth policy
-- without touching fn_tenant_filter or the eight already bound to it.
--
-- fn_tenant_filter's current definition is 012_admin_role.sql's, not
-- 001_schema.sql's -- it grew the cleat_admin bypass there. For anything
-- defined by CREATE OR ALTER, the highest-numbered migration that defines it
-- is the one in force.
--
-- tenant_id is UNIQUEIDENTIFIER, matching admin.tenants and every tenant-scoped
-- table on this dialect except dbo.workflow_promises, which is NVARCHAR(255)
-- and relies on an implicit conversion. There is no reason to inherit that
-- here.
--
-- Deletion is by foreign key rather than by a line in a hand-enumerated
-- cleanup routine, for the reason the PostgreSQL file gives. dbo.tenant_settings
-- references admin.tenants directly and is the only cascade path to it from
-- this table, so SQL Server's multiple-cascade-path restriction does not apply.

IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.tenant_settings') AND type = N'U')
CREATE TABLE dbo.tenant_settings (
    tenant_id                  UNIQUEIDENTIFIER NOT NULL,
    wasm_instance_timeout_ms   BIGINT           NULL,
    wasm_wall_clock_ceiling_ms BIGINT           NULL,
    host_retry_budget_ms       BIGINT           NULL,
    updated_at                 DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_tenant_settings PRIMARY KEY (tenant_id),
    CONSTRAINT fk_tenant_settings_tenant FOREIGN KEY (tenant_id)
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,
    CONSTRAINT ck_tenant_settings_instance_timeout_positive
        CHECK (wasm_instance_timeout_ms IS NULL OR wasm_instance_timeout_ms > 0),
    CONSTRAINT ck_tenant_settings_wall_clock_positive
        CHECK (wasm_wall_clock_ceiling_ms IS NULL OR wasm_wall_clock_ceiling_ms > 0),
    CONSTRAINT ck_tenant_settings_retry_budget_positive
        CHECK (host_retry_budget_ms IS NULL OR host_retry_budget_ms > 0)
);
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Settings')
    DROP SECURITY POLICY dbo.TenantFilter_Settings;
GO

CREATE SECURITY POLICY dbo.TenantFilter_Settings
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_settings
    WITH (STATE = ON);
GO
