-- ===========================================================================
-- 063: a plugin sweep can name itself cross-tenant (cleat.tenant_row_is_visible)
--
-- Why this exists
-- ---------------
-- #1277 gave plugin tables a fail-closed policy:
--
--     CREATE POLICY <t>_tenant_isolation ON <t>
--         FOR ALL USING (tenant_id = cleat.assert_tenant_set())
--
-- and #1278 measured what that costs. A tenant reaches a plugin on exactly one
-- path -- the HTTP middleware, the only non-test caller of auth.WithTenantID --
-- so a plugin's background loop has no tenant in its context and never can.
-- Against a policy that RAISES on an unset GUC, "add a policy" and "break the
-- sweep" are the same change, and sixteen of the seventeen plugins with a
-- database table have at least one such loop. kv_store adopted the policy
-- because every one of its access sites is request-scoped; nothing else could
-- follow.
--
-- The two ways out are a per-tenant loop, which is O(tenants) round trips at
-- the sweep interval, and an exemption a sweep asks for by name. This is the
-- second. The decision is recorded in #1278.
--
-- What it changes
-- ---------------
-- The plugin policy predicate moves from an inline comparison to this
-- function, so the two answers it has to give live in one place:
--
--     no bypass named  -> tenant_id = cleat.assert_tenant_set(), unchanged,
--                         including the RAISE when no tenant is set
--     bypass named     -> every row, for the duration of one transaction
--
-- Note what is NOT changed: every policy in 001_schema.sql, and every core
-- tenant_isolation_* policy since, still reads `tenant_id =
-- cleat.assert_tenant_set()` directly and has no bypass branch at all. The
-- engine's own cross-tenant needs are served by 023_cross_tenant_claim.sql and
-- 024_cross_tenant_schedules.sql -- a NOLOGIN BYPASSRLS role owning two
-- functions whose bodies are the whole of what the exemption can do. That is a
-- stronger construction than this one and it is the right one there, because
-- the engine's cross-tenant statements are two known queries. A plugin's are
-- arbitrary SQL written by the plugin author, so there is no function body to
-- bound the exemption with, and pretending otherwise would mean a SECURITY
-- DEFINER wrapper that takes a query string -- which is not a bound, it is a
-- hole with a ceremony in front of it.
--
-- docs/plugin-table-handling.md 3.2 proposed the narrower version of that:
-- not one wrapper taking a query, but a function per sweep, written by the
-- plugin author in its own migration, exactly as 023 does for the dispatcher.
-- That is the faithful reading of the engine's pattern and it was the starting
-- point here. It does not survive contact with WHO CREATES THE FUNCTION.
--
-- A SECURITY DEFINER function is exempt because of the role that OWNS it, and
-- 023's owner is cleat_dispatcher, a NOLOGIN BYPASSRLS role created by a core
-- migration running as an administrator. Plugin migrations do not run as that
-- administrator -- they run as the application role, through
-- plugin.RunMigrations, after the core set. For a plugin migration to create a
-- function owned by cleat_dispatcher, the application role would have to be
-- granted membership in cleat_dispatcher. At which point the application role
-- can create ANY number of RLS-exempt functions with any body at all, and the
-- exemption is no longer bounded by two reviewed function bodies; it is a
-- general capability held by the role that runs every plugin's DDL.
--
-- So the property that makes 023 safe is not SECURITY DEFINER. It is that the
-- exempt functions are created ONCE, by a core migration, with bodies fixed in
-- this repository. That property is not transferable to code a plugin author
-- writes, and a construction that looks like 023 without it would read as the
-- stronger thing while being the weaker one.
--
-- So be exact about what this is. It is NOT a security boundary against a
-- hostile plugin: plugins are Go compiled into the worker binary, and anything
-- that can write a plugin can call plugin.AcrossAllTenants, or open its own
-- pool, or shell out. It is a guard against a FORGOTTEN tenant predicate --
-- the failure #1277 demonstrated, where a query that names no tenant returns
-- another tenant's row and every test still passes -- and against that it is
-- exactly as strong as it was before, because the bypass cannot be reached by
-- forgetting something. It has to be typed.
--
-- Why an empty setting is not a bypass
-- ------------------------------------
-- set_config with is_local = true reverts the value when the transaction ends,
-- and "reverts" leaves the EMPTY STRING on that connection rather than NULL --
-- the same PostgreSQL behaviour 034 exists to handle for cleat.tenant_id, and
-- for the same reason it is dangerous here: connections come from a pool, so a
-- connection that has served one bypassed sweep would otherwise carry the
-- bypass to the next borrower. Testing for a non-empty value rather than for
-- presence is what stops that. The Go side refuses an empty reason before it
-- gets here; this is the half that does not depend on the Go side being right.
--
-- Why the value is the reason rather than a boolean
-- -------------------------------------------------
-- current_setting('cleat.cross_tenant') then answers "which sweep is this"
-- during an incident, from pg_stat_activity or a session that is misbehaving,
-- rather than only "something". It costs nothing: the check is already a
-- string comparison.
--
-- LANGUAGE sql and STABLE, so PostgreSQL can inline the body into the policy
-- qual. A plpgsql function cannot be inlined and would be called once per row
-- with a full executor context each time. cleat.assert_tenant_set() itself is
-- plpgsql and unmarked (therefore VOLATILE), which is a pre-existing per-row
-- cost in every policy in this schema and is not changed here; inlining this
-- wrapper leaves that cost exactly where it already was rather than adding a
-- second one on top of it.
--
-- CASE rather than OR. PostgreSQL does not guarantee the evaluation order of
-- OR operands, so `bypass OR tenant_id = cleat.assert_tenant_set()` may
-- evaluate the right side even when the left is true -- and the right side
-- RAISES when no tenant is set, which is precisely the state a sweep is in.
-- CASE guarantees the untaken branch is not evaluated.

CREATE OR REPLACE FUNCTION cleat.tenant_row_is_visible(row_tenant uuid)
RETURNS boolean
LANGUAGE sql
STABLE
AS $$
    SELECT CASE
        WHEN coalesce(current_setting('cleat.cross_tenant', true), '') <> ''
            THEN true
        ELSE row_tenant = cleat.assert_tenant_set()
    END
$$;

-- Plugin policies created before this migration were written with the inline
-- predicate and keep it until their plugin's migration is re-applied, which
-- never happens for an already-recorded version. That is safe in the direction
-- that matters: such a policy has no bypass branch, so a sweep against it
-- fails closed exactly as it does today. It is not silently permissive.
--
-- kv_store is the only table in this repository in that position, and it is
-- rewritten here rather than left to drift, so that the shipped tree has one
-- form of the policy rather than two. IF EXISTS because a database whose
-- plugin migrations have not yet run does not have the table -- plugin
-- migrations run after this file, from plugin.RunMigrations, not from here.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename = 'kv_store'
          AND policyname = 'kv_store_tenant_isolation'
    ) THEN
        DROP POLICY kv_store_tenant_isolation ON public.kv_store;
        CREATE POLICY kv_store_tenant_isolation ON public.kv_store
            FOR ALL USING (cleat.tenant_row_is_visible(tenant_id));
    END IF;
END
$$;
