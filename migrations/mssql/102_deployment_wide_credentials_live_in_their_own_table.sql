-- cleat migration 102 (mssql): deployment-wide credentials live in their own table
--
-- cleat#1992 part (1). The PostgreSQL counterpart (103) carries the full
-- reasoning; this header records only what differs on SQL Server.
--
-- SCHEMA-QUALIFIED (dbo.), the same lesson cleat#1568 cost tenant_secrets'
-- own mssql migration (073).
--
-- NO SECURITY POLICY and no DENY. This table carries no tenant_id, so there
-- is no predicate for dbo.fn_tenant_filter to key on -- unlike tenant_secrets,
-- which uses the security-policy mechanism SQL Server does have. SQL Server
-- also has no cleat_app-equivalent role split the way PostgreSQL does (see
-- 012_admin_role.sql's own header: every principal, sysadmin included, is
-- subject to the same predicate, and there is no GRANT-based application
-- role this migration could narrow). Protection here is the same as on
-- every dialect: the SQL a worker issues only ever sees ciphertext, and the
-- master key never touches the database.
--
-- NO CHECK CONSTRAINT ON THE NAME CHARSET, for the reason tenant_secrets'
-- own mssql migration (073) gives: SQL Server has no regexp in a CHECK, and
-- the guarantee is Go-level (engine.validSecretName), which every write goes
-- through.
--
-- GUARDED BY OBJECT EXISTENCE, unlike 073's CREATE TABLE. cleat#2117's
-- deploy-step test (cmd/cleat-worker/a_migration_is_a_deploy_step_test.go,
-- landed 2026-09-24) exercises "the NEWEST migration's tracking row is
-- missing, --migrate-only repairs it" against whichever migration happens to
-- be highest-numbered -- 102 on this dialect, as of this writing -- so this
-- CREATE must tolerate running a second time against a database where the
-- table already exists (the DDL already applied; only the schema_migrations
-- row was removed), the same as PostgreSQL's and MySQL's IF NOT EXISTS
-- already do. Found by that test failing on mssql only, after cleat-review's
-- #2202 pass: "There is already an object named 'deployment_secrets'."

IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.deployment_secrets') AND type = N'U')
CREATE TABLE dbo.deployment_secrets (
    name        NVARCHAR(128)    NOT NULL,
    ciphertext  NVARCHAR(MAX)    NOT NULL,
    key_version INT              NOT NULL CONSTRAINT df_deployment_secrets_keyver DEFAULT 1,
    disabled_at DATETIMEOFFSET   NULL,
    updated_at  DATETIMEOFFSET   NOT NULL CONSTRAINT df_deployment_secrets_updated DEFAULT SYSUTCDATETIME(),
    created_at  DATETIMEOFFSET   NOT NULL CONSTRAINT df_deployment_secrets_created DEFAULT SYSUTCDATETIME(),

    CONSTRAINT pk_deployment_secrets PRIMARY KEY (name),
    CONSTRAINT ck_deployment_secrets_ciphertext_nonempty
        CHECK (LEN(ciphertext) > 0)
);
GO
