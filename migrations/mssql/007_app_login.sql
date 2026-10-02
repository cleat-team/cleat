-- cleat#2203. SQL Server has had one login per database since the baseline: the
-- migrate step and the serving worker both run as the owner login (`sa` in the
-- containers this repo tests against, whatever the deployment's administrator
-- login is otherwise), so a worker can INSERT/UPDATE/DELETE deployment_secrets
-- even though nothing it does is supposed to write there
-- (docs/how-to/use-deployment-secrets.md: "no cleat_app-equivalent application
-- role on SQL Server at all"). This follows PostgreSQL's cleat_app
-- (migrations/postgres/001_schema.sql, folded in from the pre-compaction
-- 005_app_role.sql): a least-privilege identity the worker serves as, with the
-- owner/migrate login kept for schema changes only.
--
-- cleat-review's G2 (cleat#2203): this file used to CREATE the LOGIN directly,
-- the same way PostgreSQL's baseline creates cleat_app. Measured against a
-- login scoped to db_owner on its own database with no server-level role --
-- which is everything develop asks of a SQL Server migrate login today, and
-- which migrates develop cleanly -- CREATE LOGIN failed: "User does not have
-- permission to perform this action. (15247)". CREATE LOGIN is a SERVER-level
-- operation (needs securityadmin or sysadmin); nothing else a SQL Server
-- migration does is. Shipping it here would have broken every upgrade whose
-- migrate login is scoped the way SQL Server deployments are documented to
-- scope it.
--
-- So this migration creates a database ROLE instead -- CREATE ROLE needs only
-- ALTER ANY ROLE, which db_owner already implies within its own database, same
-- as every GRANT/DENY below. The LOGIN that actually authenticates as cleat_app
-- is created by deploy/mssql/900-app-role.sql, run once by an operator who DOES
-- have securityadmin or sysadmin -- the same division PostgreSQL already has
-- between this file (creates the role) and the deployment's own
-- `ALTER ROLE cleat_app LOGIN PASSWORD ...` step
-- (deploy/postgres/900-app-role.sh), just split one principal earlier here
-- because SQL Server's role and login are two separate object types where
-- PostgreSQL's are one.
--
-- Note cleat_app_role is a DIFFERENT role from dbo.cleat_admin
-- (migrations/mssql/001_schema.sql, folded in from 012_admin_role.sql): that
-- one exists for --claim-across-tenants, is membership-gated and opt-in
-- (migrations/mssql/optional/cross_tenant_claim.sql), and nothing in the tree
-- grants anything to it by default. cleat_app gets NO membership in
-- cleat_admin -- it is meant to be an ordinary, single-tenant, RLS-scoped
-- connection, same as every login the fn_tenant_filter predicate already
-- treats as unprivileged (see docs/contributor/migrations.md: SQL Server's
-- security policies have no superuser-style bypass, so this split is about
-- segregating DDL rights and deployment_secrets writes, not about RLS
-- exemption the way PostgreSQL's cleat_app split originally was).
IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_app_role' AND type = 'R')
    CREATE ROLE cleat_app_role;
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
-- see the DENY below for why that does not mean a member of this role can
-- write it.
GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::dbo TO cleat_app_role;
GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::admin TO cleat_app_role;
GRANT EXECUTE ON SCHEMA::dbo TO cleat_app_role;
GRANT EXECUTE ON SCHEMA::admin TO cleat_app_role;

-- The one thing SQL Server can do that neither PostgreSQL nor MySQL's grant
-- model can: DENY always overrides a GRANT, regardless of which level granted
-- it or in which order the two statements ran. A member of cleat_app_role
-- keeps SELECT on deployment_secrets from the schema-level GRANT above; this
-- removes only the write.
DENY INSERT, UPDATE, DELETE ON dbo.deployment_secrets TO cleat_app_role;
GO
