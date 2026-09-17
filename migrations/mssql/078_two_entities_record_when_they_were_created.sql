-- cleat migration 078 (mssql): two entities record when they were created
--
-- cleat#1702, the SQL Server half of postgres/086. See that file for why
-- existing rows are backfilled from `updated_at`, and in particular for why
-- that is an UPPER BOUND here rather than the exact value 085 could claim going
-- the other way -- these two members are the inverse case, carrying
-- `updated_at` and no `created_at`.
--
-- THE SECURITY POLICIES MUST BE OFF FOR THE BACKFILL, and this is the half that
-- differs from Postgres rather than a transliteration of it. Both tables carry
-- a FILTER PREDICATE -- dbo.TenantFilter_Settings (042) and
-- dbo.TenantFilter_Secrets (073) -- and SQL Server's filter predicates exempt
-- NOBODY, not even sysadmin. Postgres's equivalent UPDATE needs no toggle
-- because a superuser bypasses RLS unconditionally and cannot be forced
-- (001_schema.sql:614). Same intent, same rows, and the difference is not in
-- the SQL.
--
-- LEFT OFF, A FILTERED BACKFILL IS SILENT. `fn_tenant_filter` returns no rows
-- when SESSION_CONTEXT('tenant_id') is unset, which is what a migration runner
-- has, so the UPDATE would match zero rows and report success. The column would
-- then stay NULL -- and the `ALTER COLUMN ... NOT NULL` below is what catches
-- that: it fails rather than completing with a column nobody filled in. There
-- is no silent path, which is the property 077 states and this file inherits.
--
-- TRY/CATCH around each toggle, restoring STATE = ON on the way out, so a
-- failure cannot leave tenant isolation disabled. Scoped to one policy and one
-- table at a time rather than disabling both at once.
--
-- DATETIMEOFFSET NOT NULL with a NAMED default constraint, matching
-- `updated_at` on both tables (042, 073). Named rather than anonymous because
-- SQL Server otherwise invents something like DF__tenant_se__creat__1A2B3C4D,
-- which differs per database and cannot be dropped by a later migration without
-- first looking it up in sys.default_constraints.
--
-- Idempotent: COL_LENGTH guards the add, the backfill is predicated on IS NULL,
-- ALTER COLUMN is a no-op against the shape it already has, and the default
-- constraint is guarded on sys.default_constraints.

-- tenant_settings ------------------------------------------------------------

IF COL_LENGTH(N'dbo.tenant_settings', N'created_at') IS NULL
    ALTER TABLE dbo.tenant_settings ADD created_at DATETIMEOFFSET NULL;
GO
ALTER SECURITY POLICY dbo.TenantFilter_Settings WITH (STATE = OFF);
BEGIN TRY
    UPDATE dbo.tenant_settings SET created_at = updated_at WHERE created_at IS NULL;
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Settings WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Settings WITH (STATE = ON);
GO
ALTER TABLE dbo.tenant_settings ALTER COLUMN created_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_tenant_settings_created_at')
    ALTER TABLE dbo.tenant_settings ADD CONSTRAINT df_tenant_settings_created_at DEFAULT SYSUTCDATETIME() FOR created_at;
GO

-- tenant_secrets -------------------------------------------------------------

IF COL_LENGTH(N'dbo.tenant_secrets', N'created_at') IS NULL
    ALTER TABLE dbo.tenant_secrets ADD created_at DATETIMEOFFSET NULL;
GO
ALTER SECURITY POLICY dbo.TenantFilter_Secrets WITH (STATE = OFF);
BEGIN TRY
    UPDATE dbo.tenant_secrets SET created_at = updated_at WHERE created_at IS NULL;
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Secrets WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Secrets WITH (STATE = ON);
GO
ALTER TABLE dbo.tenant_secrets ALTER COLUMN created_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_tenant_secrets_created_at')
    ALTER TABLE dbo.tenant_secrets ADD CONSTRAINT df_tenant_secrets_created_at DEFAULT SYSUTCDATETIME() FOR created_at;
GO
