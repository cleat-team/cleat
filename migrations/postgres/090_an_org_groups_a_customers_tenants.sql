-- cleat migration 090 (postgres): an org groups a customer's tenants
--
-- cleat#1898. `admin.tenants` is flat -- no parent, no group. That blocks the
-- owner's model (see cleat-internal/org-model-design-2026-09-18.md): a
-- microservice maps to a tenant, and an org groups a customer's tenants and
-- is the billing and ownership entity. This is the foundation only --
-- identity, the link, and CreateTenant recording it. Cross-tenant calls,
-- exposure classes, and org-scoped authorization are separate, later work.
--
-- admin.orgs CARRIES IDENTITY ONLY. Owner's decision: "it's not a core
-- feature, and policy should not be built in there." No plan, no limit, no
-- usage column -- billing systems key on org_id from outside, and cleat knows
-- WHO, not what they bought. This matches a call the repo already made:
-- tenantquota is a plugin, not core, so limits are plugin territory by
-- existing precedent.
--
-- org_id IS IMMUTABLE, enforced by trigger rather than documented. The
-- asymmetry runs one way: adding a move operation later is easy, removing
-- mutability once people rely on UPDATE is not. It is also the option with a
-- real hazard behind it -- org_id will decide who may call a tenant's
-- org-scoped endpoints, so changing it is a trust-boundary change, not an
-- attribute edit. With a per-request policy cache (TenantEgressStore already
-- uses one, 30s), a mutable field gives a window where a tenant reads as
-- being in both orgs on different workers.
--
-- A TRIGGER, not a REVOKE. No trigger exists anywhere in this migration
-- history to copy the syntax from -- checked, not assumed
-- (grep -l 'RETURNS TRIGGER' migrations/postgres/*.sql returns nothing before
-- this file). A revoked column privilege was the other option offered, and it
-- was rejected here for a specific reason: GRANT/REVOKE is checked per ROLE,
-- and the table owner (or a superuser) bypasses it entirely by default --
-- exactly the connection an application pool is likely to use. A BEFORE
-- UPDATE trigger fires regardless of which role issues the UPDATE, which
-- "refused BY THE DATABASE" (the issue's own acceptance wording) needs.
--
-- EXISTING TENANTS GET A DEFAULT ORG, not a nullable sentinel. The issue's own
-- open question, resolved here as it leans: "a NOT NULL column with a default
-- org is simplest; a nullable one makes every consumer handle absence
-- forever, which argues against it." The default org uses the same
-- all-zeros UUID idiom 002_defaults.sql already established for the default
-- tenant, so there is exactly one bootstrap identity convention, not two.

CREATE TABLE IF NOT EXISTS admin.orgs (
    org_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    suspended  BOOLEAN NOT NULL DEFAULT false
);

INSERT INTO admin.orgs (org_id, name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default')
    ON CONFLICT (org_id) DO NOTHING;

-- table_schema = 'admin', hardcoded, NOT current_schema(). 089's guard on
-- workflow_schedules (an UNQUALIFIED name, resolved through search_path) has
-- to use current_schema() to stay pool-specific -- but admin.tenants is
-- schema-QUALIFIED, so there is no search_path ambiguity to guard against,
-- and current_schema() answers a different question: which schema this
-- SESSION defaults to, almost always 'public', not which schema the ALTER
-- below actually targets. Copying 089's pattern here made the guard always
-- read NOT EXISTS against public.tenants (which does not exist), so it fired
-- unconditionally -- caught by TestShippedSchema_IsIdempotent re-applying
-- this file to a database that already had it: "column org_id ... already
-- exists". Measured: SELECT current_schema() on a fresh connection to this
-- database returns 'public'.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'admin'
          AND table_name = 'tenants'
          AND column_name = 'org_id'
    ) THEN
        ALTER TABLE admin.tenants ADD COLUMN org_id UUID;
    END IF;
END $$;

UPDATE admin.tenants SET org_id = '00000000-0000-0000-0000-000000000000' WHERE org_id IS NULL;

ALTER TABLE admin.tenants ALTER COLUMN org_id SET NOT NULL;

-- A column DEFAULT, not just a backfill. Without it, every existing direct
-- SQL insert into admin.tenants across engine/*_test.go -- roughly a dozen
-- call sites, none of them about orgs -- fails on "null value in column
-- org_id violates not-null constraint" the moment this migration lands.
-- CreateTenant (auth/tenant_store.go) is unaffected: it names org_id in its
-- own INSERT explicitly and always will, so this default only matters to
-- callers that do not specify one.
ALTER TABLE admin.tenants ALTER COLUMN org_id SET DEFAULT '00000000-0000-0000-0000-000000000000';

-- table_schema = 'admin', same fix and same reason as the guard above.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_schema = 'admin'
          AND table_name = 'tenants'
          AND constraint_name = 'tenants_org_id_fkey'
    ) THEN
        ALTER TABLE admin.tenants
            ADD CONSTRAINT tenants_org_id_fkey FOREIGN KEY (org_id) REFERENCES admin.orgs(org_id);
    END IF;
END $$;

CREATE OR REPLACE FUNCTION admin.tenants_org_id_is_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.org_id IS DISTINCT FROM OLD.org_id THEN
        RAISE EXCEPTION 'admin.tenants.org_id is immutable and cannot be changed (tenant_id=%, from org %  to org %)',
            OLD.tenant_id, OLD.org_id, NEW.org_id
            USING ERRCODE = '23514'; -- check_violation, the same code a CHECK constraint would raise
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- REVOKE first: PostgreSQL grants EXECUTE to PUBLIC on a new function by
-- default. 065 revokes it schema-wide and sets a default privilege for
-- future functions created by the role that ran 065, but that role is not
-- necessarily the one running this migration, so this is explicit for the
-- same reason 073's is -- and TestNoAdminFunctionGrantsExecuteToPublic
-- caught this function without it.
REVOKE ALL ON FUNCTION admin.tenants_org_id_is_immutable() FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        GRANT EXECUTE ON FUNCTION admin.tenants_org_id_is_immutable() TO cleat_app;
    END IF;
END
$$;

DROP TRIGGER IF EXISTS tenants_org_id_immutable ON admin.tenants;
CREATE TRIGGER tenants_org_id_immutable
    BEFORE UPDATE ON admin.tenants
    FOR EACH ROW
    EXECUTE FUNCTION admin.tenants_org_id_is_immutable();

COMMENT ON COLUMN admin.tenants.org_id IS
    'cleat#1898: the org this tenant belongs to. IMMUTABLE -- enforced by the tenants_org_id_immutable trigger, not just this comment. Changing which org a tenant belongs to is a trust-boundary change (it decides who may reach the tenant''s org-scoped endpoints), not an attribute edit. An explicit, audited operator move operation is the answer when one is needed.';

COMMENT ON TABLE admin.orgs IS
    'cleat#1898: identity only, by design -- no plan, limit or usage column. Billing systems key on org_id from outside; cleat knows WHO, not what they bought.';
