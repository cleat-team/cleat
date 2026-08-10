-- ===========================================================================
-- 032: admin.drop_tenant actually deletes the tenant's data
--
-- Finding S3. admin.drop_tenant(p_tenant_id) (001_schema.sql) drops the
-- tenant's plugin schema and role and two admin.* bookkeeping rows
-- (admin.tenant_roles, admin.tenants). It never touched a single row of the
-- tenant's actual workflow data: workflow_instances, event_history,
-- workflow_signals, workflow_schedules, workflow_tags, workflow_routing,
-- workflow_promises, workflow_update_requests, concurrency_keys,
-- admin.tenant_api_keys -- exactly the tables most likely to carry PII
-- (workflow inputs and outputs, signal payloads). Nothing in the Go tree
-- ever called it either (grep -rn "drop_tenant|DropTenant" --include='*.go'
-- . matches only MySQL's unrelated DropTenantDatabase), so this was a
-- GDPR/CCPA erasure request with no code path to honour it at all.
--
-- The real FK graph, checked rather than assumed
-- ------------------------------------------------
-- The engine/db.go doc comment this stream also fixed (see the sibling
-- commit for DeleteDeadLetteredWorkflows) turned out to be wrong about
-- event_history, which is exactly the table a doc-comment-only read of this
-- schema would have gotten wrong here too. Checked directly against
-- pg_constraint on a live database rather than the CREATE TABLE text in
-- 001_schema.sql (which still shows event_history's FK in its original,
-- since-dropped form):
--
--   confrelid = workflow_instances, confdeltype = 'c' (CASCADE):
--     workflow_signals, workflow_promises, concurrency_keys,
--     workflow_update_requests            -- covered by DELETE FROM
--                                             workflow_instances below, no
--                                             explicit delete needed.
--
--   event_history                          -- NO FK AT ALL. 003_procedures.sql
--                                             drops fk_event_history_workflow
--                                             deliberately ("events are
--                                             deleted on terminal", via
--                                             finalize_workflow_status). A
--                                             dead-lettered or still-running
--                                             workflow's events are not
--                                             touched by that path, so they
--                                             must be deleted explicitly here
--                                             or they survive the tenant's
--                                             workflow_instances rows being
--                                             gone -- silently, permanently.
--
--   workflow_tags, workflow_routing        -- FK to workflow_defs (name,
--                                             version), confdeltype = 'a'
--                                             (NO ACTION), nothing to do
--                                             with workflow_instances at
--                                             all. Explicit delete needed.
--
--   workflow_schedules                     -- no FK to workflow_instances,
--                                             own tenant_id column. Explicit
--                                             delete needed.
--
--   admin.tenant_api_keys                  -- FK to admin.tenants,
--                                             confdeltype = 'a' (NO ACTION).
--                                             The original body deleted
--                                             admin.tenants without first
--                                             deleting this table, so any
--                                             tenant that had ever had an
--                                             API key issued would make the
--                                             final DELETE FROM admin.tenants
--                                             fail its own FK constraint --
--                                             loud, not silent, but still a
--                                             bug this migration also fixes.
--                                             Must run before admin.tenants.
--
--   admin.tenant_roles                     -- FK to admin.tenants, same
--                                             confdeltype = 'a'. The
--                                             original body already deleted
--                                             this before admin.tenants;
--                                             kept in the same order.
--
--   idempotency_keys                       -- not in Finding S3's table
--                                             list, but carries a tenant_id
--                                             column (010_idempotency_keys_
--                                             tenant_id.sql) and its `result`
--                                             / `error_msg` columns are a
--                                             workflow's actual output --
--                                             the same PII category the
--                                             finding is about. Added here.
--                                             Not RLS-backed (031's own
--                                             comment explains why: two call
--                                             sites in StartNewRun run before
--                                             any RLS context exists), but
--                                             that reasoning is about a
--                                             fail-closed *policy* breaking
--                                             an ordinary request path --
--                                             it has no bearing on a plain
--                                             DELETE ... WHERE tenant_id = $1
--                                             issued from this function, so
--                                             is included with no gap.
--
--   workflow_defs, plugin_defs             -- deliberately NOT touched.
--                                             DeployWorkflowDef
--                                             (engine/store_deployment.go)
--                                             always writes tenant_id =
--                                             '00000000-...' regardless of
--                                             the calling store's tenant
--                                             (see the tenant_isolation_defs
--                                             policy comment in
--                                             001_schema.sql) -- workflow
--                                             definitions are a shared
--                                             registry today, not
--                                             tenant-owned data, so there is
--                                             nothing under a non-default
--                                             tenant_id to delete, and
--                                             deleting the default tenant's
--                                             rows here would remove every
--                                             tenant's workflow defs. See
--                                             the default-tenant guard below.
--
-- Guard: refuse the default tenant
-- ---------------------------------
-- '00000000-0000-0000-0000-000000000000' (engine.DefaultTenantUUID) is not
-- an ordinary tenant: every single-tenant deployment's data lives under it,
-- and workflow_defs/plugin_defs use it as the shared registry owner
-- unconditionally. Deleting "the default tenant" would silently wipe every
-- workflow, schedule, and signal in a single-tenant deployment while
-- reporting success, which is a worse outcome than the bug this migration
-- fixes. Refused outright rather than left to caller discipline.
--
-- Ordering and atomicity
-- -----------------------
-- A single top-level `SELECT admin.drop_tenant(...)` is one implicit
-- transaction in PostgreSQL, so every DELETE below either all lands or none
-- does. Order: event_history and workflow_instances first (instances'
-- delete cascades the four real-FK child tables), then the
-- FK-to-workflow_defs and no-FK tables, then admin.tenant_api_keys (must
-- precede admin.tenants), then the pre-existing admin.tenant_roles /
-- admin.tenants deletes, unchanged in position.
--
-- RLS
-- ---
-- This function is SECURITY DEFINER, so it runs with the privileges of
-- whichever role owns it -- normally the migration-applying role, which
-- 005_app_role.sql deliberately keeps distinct from cleat_app but which is
-- still subject to `FORCE ROW LEVEL SECURITY` unless it is a superuser.
-- Every DELETE below already carries an explicit `tenant_id = p_tenant_id`
-- predicate, so the policy's own tenant match is redundant for correctness
-- -- but without a tenant context set on this session, a fail-closed
-- `cleat.assert_tenant_set()` policy raises before the predicate is ever
-- evaluated. set_config(..., true) below (transaction-local) makes this
-- function work correctly regardless of whether the connecting/owning role
-- happens to be a superuser, rather than only in the test environment
-- where the schema owner always is one.
-- ===========================================================================

SET search_path = public;

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
