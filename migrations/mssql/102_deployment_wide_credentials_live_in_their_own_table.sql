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
