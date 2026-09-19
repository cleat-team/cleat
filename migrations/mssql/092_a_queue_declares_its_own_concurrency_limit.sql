-- cleat migration 092 (mssql): a queue declares its own concurrency limit
--
-- See migrations/postgres/093_a_queue_declares_its_own_concurrency_limit.sql
-- for the full reasoning: cleat#1116, explicit registration rather than
-- implicit-on-first-use, #1702-conforming (created_at/updated_at/disabled_at,
-- no new retirement spelling). This header records only what differs here.
--
-- NO TENANT-ROLE GRANT. 064's own header states why: "login roles are the
-- mechanism, and ... SQL Server uses session context" -- there is no per-tenant
-- login role on this dialect to grant to, so unlike the postgres migration this
-- one is table + security policy only.
--
-- UNGUARDED CREATE TABLE, matching 072 and 073's own precedent for a brand-new
-- table on this dialect -- checked rather than assumed
-- (grep -B3 'CREATE TABLE dbo\.' against both returns no existence guard).
--
-- NO CHECK CONSTRAINT ON THE NAME CHARSET, following 073's own reasoning for
-- tenant_secrets: SQL Server has no regexp in a CHECK, and a LIKE pattern
-- cannot express a bounded repeat of a character class. Relies on the same
-- Go-level validator every write goes through, stated rather than silently
-- omitted.

CREATE TABLE dbo.queues (
    tenant_id           UNIQUEIDENTIFIER NOT NULL,
    name                NVARCHAR(128)    NOT NULL,
    concurrency_limit   INT              NOT NULL,
    created_at          DATETIMEOFFSET   NOT NULL CONSTRAINT df_queues_created DEFAULT SYSUTCDATETIME(),
    updated_at          DATETIMEOFFSET   NOT NULL CONSTRAINT df_queues_updated DEFAULT SYSUTCDATETIME(),
    disabled_at         DATETIMEOFFSET   NULL,

    CONSTRAINT pk_queues PRIMARY KEY (tenant_id, name),
    CONSTRAINT fk_queues_tenant FOREIGN KEY (tenant_id)
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,
    CONSTRAINT ck_queues_concurrency_limit_positive
        CHECK (concurrency_limit >= 1)
);
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Queues')
    DROP SECURITY POLICY dbo.TenantFilter_Queues;
GO

CREATE SECURITY POLICY dbo.TenantFilter_Queues
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.queues
    WITH (STATE = ON);
GO
