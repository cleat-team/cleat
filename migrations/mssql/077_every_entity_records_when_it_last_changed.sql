-- cleat migration 077 (mssql): every entity records when it last changed
--
-- cleat#1702, the SQL Server half of postgres/085. See that file for why
-- existing rows are backfilled from created_at rather than stamped with the
-- migration's own clock, and for why nothing maintains the column yet.
--
-- SCHEMAS DIFFER FROM POSTGRES AND FROM MYSQL: `admin` for the three
-- tenant-administration tables, `dbo` for the rest, as in migration 076.
--
-- THE BACKFILL DISABLES FIVE SECURITY POLICIES, AND THAT IS NOT BELT AND
-- BRACES -- WITHOUT IT THIS MIGRATION FAILS ON ANY NON-EMPTY DATABASE.
--
-- Row-level security differs between the two engines in exactly the way that
-- matters here. PostgreSQL exempts a superuser from RLS unconditionally, and
-- migrations are applied by one in every configuration cleat ships
-- (migrations/postgres/005_app_role.sql documents this at length). SQL Server
-- grants sysadmin NO such exemption: a FILTER predicate applies to `sa` like
-- anyone else. A migration connection sets no SESSION_CONTEXT('tenant_id'), so
-- on the five `dbo` tables that carry a TenantFilter policy, every row is
-- invisible and `UPDATE ... SET updated_at = created_at` matches nothing.
--
-- Measured on this tree, before the policy toggles were added. One row seeded
-- into each of the eight, then this migration run as `sa`:
--
--     admin.tenant_api_keys      (1 rows affected)
--     admin.tenant_roles         (1 rows affected)
--     admin.tenant_egress_allow  (1 rows affected)
--     dbo.workflow_defs          (0 rows affected)   <-- filtered
--     ... then Msg 515: Cannot insert the value NULL into column 'updated_at'
--
-- The three `admin` tables carry no security policy and needed no toggle; the
-- failure began exactly at the first `dbo` one. On an EMPTY database the same
-- file applied cleanly in all three dialects, which is why this had to be
-- found by seeding rows rather than by running the migration set.
--
-- Each policy is disabled, the update runs, and it is re-enabled -- including
-- on the error path, via CATCH ... THROW, so a failure cannot leave tenant
-- isolation switched off. Each policy carries exactly one predicate over
-- exactly one table (sys.security_predicates, measured), so the window is the
-- table being backfilled and nothing else.
--
-- `ALTER COLUMN ... NOT NULL` IS THE ASSERTION. If a future policy change
-- hides rows from the backfill again, the column still holds NULL and this
-- migration fails at that line rather than completing with a column nobody
-- filled in. That is deliberate: there is no silent path.
--
-- DATETIMEOFFSET NOT NULL with a NAMED default constraint, matching
-- tenant_secrets in migration 073. Named rather than anonymous because SQL
-- Server otherwise invents something like DF__workflow___updat__1A2B3C4D, which
-- differs per database and cannot be dropped by a later migration without
-- first looking it up in sys.default_constraints.
--

IF COL_LENGTH(N'admin.tenant_api_keys', N'updated_at') IS NULL
    ALTER TABLE admin.tenant_api_keys ADD updated_at DATETIMEOFFSET NULL;
GO
UPDATE admin.tenant_api_keys SET updated_at = created_at WHERE updated_at IS NULL;
GO
ALTER TABLE admin.tenant_api_keys ALTER COLUMN updated_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_tenant_api_keys_updated_at')
    ALTER TABLE admin.tenant_api_keys ADD CONSTRAINT df_tenant_api_keys_updated_at DEFAULT SYSUTCDATETIME() FOR updated_at;
GO

IF COL_LENGTH(N'admin.tenant_roles', N'updated_at') IS NULL
    ALTER TABLE admin.tenant_roles ADD updated_at DATETIMEOFFSET NULL;
GO
UPDATE admin.tenant_roles SET updated_at = created_at WHERE updated_at IS NULL;
GO
ALTER TABLE admin.tenant_roles ALTER COLUMN updated_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_tenant_roles_updated_at')
    ALTER TABLE admin.tenant_roles ADD CONSTRAINT df_tenant_roles_updated_at DEFAULT SYSUTCDATETIME() FOR updated_at;
GO

IF COL_LENGTH(N'admin.tenant_egress_allow', N'updated_at') IS NULL
    ALTER TABLE admin.tenant_egress_allow ADD updated_at DATETIMEOFFSET NULL;
GO
UPDATE admin.tenant_egress_allow SET updated_at = created_at WHERE updated_at IS NULL;
GO
ALTER TABLE admin.tenant_egress_allow ALTER COLUMN updated_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_tenant_egress_allow_updated_at')
    ALTER TABLE admin.tenant_egress_allow ADD CONSTRAINT df_tenant_egress_allow_updated_at DEFAULT SYSUTCDATETIME() FOR updated_at;
GO

IF COL_LENGTH(N'dbo.workflow_defs', N'updated_at') IS NULL
    ALTER TABLE dbo.workflow_defs ADD updated_at DATETIMEOFFSET NULL;
GO
ALTER SECURITY POLICY dbo.TenantFilter_Defs WITH (STATE = OFF);
BEGIN TRY
    UPDATE dbo.workflow_defs SET updated_at = created_at WHERE updated_at IS NULL;
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Defs WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Defs WITH (STATE = ON);
GO
ALTER TABLE dbo.workflow_defs ALTER COLUMN updated_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_workflow_defs_updated_at')
    ALTER TABLE dbo.workflow_defs ADD CONSTRAINT df_workflow_defs_updated_at DEFAULT SYSUTCDATETIME() FOR updated_at;
GO

IF COL_LENGTH(N'dbo.workflow_schedules', N'updated_at') IS NULL
    ALTER TABLE dbo.workflow_schedules ADD updated_at DATETIMEOFFSET NULL;
GO
ALTER SECURITY POLICY dbo.TenantFilter_Schedules WITH (STATE = OFF);
BEGIN TRY
    UPDATE dbo.workflow_schedules SET updated_at = created_at WHERE updated_at IS NULL;
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Schedules WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Schedules WITH (STATE = ON);
GO
ALTER TABLE dbo.workflow_schedules ALTER COLUMN updated_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_workflow_schedules_updated_at')
    ALTER TABLE dbo.workflow_schedules ADD CONSTRAINT df_workflow_schedules_updated_at DEFAULT SYSUTCDATETIME() FOR updated_at;
GO

IF COL_LENGTH(N'dbo.workflow_routing', N'updated_at') IS NULL
    ALTER TABLE dbo.workflow_routing ADD updated_at DATETIMEOFFSET NULL;
GO
ALTER SECURITY POLICY dbo.TenantFilter_Routing WITH (STATE = OFF);
BEGIN TRY
    UPDATE dbo.workflow_routing SET updated_at = created_at WHERE updated_at IS NULL;
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Routing WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Routing WITH (STATE = ON);
GO
ALTER TABLE dbo.workflow_routing ALTER COLUMN updated_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_workflow_routing_updated_at')
    ALTER TABLE dbo.workflow_routing ADD CONSTRAINT df_workflow_routing_updated_at DEFAULT SYSUTCDATETIME() FOR updated_at;
GO

IF COL_LENGTH(N'dbo.workflow_tags', N'updated_at') IS NULL
    ALTER TABLE dbo.workflow_tags ADD updated_at DATETIMEOFFSET NULL;
GO
ALTER SECURITY POLICY dbo.TenantFilter_Tags WITH (STATE = OFF);
BEGIN TRY
    UPDATE dbo.workflow_tags SET updated_at = created_at WHERE updated_at IS NULL;
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Tags WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Tags WITH (STATE = ON);
GO
ALTER TABLE dbo.workflow_tags ALTER COLUMN updated_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_workflow_tags_updated_at')
    ALTER TABLE dbo.workflow_tags ADD CONSTRAINT df_workflow_tags_updated_at DEFAULT SYSUTCDATETIME() FOR updated_at;
GO

IF COL_LENGTH(N'dbo.tenant_domains', N'updated_at') IS NULL
    ALTER TABLE dbo.tenant_domains ADD updated_at DATETIMEOFFSET NULL;
GO
ALTER SECURITY POLICY dbo.TenantFilter_Domains WITH (STATE = OFF);
BEGIN TRY
    UPDATE dbo.tenant_domains SET updated_at = created_at WHERE updated_at IS NULL;
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Domains WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Domains WITH (STATE = ON);
GO
ALTER TABLE dbo.tenant_domains ALTER COLUMN updated_at DATETIMEOFFSET NOT NULL;
GO
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_tenant_domains_updated_at')
    ALTER TABLE dbo.tenant_domains ADD CONSTRAINT df_tenant_domains_updated_at DEFAULT SYSUTCDATETIME() FOR updated_at;
GO
