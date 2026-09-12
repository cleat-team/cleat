-- ===========================================================================
-- 065: the admin.* functions stop granting EXECUTE to PUBLIC
--
-- WHAT THIS FIXES, and it is not theoretical: cleat#1365.
--
-- Any tenant's own login role could call admin.drop_tenant() on ANY OTHER
-- tenant and destroy its data. No superuser, no SET ROLE, no misconfiguration.
-- Measured on 16.15 against a database built from these migrations, with two
-- tenants provisioned through admin.create_tenant_role and a connection opened
-- as tenant A's own login role:
--
--   CONTROL: tenant A sees 0 of tenant B's workflow_defs rows  -- RLS works
--   A calling admin.drop_tenant(B) -> err=<nil>
--   AFTER:   tenant B workflow_defs=0  admin.tenants=0
--
-- The control is the finding. RLS is doing exactly what it is designed to do
-- for direct access; a SECURITY DEFINER function executes as its OWNER and
-- does not consult it. So a role that cannot read one of B's rows destroys all
-- of them.
--
-- Why that matters more than it looks: plugin/tenant_db.go's TenantPools is
-- "the connection IS the tenant", and it exists so that isolation does not
-- depend on application code remembering a tenant predicate. The credential
-- handed out to strengthen isolation carried a capability that bypasses the
-- mechanism it exists to strengthen.
--
-- WHY EVERY FUNCTION IN THE SCHEMA AND NOT ONE (cleat#1373).
--
-- The census, by predicate rather than by reading ACL strings -- 
-- has_function_privilege('public', oid, 'EXECUTE') over every function in
-- admin, measured before this migration:
--
--   PUBLIC=false  claim_workflows            owner=cleat_dispatcher
--   PUBLIC=false  get_due_schedules          owner=cleat_dispatcher
--   PUBLIC=true   create_tenant_role         owner=<whoever ran the migrations>
--   PUBLIC=true   drop_tenant                owner=<whoever ran the migrations>
--   PUBLIC=true   grant_core_tables_to_tenant_role   (added by 064)
--   PUBLIC=true   grant_plugin_to_tenant     owner=<whoever ran the migrations>
--   PUBLIC=true   revoke_plugin_from_tenant  owner=<whoever ran the migrations>
--
-- Five of seven. The two clean ones are 023's and 024's, which assign an owner and grant
-- EXECUTE explicitly. The repository already knows the right shape; the four
-- in 001_schema.sql simply kept PostgreSQL's default of EXECUTE TO PUBLIC.
--
-- create_tenant_role is live, not latent: called on a tenant that already
-- exists it takes the ELSE branch and runs ALTER ROLE ... WITH PASSWORD with a
-- fresh value, so any tenant could force a credential rotation on any other
-- tenant's login role. grant_plugin_to_tenant and revoke_plugin_from_tenant
-- are latent only because admin.plugin_tables is empty -- RegisterPluginTables
-- has no production caller -- so they are one populated table away.
--
-- Fixing one and leaving the rest is also what makes the guard unwritable. The
-- durable fix is not four REVOKEs; it is
-- engine/admin_functions_are_not_public_test.go asserting that NO function in
-- admin grants EXECUTE to PUBLIC, so that function number seven inherits the
-- same default and is caught. That test cannot be written while three known
-- violations remain: it would ship with a four-entry allowlist, which records
-- the exceptions as acceptable and goes quiet on exactly the case it exists to
-- catch.
--
-- WHY THE ACL IS NOT NULL, which is what makes this visible at all. A function
-- with no explicit grants has proacl = NULL and PUBLIC holds EXECUTE
-- implicitly. These show `{=X/postgres,...}` instead, with a leading `=X/`
-- naming PUBLIC explicitly, because 005_app_role.sql's
-- `GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA admin TO cleat_app` materialised
-- the default ACL and wrote PUBLIC's entry into the catalogue alongside
-- cleat_app's.
--
-- WHAT THIS DOES NOT FIX, deliberately. admin.drop_tenant also carries no
-- search_path of its own, so a caller-created temporary table shadows the
-- eight unqualified tables in its body (cleat#1363). That is a name-resolution
-- bug, this is a grant bug, and neither closes the other. Adding
-- `SET search_path FROM CURRENT` here and closing both issues would leave the
-- grant hole behind a closed ticket.

-- BY SCHEMA, NOT BY SIGNATURE, and the first draft of this migration is the
-- argument for it. That draft named four functions with their argument types,
-- built from a census taken an hour earlier. Migration 064 landed in between:
-- it DROPped admin.create_tenant_role(UUID) and created
-- admin.create_tenant_role(UUID, TEXT), and it added a sixth function,
-- admin.grant_core_tables_to_tenant_role(TEXT), which inherited the same
-- PUBLIC default. So the signature list was wrong on one entry and short by
-- one function, an hour after it was measured.
--
-- The signature error was loud -- `function admin.create_tenant_role(uuid)
-- does not exist` -- and only by luck. Had 064 merely ADDED its function
-- without changing a signature, the enumerated migration would have applied
-- cleanly and left the new one public, which is the failure this whole
-- migration is about, reintroduced by the fix for it.
--
-- "Function number seven" was not hypothetical. It arrived while this was
-- being written.
REVOKE ALL ON ALL ROUTINES IN SCHEMA admin FROM PUBLIC;

-- ROUTINES rather than FUNCTIONS: ALL FUNCTIONS excludes procedures, and admin
-- holds none today. Naming the wider category costs nothing and does not have
-- to be revisited when one is added.

-- And stop the next one inheriting it. Without this, a function created by a
-- later migration gets PostgreSQL's default of EXECUTE TO PUBLIC and this
-- migration has fixed a list rather than a rule.
--
-- The limit, stated because it is easy to over-read: ALTER DEFAULT PRIVILEGES
-- is scoped to the role that runs it. It covers functions created later by
-- this same role, which is every migration, since they all run as the
-- migration runner. A function created by a DIFFERENT role is not covered --
-- which is why engine/admin_functions_are_not_public_test.go asserts the
-- property rather than trusting this line.
ALTER DEFAULT PRIVILEGES IN SCHEMA admin REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

-- cleat_app keeps EXECUTE: 005_app_role.sql grants it explicitly, and REVOKE
-- ... FROM PUBLIC does not touch a named grantee -- but REVOKE ALL ON ALL
-- ROUTINES is a blunter instrument than the four REVOKEs this replaced, so the
-- grant is re-asserted rather than assumed. 005 uses exactly this form.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        EXECUTE 'GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA admin TO cleat_app';
    END IF;
END
$$;
