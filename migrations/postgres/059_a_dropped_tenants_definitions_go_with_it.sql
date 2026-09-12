-- cleat migration 059 (postgres): a dropped tenant's workflow definitions go with it
--
-- Bug: admin.drop_tenant deletes thirteen tables of a tenant's data and leaves
-- that tenant's uploaded WASM in workflow_defs forever, keyed to a tenant_id
-- that no longer exists. cleatctl drop-tenant's usage text states the premise
-- -- "Does not touch workflow_defs/plugin_defs (shared registry, not
-- tenant-owned data)" -- and it is half true. cleat#1201.
--
-- WHAT THE SCHEMA SAYS, measured on a live database rather than read off
-- 001_schema.sql, which predates the change that matters:
--
--   workflow_defs  PRIMARY KEY (tenant_id, name, version)   <- tenant-owned
--   plugin_defs    PRIMARY KEY (name, version)              <- no tenant_id
--
-- So the sentence is right about plugin_defs, which this does not touch, and
-- wrong about workflow_defs. (001_schema.sql still shows workflow_defs keyed
-- (name, version); the tenant joined the key later, which is presumably how
-- the comment came to be written and why it was true when it was.)
--
-- WHY A DELETE AND NOT A FOREIGN KEY, which is the obvious fix and the one
-- migration 039 chose for tenant_settings.
--
-- An FK from workflow_defs.tenant_id to admin.tenants ON DELETE CASCADE works,
-- fixes the class rather than the instance, and I built it first. It also
-- makes writing a definition for a tenant with no admin.tenants row an ERROR,
-- and that turns out to be a live, reachable state rather than a hypothetical:
--
--   * 65 tests in ./engine fail on it. They deploy definitions under synthetic
--     tenant UUIDs to exercise isolation, and never create the tenant row --
--     a legitimate idiom for what they are testing.
--   * `cleat deploy --tenant <uuid>` writes workflow_defs directly, and
--     resolveDeployTenant (cmd/cleat/main.go) returns the flag or
--     CLEAT_TENANT_ID VERBATIM with no existence check. A typo'd UUID deploys
--     successfully today.
--
-- That is a product decision about whether --tenant should be validated, not
-- the bug cleat#1201 reports, and the issue said so itself: "I also have not
-- established what the right behaviour is... That is a design call." So this
-- fixes the reported bug and leaves the constraint question open, with the
-- measurements recorded on the issue.
--
-- 039 is not a contradicted precedent: tenant_settings was a NEW table with no
-- existing writers, so an FK there constrained nothing that already existed.
--
-- No data step. Unlike a constraint, a DELETE inside this function does not
-- care about rows already orphaned, so definitions belonging to tenants
-- dropped BEFORE this migration are still there. They are unreachable --
-- tenant_id is in the primary key and the tenant is gone -- and removing them
-- is a cleanup an operator should run knowingly rather than a side effect of
-- applying a migration. cleatctl check-db is the natural place to report them.

-- Unqualified names below resolve through search_path, which migration.Runner
-- sets to the configured schema before applying this file (and which
-- deploy/postgres/100-apply-migrations.sh sets through PGOPTIONS on the psql
-- path). This file used to pin it to the literal `public` itself; see the
-- WithSchema comment in migration/runner.go for why that had to stop.

CREATE OR REPLACE FUNCTION admin.drop_tenant(p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
BEGIN
    IF p_tenant_id = '00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'admin.drop_tenant: refusing to delete the default tenant (00000000-0000-0000-0000-000000000000) -- it is shared by every single-tenant deployment and by workflow_defs/plugin_defs, which are not tenant-owned data';
    END IF;

    -- See "RLS" above: makes every DELETE below correct regardless of
    -- whether the owning/executing role is a superuser.
    PERFORM set_config('cleat.tenant_id', p_tenant_id::text, true);

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    -- event_history has no FK/CASCADE back to workflow_instances (dropped
    -- deliberately by 003_procedures.sql); must be deleted explicitly or it
    -- is orphaned the instant the workflow_instances rows below are gone.
    DELETE FROM event_history WHERE tenant_id = p_tenant_id;

    -- Cascades workflow_signals, workflow_promises, concurrency_keys,
    -- workflow_update_requests via their real ON DELETE CASCADE FKs.
    DELETE FROM workflow_instances WHERE tenant_id = p_tenant_id;

    -- No FK relationship to workflow_instances at all; own tenant_id column.
    DELETE FROM workflow_schedules WHERE tenant_id = p_tenant_id;

    -- FK to workflow_defs (NO ACTION), not workflow_instances; own tenant_id
    -- column, so a straight tenant_id predicate is correct and does not
    -- touch workflow_defs itself.
    DELETE FROM workflow_tags WHERE tenant_id = p_tenant_id;
    DELETE FROM workflow_routing WHERE tenant_id = p_tenant_id;

    -- Not in Finding S3's original table list; added because it carries a
    -- tenant_id column (010) and its result/error_msg columns hold a
    -- workflow's actual output. See the comment block above.
    DELETE FROM idempotency_keys WHERE tenant_id = p_tenant_id;

    -- cleat#1201. A definition is the tenant's PROGRAM -- the most sensitive
    -- artifact they hand cleat -- and it was the one thing this function kept.
    --
    -- Safe here and not earlier: workflow_instances carries an FK to
    -- workflow_defs (NO ACTION), so a definition cannot be deleted while an
    -- instance references it. The instances of this tenant are deleted at the
    -- top of this function, so by now there are none.
    --
    -- plugin_defs is deliberately NOT touched: it has no tenant_id column at
    -- all (PRIMARY KEY (name, version)) and is genuinely shared, which is the
    -- half of the old "shared registry" comment that was true.
    DELETE FROM workflow_defs WHERE tenant_id = p_tenant_id;

    -- Must precede the admin.tenants delete below: admin.tenant_api_keys'
    -- FK to admin.tenants has no ON DELETE clause (NO ACTION), so deleting
    -- admin.tenants first would fail with a foreign key violation for any
    -- tenant that had ever had a key issued.
    DELETE FROM admin.tenant_api_keys WHERE tenant_id = p_tenant_id;

    -- Plugin data and role/schema cleanup (pre-existing), plus a bug found
    -- verifying this migration end-to-end: admin.create_tenant_role grants
    -- the tenant role SELECT/INSERT/UPDATE/DELETE on workflow_defs,
    -- workflow_instances, event_history, workflow_signals,
    -- workflow_promises, and workflow_schedules (001_schema.sql), and
    -- admin.grant_plugin_to_tenant grants it privileges on the tenant's own
    -- plugin tables too. DROP ROLE refuses to drop a role that still holds
    -- privileges anywhere:
    --
    --   ERROR:  role "cleat_tenant_..." cannot be dropped because some
    --   objects depend on it
    --   DETAIL:  privileges for schema public
    --            privileges for table workflow_instances ...
    --
    -- which is not a warning-and-continue error inside a PL/pgSQL function
    -- with no exception handler -- it aborts the whole function, and
    -- because a single top-level CALL is one transaction, every DELETE
    -- above rolls back with it. So the original admin.drop_tenant, called
    -- against any tenant that had ever been through
    -- create_tenant_role/grant_plugin_to_tenant (i.e. any tenant onboarded
    -- through the normal path), would have deleted nothing at all and
    -- surfaced a DROP ROLE error instead of the silent-almost-no-op Finding
    -- S3 describes -- loud rather than silent, but still a full failure of
    -- the one thing this function exists to do. Verified directly: the
    -- unmodified EXECUTE format('DROP ROLE IF EXISTS %I', ...) below,
    -- called against a role created by admin.create_tenant_role, raises
    -- exactly the error above and every DELETE in this function is rolled
    -- back with it -- confirmed by counting workflow_instances /
    -- event_history for the tenant afterward and finding them still
    -- present. DROP OWNED BY strips every privilege grant (and would drop
    -- any objects the role owned, though this role owns none -- it only
    -- has GRANTs) so the subsequent DROP ROLE succeeds. Guarded on
    -- existence first: DROP OWNED BY has no IF EXISTS form, and the role
    -- may legitimately not exist (grant_plugin_to_tenant and
    -- create_tenant_role both warn-and-skip rather than fail in
    -- single-tenant mode, per their own comments in 001_schema.sql).
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        EXECUTE format('DROP OWNED BY %I', v_role_name);
    END IF;
    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_schema_name);
    EXECUTE format('DROP ROLE IF EXISTS %I', v_role_name);

    DELETE FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    DELETE FROM admin.tenants WHERE tenant_id = p_tenant_id;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;
