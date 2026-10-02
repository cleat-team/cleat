-- cleat#2203. SQL Server has had one login per database since the baseline: the
-- migrate step and the serving worker both run as the owner login (`sa` in the
-- containers this repo tests against, whatever the deployment's administrator
-- login is otherwise), so a worker can INSERT/UPDATE/DELETE deployment_secrets
-- even though nothing it does is supposed to write there
-- (docs/how-to/use-deployment-secrets.md: "no cleat_app-equivalent application
-- role on SQL Server at all"). This follows PostgreSQL's cleat_app
-- (migrations/postgres/001_schema.sql, folded in from the pre-compaction
-- 005_app_role.sql): a least-privilege login the worker serves as, with the
-- owner/migrate login kept for schema changes only.
--
-- Note this is a DIFFERENT role from dbo.cleat_admin (migrations/mssql/001_schema.sql,
-- folded in from 012_admin_role.sql): that one exists for --claim-across-tenants,
-- is membership-gated and opt-in (migrations/mssql/optional/cross_tenant_claim.sql),
-- and nothing in the tree grants anything to it by default. cleat_app below gets
-- NO membership in cleat_admin -- it is meant to be an ordinary, single-tenant,
-- RLS-scoped connection, same as every login the fn_tenant_filter predicate
-- already treats as unprivileged (see docs/contributor/migrations.md: SQL
-- Server's security policies have no superuser-style bypass, so this split is
-- about segregating DDL rights and deployment_secrets writes, not about RLS
-- exemption the way PostgreSQL's cleat_app split is).
--
-- SQL Server LOGINs cannot be created without a password, unlike PostgreSQL's
-- NOLOGIN. The equivalent of "exists, cannot authenticate until a deployment
-- sets a real credential" is a login created WITH a throwaway password and
-- then immediately DISABLED -- ALTER LOGIN DISABLE refuses every
-- authentication attempt regardless of the password hash underneath it, the
-- same way CHECK_POLICY or account-lock semantics do elsewhere. The throwaway
-- value below authenticates nothing: a disabled login cannot log in with any
-- password, correct or not, so there is nothing here for a deployment to
-- protect. It is left meeting SQL Server's default password complexity policy
-- (CHECK_POLICY stays at its default, ON) rather than turning that policy off
-- to get a simpler literal past it -- ALTER LOGIN ... WITH PASSWORD leaves
-- CHECK_POLICY as it found it, so disabling it here would also exempt
-- whatever real password a deployment sets next. A deployment issues both of
-- the following together to bring it up:
--
--   ALTER LOGIN cleat_app WITH PASSWORD = '<password>';
--   ALTER LOGIN cleat_app ENABLE;
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'cleat_app' AND type = 'S')
BEGIN
    CREATE LOGIN cleat_app WITH PASSWORD = N'Disabled-At-Creation-2203!';
    ALTER LOGIN cleat_app DISABLE;
END
GO

IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_app' AND type = 'S')
    CREATE USER cleat_app FOR LOGIN cleat_app;
GO

-- Schema-level, not per-table: `admin` and `dbo` both gain tables over time --
-- every plugin install adds its own (audit, blobstore, eventtriggers,
-- eventstore, feature-flags, jobqueue, kvstore, notifications, oauthprovider,
-- scheduler, webhook-ingest all have their own migrations under dbo) -- and a
-- SQL Server schema-level GRANT applies to whatever the schema contains at
-- permission-check time, not just what existed when the GRANT ran. So a table
-- a plugin adds after this migration applies is covered with no further
-- change here, the same property PostgreSQL gets from
-- `ALTER DEFAULT PRIVILEGES IN SCHEMA ... GRANT ... ON TABLES`, by a different
-- mechanism. deployment_secrets is covered by this GRANT too, deliberately --
-- see the DENY below for why that does not mean cleat_app can write it.
GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::dbo TO cleat_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::admin TO cleat_app;
GRANT EXECUTE ON SCHEMA::dbo TO cleat_app;
GRANT EXECUTE ON SCHEMA::admin TO cleat_app;

-- The one thing SQL Server can do that neither PostgreSQL nor MySQL's grant
-- model can: DENY always overrides a GRANT, regardless of which level granted
-- it or in which order the two statements ran, so there is no equivalent here
-- of the MySQL file's "never grant it broadly in the first place" constraint.
-- cleat_app keeps SELECT on deployment_secrets from the schema-level GRANT
-- above; this removes only the write.
DENY INSERT, UPDATE, DELETE ON dbo.deployment_secrets TO cleat_app;
GO
