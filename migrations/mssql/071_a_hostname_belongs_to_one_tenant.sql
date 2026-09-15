-- cleat migration 071 (mssql): a hostname belongs to one tenant
--
-- cleat#1568. The PostgreSQL counterpart (079) carries the full reasoning for
-- why this table exists and why hostname is the primary key; this header
-- records only what differs on SQL Server.
--
-- THE FIRST VERSION OF THIS FILE GOT TWO THINGS WRONG AND CI CAUGHT BOTH,
-- recorded because the second is a claim someone might otherwise repeat.
--
-- It referenced `tenants` unqualified. On SQL Server the table is
-- admin.tenants, and an unqualified name resolves to dbo -- CLAUDE.md records
-- the same trap costing a startup key that was never generated, because
-- `SELECT count(*) FROM tenant_api_keys` counted an always-empty dbo copy.
-- Here it surfaced as "Could not create constraint or index" and took every
-- mssql test in the suite down with it, since no migration could apply.
--
-- And its header asserted that SQL Server has no row-level security, so the
-- lookup's own predicate was the whole of the scoping. THAT IS FALSE.
-- 042_tenant_settings.sql creates dbo.TenantFilter_Settings, a SECURITY POLICY
-- with a FILTER PREDICATE, and this file now does the same. The engine does
-- also carry explicit predicates -- engine/mssql_lifecycle.go says the
-- predicate "IS the whole of the tenant scoping" for CountRunnableWorkflows --
-- but that is a statement about one query, not about the dialect.
--
-- The middleware's lookup still carries `AND tenant_id = ?` on every dialect,
-- which is now belt and braces here as it is on PostgreSQL rather than the only
-- protection. That was the right code either way; the reasoning written beside
-- it was wrong.
--
-- NVARCHAR(255) rather than NVARCHAR(MAX): SQL Server will not index a MAX
-- column, and this table exists to be looked up by hostname on every request.
-- 255 is the maximum length of a DNS name, and 510 bytes is well inside the
-- 900-byte index key limit.

CREATE TABLE dbo.tenant_domains (
    hostname   NVARCHAR(255)    NOT NULL,
    tenant_id  UNIQUEIDENTIFIER NOT NULL,
    created_at DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),

    CONSTRAINT pk_tenant_domains PRIMARY KEY (hostname),
    CONSTRAINT fk_tenant_domains_tenant FOREIGN KEY (tenant_id)
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,
    CONSTRAINT ck_tenant_domains_hostname_lowercase
        CHECK (hostname = LOWER(hostname)),
    CONSTRAINT ck_tenant_domains_hostname_nonempty
        CHECK (LEN(hostname) > 0),
    -- A port must not appear here. The middleware strips it from Host before
    -- looking up, so a row carrying one could never match, and the failure
    -- would look like a configuration that was ignored.
    CONSTRAINT ck_tenant_domains_hostname_no_port
        CHECK (CHARINDEX(':', hostname) = 0)
);
GO

-- The reverse lookup: every hostname a tenant owns. The primary key serves the
-- middleware's hostname -> tenant direction; this serves an operator listing a
-- tenant's domains, and the cascade above.
CREATE INDEX idx_tenant_domains_tenant ON dbo.tenant_domains(tenant_id);
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Domains')
    DROP SECURITY POLICY dbo.TenantFilter_Domains;
GO

CREATE SECURITY POLICY dbo.TenantFilter_Domains
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_domains
    WITH (STATE = ON);
GO
