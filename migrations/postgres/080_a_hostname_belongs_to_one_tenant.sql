-- cleat migration 080 (postgres): a hostname belongs to one tenant
--
-- cleat#1568. Per-tenant URLs -- the headline of the multi-tenant story in
-- docs/playbooks/b2b-saas-control-plane.md -- have no safe implementation
-- without this table, and the reason is a distinction the HTTP layer does not
-- make on its own.
--
-- AN API KEY IS PROVED; A HOSTNAME IS ASSERTED. The caller presents a
-- credential cleat verifies against admin.tenant_api_keys. The caller also
-- presents a Host header, which cleat has, until now, had no way to check
-- against anything. So tenant A's valid key works against tenant B's URL. That
-- is a confused deputy, and it is the seed of multi-tenant cache poisoning:
-- anything in front that keys on the URL -- a CDN, a shared cache -- can then
-- be induced to serve one tenant's response to another.
--
-- The rule this table implements, from docs/multi-tenant-serving-design.md:
-- the tenant comes from the credential, and the Host is then ASSERTED to match
-- it. Not the other way round, ever.
--
-- ---------------------------------------------------------------------------
-- WHY hostname IS THE PRIMARY KEY, not (tenant_id, hostname).
--
-- A hostname resolves to exactly one tenant or the mapping is meaningless: two
-- rows claiming `app.example.com` would make the question this table exists to
-- answer ambiguous, and the middleware would have to pick. A global primary key
-- makes that unrepresentable rather than merely discouraged.
--
-- ---------------------------------------------------------------------------
-- ROW-LEVEL SECURITY GIVES THE NO-ORACLE PROPERTY FOR FREE, which is worth
-- stating because it was not the reason for adding the policy and is a real
-- consequence of it.
--
-- The middleware reads this table as the AUTHENTICATED tenant. Under the policy
-- below, a hostname owned by another tenant returns NO ROW -- indistinguishable
-- from a hostname nobody owns. So "unknown host" and "host belongs to someone
-- else" are the same answer at the database, and the refusal above it cannot
-- become an oracle for which tenants exist. handleDeadLetterTerminate argues
-- exactly this for workflow ids and had to do it by hand; here the policy does
-- it.
--
-- WRITES ARE OPERATOR-ONLY IN THIS MIGRATION, and that is a scope decision
-- rather than an oversight. Were tenants able to register their own hostnames,
-- the PRIMARY KEY would leak what the SELECT does not: tenant A could probe
-- whether tenant B owns a name by attempting to insert it and reading the
-- unique violation. Closing that needs domain-ownership verification (a DNS TXT
-- challenge or similar), which is a larger piece of work and is tracked on
-- cleat#1568 rather than half-built here. Until it exists, rows are written by
-- an operator.
--
-- ---------------------------------------------------------------------------
-- Deletion is by foreign key, following 039's reasoning verbatim:
-- admin.drop_tenant enumerates tables by hand, so an entry there is a line
-- someone must remember to add, and 039's own header records tenant_settings
-- being absent from that list for twenty migrations. ON DELETE CASCADE cannot
-- be forgotten, and it composes with drop_tenant because that function deletes
-- admin.tenants LAST. Referential-integrity actions are not subject to
-- row-level security, so the cascade is unaffected by the policy below.
--
-- The name is still added to cleatctl's dropTenantTables, for the count it
-- reports -- that list's own comment says an operator "does not care which
-- mechanism took their data".

CREATE TABLE IF NOT EXISTS tenant_domains (
    -- Stored lowercase. Host comparison is case-insensitive per RFC 4343, and
    -- a mixed-case row would simply never match a normalised lookup, which
    -- presents as "my domain is configured and cleat refuses it".
    hostname   TEXT PRIMARY KEY,

    tenant_id  UUID NOT NULL
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_tenant_domains_hostname_lowercase
        CHECK (hostname = lower(hostname)),
    CONSTRAINT ck_tenant_domains_hostname_nonempty
        CHECK (length(hostname) > 0),
    -- A port must not appear here. The middleware strips it from Host before
    -- looking up, so a row carrying one could never match, and the failure
    -- would look like a configuration that was ignored.
    CONSTRAINT ck_tenant_domains_hostname_no_port
        CHECK (position(':' in hostname) = 0)
);

-- The reverse lookup: every hostname a tenant owns. The primary key serves the
-- middleware's hostname -> tenant direction; this serves an operator listing a
-- tenant's domains, and the cascade above.
CREATE INDEX IF NOT EXISTS idx_tenant_domains_tenant ON tenant_domains(tenant_id);

-- Row-level security, in the shape 039 established. FORCE is what makes it real
-- for the table owner: without it RLS is silently bypassed for the role that
-- ran the migration, which normally owns what it creates. See 001_schema.sql
-- and IMPROVEMENT-PLAN 1.10.
ALTER TABLE tenant_domains ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_domains FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_domains ON tenant_domains;
CREATE POLICY tenant_isolation_domains ON tenant_domains
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

-- No GRANT here, deliberately: 005_app_role.sql issues ALTER DEFAULT PRIVILEGES
-- covering tables the migration role creates afterwards, this one included.
-- Same reasoning as 039.
