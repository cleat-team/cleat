-- cleat#2059 compacted defaults (003)
-- Runs after 002_procedures.sql, which is what makes admin.create_tenant_role
-- available here -- and also why this file does NOT call it.
--
-- Unqualified names below resolve through search_path, which migration.Runner
-- sets to the configured schema before applying this file (see 001_schema.sql's
-- header for the incident that made that non-negotiable).

-- Same all-zeros bootstrap identity as the default tenant below (091's own
-- comment: "there is exactly one bootstrap identity convention, not two").
-- admin.tenants.org_id defaults to this id and carries a NOT NULL FK to
-- admin.orgs, so this insert must precede the tenant insert.
INSERT INTO admin.orgs (org_id, name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default')
    ON CONFLICT (org_id) DO NOTHING;

INSERT INTO admin.tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant')
    ON CONFLICT (tenant_id) DO NOTHING;

-- KNOWN GAP, left open deliberately rather than papered over -- see the
-- cleat#2059 draft report: under --tenant-isolation=role, every OTHER tenant
-- gets its PostgreSQL login role from cmd/cleat-worker's --create-tenant path
-- (main.go, "PROVISION THE ROLE"), which derives the password from the
-- worker's key via plugin.TenantRolePassword. That derivation needs the key,
-- which a plain SQL migration file has no access to -- so this file cannot
-- call admin.create_tenant_role(p_tenant_id, p_password) for the default
-- tenant the way the OLD 002_defaults.sql's backfill loop used to (and only
-- could, because it ran back when create_tenant_role took no password
-- argument and generated one internally -- migration 064 removed that form).
--
-- Concretely: a deployment using --tenant-isolation=role gets a working role
-- for every tenant it creates with --create-tenant, but the default tenant
-- (00000000-0000-0000-0000-000000000000, seeded above) has none, on a fresh
-- install built from this compacted baseline. The historical 001-104 chain
-- did not have this gap, purely as a side effect of 002_defaults.sql running
-- while the pre-064 signature still existed -- not because anything
-- deliberately provisioned the default tenant specially.
--
-- Not resolved here. Flagged for the cleat#2059 freeze decision: either (a)
-- cmd/cleat-worker provisions the default tenant's role at boot the same way
-- --create-tenant does for any other tenant, closing the gap properly, or
-- (b) an operator on role isolation runs an equivalent of --create-tenant
-- for the default tenant explicitly. Either is a real code/ops decision, not
-- a migration-file one.
