-- cleat migration 059 (mssql): scope the workflow memory profile to a tenant
--
-- See migrations/postgres/056_the_memory_profile_is_scoped_to_a_tenant.sql for
-- the full finding (cleat#1040).
--
-- This file carries the part that is worth more than the fix: both tables are
-- bound to dbo.fn_tenant_filter, which is the security policy AND the source
-- of truth for engine/mssql_tenant_predicate_test.go's universe --
--
--     bind := regexp.MustCompile(`(?i)ADD FILTER PREDICATE dbo\.fn_tenant_filter\(tenant_id\)\s+ON\s+dbo\.(\w+)`)
--
-- so until now the guard was not merely silent about these two tables, it
-- could not see them. Binding them means every future statement against
-- either is checked for a tenant predicate at build time, which is the
-- mechanism that stops this class recurring rather than fixing one instance
-- of it.

-- Discard before adding the column: there is no owning row to derive a
-- tenant from. See the PostgreSQL file for why this is preferred to
-- defaulting existing rows to the default tenant.
TRUNCATE TABLE dbo.workflow_memory_stats;
TRUNCATE TABLE dbo.workflow_memory_samples;

IF COL_LENGTH('dbo.workflow_memory_stats', 'tenant_id') IS NULL
    ALTER TABLE dbo.workflow_memory_stats
        ADD tenant_id UNIQUEIDENTIFIER NOT NULL
        CONSTRAINT df_memory_stats_tenant DEFAULT '00000000-0000-0000-0000-000000000000';

IF COL_LENGTH('dbo.workflow_memory_samples', 'tenant_id') IS NULL
    ALTER TABLE dbo.workflow_memory_samples
        ADD tenant_id UNIQUEIDENTIFIER NOT NULL
        CONSTRAINT df_memory_samples_tenant DEFAULT '00000000-0000-0000-0000-000000000000';
GO

-- One summary row per (tenant, name).
IF EXISTS (SELECT 1 FROM sys.key_constraints WHERE name = 'pk_workflow_memory_stats')
    ALTER TABLE dbo.workflow_memory_stats DROP CONSTRAINT pk_workflow_memory_stats;

ALTER TABLE dbo.workflow_memory_stats
    ADD CONSTRAINT pk_workflow_memory_stats PRIMARY KEY (tenant_id, def_name);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_memory_samples_tenant_def')
    CREATE INDEX idx_memory_samples_tenant_def
        ON dbo.workflow_memory_samples (tenant_id, def_name, recorded_at DESC);
GO

-- Bind both tables to the tenant filter predicate. This is the database-level
-- backstop the other tenant-scoped tables already have, and it is what brings
-- these two inside the build-time guard's universe.
--
-- One policy per table, named TenantFilter_<Thing>, following 042's
-- precedent -- there is no single shared policy in this schema, and assuming
-- one (dbo.TenantSecurityPolicy) is the first thing I got wrong writing this
-- file. 001_schema.sql creates seven, 012_admin_role.sql restates them, and
-- 031 and 042 each add one more the same way.
--
-- The DROP-if-exists guard is 042's, and it is not defensive padding: a
-- SECURITY POLICY cannot be re-created over itself, and a half-applied
-- migration would otherwise leave this file unable to run a second time.

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_MemoryStats')
    DROP SECURITY POLICY dbo.TenantFilter_MemoryStats;
GO

CREATE SECURITY POLICY dbo.TenantFilter_MemoryStats
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_stats
    WITH (STATE = ON);
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_MemorySamples')
    DROP SECURITY POLICY dbo.TenantFilter_MemorySamples;
GO

CREATE SECURITY POLICY dbo.TenantFilter_MemorySamples
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_samples
    WITH (STATE = ON);
GO
