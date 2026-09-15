-- cleat migration 067 (mysql): a hostname belongs to one tenant
--
-- cleat#1568. The PostgreSQL counterpart (079) carries the full reasoning for
-- why this table exists and why hostname is the primary key; this header
-- records only what differs on MySQL.
--
-- NO ROW-LEVEL SECURITY, because MySQL has none. Tenant isolation on this
-- dialect is a database per tenant, so a tenant_domains row is already
-- unreachable from another tenant's connection by construction -- the
-- separation is at a level above the table rather than inside it.
--
-- That has one consequence worth stating rather than discovering: on
-- PostgreSQL a policy makes "unknown host" and "host owned by another tenant"
-- the same answer, and the middleware inherits a no-oracle property from the
-- database. SQL Server reaches it with a SECURITY POLICY filter predicate (see
-- 071). Here the tenant's database simply does not contain another tenant's
-- rows, which reaches the same outcome by a third route. The middleware must
-- not rely on any of them specifically; it carries its own tenant predicate,
-- compares the row it gets against the authenticated tenant, and refuses
-- identically when there is no row.
--
-- hostname is VARCHAR(255) rather than TEXT because MySQL cannot make a TEXT
-- column a primary key without a prefix length, and 255 is the maximum length
-- of a DNS name. A prefix key would let two hostnames sharing a long prefix
-- collide, which is exactly the ambiguity the primary key exists to prevent.

CREATE TABLE IF NOT EXISTS tenant_domains (
    hostname   VARCHAR(255) NOT NULL,
    tenant_id  CHAR(36)     NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),

    PRIMARY KEY (hostname),
    KEY idx_tenant_domains_tenant (tenant_id),

    CONSTRAINT fk_tenant_domains_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id) ON DELETE CASCADE,

    CONSTRAINT ck_tenant_domains_hostname_lowercase
        CHECK (hostname = LOWER(hostname)),
    CONSTRAINT ck_tenant_domains_hostname_nonempty
        CHECK (CHAR_LENGTH(hostname) > 0),
    CONSTRAINT ck_tenant_domains_hostname_no_port
        CHECK (LOCATE(':', hostname) = 0)
);
