-- ===========================================================================
-- 067: a tenant says which hosts its workflows may reach (tenant_egress_allow)
--
-- cleat#1565. A workflow can ask the worker to fetch a URL. Until #1587 nothing
-- inspected the destination; that added a non-overridable floor (loopback,
-- link-local, RFC1918). This is the other half: egress is permitted only to
-- hosts the tenant has listed.
--
-- AN EMPTY LIST PERMITS NOTHING. Owner decision 2026-09-14. Not "unconfigured,
-- so allow" -- a platform that runs code it did not write must not read the
-- absence of a policy as permission. There are no live deployments to migrate,
-- which is what makes the strict reading affordable now and would not be later.
--
-- Entry forms, and only these two:
--     example.com      exact host, case-insensitive
--     .example.com     any host ENDING in .example.com; the APEX is excluded,
--                      so an operator writing the narrower-looking entry does
--                      not get the wider grant.
-- Not a regex and not a glob: this is a security boundary, and "what does this
-- permit" has to be answerable by reading it.
--
-- NO ROW-LEVEL SECURITY, and that is deliberate rather than an omission. It
-- sits in admin alongside tenant_api_keys, which is the same shape: per-tenant
-- SECURITY configuration that the worker reads ON BEHALF OF a tenant, on a
-- pool that carries no tenant GUC. A policy here would not fail closed in the
-- useful sense -- it would make an unreadable table indistinguishable from an
-- empty allowlist, and an empty allowlist denies everything. Silent total
-- egress failure is a worse outcome than the one the policy would prevent, and
-- the reads are already scoped by an explicit tenant_id predicate.
--
-- The floor still applies on top of whatever is listed here: allowlisting
-- 169.254.169.254 does not reach cloud instance metadata.
-- ===========================================================================

CREATE TABLE IF NOT EXISTS tenant_egress_allow (
    tenant_id  CHAR(36)     NOT NULL,
    host       VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (tenant_id, host),
    FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id) ON DELETE CASCADE
) ENGINE=InnoDB;
