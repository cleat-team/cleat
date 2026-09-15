-- cleat migration 072 (mssql): a secret never reaches the guest
--
-- cleat#1570. The PostgreSQL counterpart (080) carries the full reasoning; this
-- header records only what differs on SQL Server.
--
-- NUMBER TAKEN BY HAND: 071 is claimed by an open pull request (cleat#1568) and
-- is not visible on develop. See 080's header.
--
-- SCHEMA-QUALIFIED, AND THE FOREIGN KEY NAMES admin.tenants. Both learned the
-- hard way on cleat#1568, where an unqualified `tenants` resolved to dbo and
-- took every mssql test in the suite down with "Could not create constraint or
-- index" -- the same trap CLAUDE.md records costing a startup API key that was
-- never generated.
--
-- ROW-LEVEL SECURITY VIA A SECURITY POLICY, which SQL Server does have. The
-- first version of cleat#1568's migration asserted it did not, generalising
-- from engine/mssql_lifecycle.go's statement that a predicate "IS the whole of
-- the tenant scoping" for one query. 042_tenant_settings.sql has created a
-- FILTER PREDICATE since long before either.
--
-- NO CHECK CONSTRAINT ON THE NAME CHARSET. SQL Server has no regexp in a CHECK,
-- and the LIKE pattern that approximates it cannot express a bounded repeat of
-- a character class. Rather than ship a weaker constraint that LOOKS equivalent
-- to the other two dialects, this relies on engine.validSecretName, which every
-- write goes through. Stated rather than silently omitted: the guarantee here
-- is Go-level, and a row inserted by other means could carry a name the
-- resolver will never match.

CREATE TABLE dbo.tenant_secrets (
    tenant_id   UNIQUEIDENTIFIER NOT NULL,
    name        NVARCHAR(128)    NOT NULL,
    ciphertext  NVARCHAR(MAX)    NOT NULL,
    key_version INT              NOT NULL CONSTRAINT df_tenant_secrets_keyver DEFAULT 1,
    updated_at  DATETIMEOFFSET   NOT NULL CONSTRAINT df_tenant_secrets_updated DEFAULT SYSUTCDATETIME(),

    CONSTRAINT pk_tenant_secrets PRIMARY KEY (tenant_id, name),
    CONSTRAINT fk_tenant_secrets_tenant FOREIGN KEY (tenant_id)
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,
    CONSTRAINT ck_tenant_secrets_ciphertext_nonempty
        CHECK (LEN(ciphertext) > 0)
);
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Secrets')
    DROP SECURITY POLICY dbo.TenantFilter_Secrets;
GO

CREATE SECURITY POLICY dbo.TenantFilter_Secrets
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_secrets
    WITH (STATE = ON);
GO
