-- cleat migration 076 (mssql): one spelling of retirement for every entity
--
-- cleat#1702, the SQL Server half of postgres/084. See that file for why the
-- contract spells retirement as a nullable timestamp rather than a boolean --
-- the class had four spellings of one concept and one of them, `enabled`, is
-- inverted relative to the other three.
--
-- SCHEMAS DIFFER FROM POSTGRES AND FROM MYSQL: this dialect keeps the three
-- tenant-administration tables in `admin` and the rest in `dbo`, where Postgres
-- uses `admin` and `public` and MySQL has no schemas at all. Membership in the
-- contract is matched on the BARE name for exactly this reason (cleat#1719).
--
-- DATETIMEOFFSET, matching every other timestamp column in this dialect, and
-- nullable with NULL meaning live. No DEFAULT: there is no "unknown" state, and
-- a named default constraint here would only have to be found in
-- sys.default_constraints and dropped by a later migration.
--
-- No backfill: the legacy columns are still the authority until each table's
-- own conversion migration moves the value across.
--
-- COL_LENGTH guards each statement, which is this dialect's existing idiom for
-- an idempotent ADD COLUMN, so the file is safe against a database whose schema
-- engine/testutil built with the column already present.
-- ===========================================================================

IF COL_LENGTH(N'admin.tenant_api_keys', N'disabled_at') IS NULL
    ALTER TABLE admin.tenant_api_keys ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'admin.tenant_roles', N'disabled_at') IS NULL
    ALTER TABLE admin.tenant_roles ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'admin.tenant_egress_allow', N'disabled_at') IS NULL
    ALTER TABLE admin.tenant_egress_allow ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'dbo.workflow_defs', N'disabled_at') IS NULL
    ALTER TABLE dbo.workflow_defs ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'dbo.workflow_schedules', N'disabled_at') IS NULL
    ALTER TABLE dbo.workflow_schedules ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'dbo.workflow_routing', N'disabled_at') IS NULL
    ALTER TABLE dbo.workflow_routing ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'dbo.workflow_tags', N'disabled_at') IS NULL
    ALTER TABLE dbo.workflow_tags ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'dbo.tenant_settings', N'disabled_at') IS NULL
    ALTER TABLE dbo.tenant_settings ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'dbo.tenant_secrets', N'disabled_at') IS NULL
    ALTER TABLE dbo.tenant_secrets ADD disabled_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'dbo.tenant_domains', N'disabled_at') IS NULL
    ALTER TABLE dbo.tenant_domains ADD disabled_at DATETIMEOFFSET NULL;
GO
