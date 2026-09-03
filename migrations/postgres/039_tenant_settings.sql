-- cleat migration 039 (postgres): per-tenant execution limits.
--
-- IMPROVEMENT-PLAN 3.94 step 3. Every execution limit cleat has is a worker
-- flag and therefore process-wide: one `--wasm-wall-clock-ceiling` for every
-- tenant sharing the deployment. The requirement (2026-09-03) is that several
-- microservices, or several organisations, sharing one cleat can each manage
-- their own settings without affecting the others. This table is where a
-- tenant's overrides live.
--
-- ---------------------------------------------------------------------------
-- NULL means "no override", and that is not the same as zero.
--
-- Zero is already meaningful for every one of these: the executor tests
-- `if ceiling > 0` before applying a bound, so zero means UNBOUNDED. If absent
-- overrides were stored as 0 the table could not express "leave this one to the
-- operator" at all, and worse, a row created to set one field would silently
-- remove the bound on the other two. Nullable columns with the CHECKs below
-- make the two states distinct and make the dangerous one unrepresentable.
--
-- ---------------------------------------------------------------------------
-- The CHECK constraints are a privilege boundary, not input validation.
--
-- A tenant may only ever TIGHTEN a limit -- resolution clamps its value to the
-- operator's flag (engine/tenant_settings.go, ClampToCeiling). That property
-- depends on the stored value being positive: with zero or a negative allowed,
-- a tenant writes 0, resolution reads "unbounded", and the clamp it was
-- supposed to pass through hands it MORE than the operator granted. That is
-- the one direction this feature must not permit, so it is refused by the
-- database rather than by whichever read path happens to be looking.
--
-- ---------------------------------------------------------------------------
-- Deletion is by foreign key rather than by extending admin.drop_tenant.
--
-- admin.drop_tenant (032_drop_tenant_deletes_tenant_data.sql) enumerates
-- fourteen tables by hand, so a fifteenth is a line someone must remember to
-- add. ON DELETE CASCADE cannot be forgotten, and it composes correctly with
-- that function anyway: drop_tenant deletes admin.tenants LAST, so the cascade
-- fires at the end of the pass exactly as an explicit DELETE would have.
-- Referential-integrity actions are not subject to row-level security, so the
-- cascade is unaffected by the policy below -- asserted rather than assumed, in
-- engine/tenant_settings_rls_test.go.

-- Pin the creation target, exactly as 001_schema.sql through 004 do, and for
-- the reason 001's own header gives: the default search_path is
-- `"$user", public`, so every unqualified CREATE lands in a schema named after
-- the connecting role whenever such a schema exists. 001 creates a schema
-- called "cleat" (it holds assert_tenant_set), and ci.yml's cluster and Tier 1
-- jobs -- like docker-compose.cluster.yml -- connect as role "cleat". Without
-- this line the table below is created in the "cleat" schema while all fifteen
-- of its neighbours sit in public.
--
-- Measured 2026-09-03 against a database owned by role "cleat", before the line
-- was added, and deterministic across two fresh databases:
--
--   cleat.tenant_settings   public.workflow_instances   public.workflow_tags
--
-- The symptom is not a migration error -- the migration succeeds. It is
-- `relation "tenant_settings" does not exist` from application code whose
-- search_path resolves differently, which reads like a migration that never
-- ran. 001's header records the same trap costing "the shipped cluster
-- deployment is broken by nothing more than its own username".
--
-- Only three PostgreSQL migrations create a table -- 001, 032 and this one --
-- which is why 006..038 get away without it: ALTER on an unqualified name
-- falls through "cleat" (where no such table exists) to public, while CREATE
-- stops at the first entry.
SET search_path = public;

CREATE TABLE IF NOT EXISTS tenant_settings (
    tenant_id                  UUID PRIMARY KEY
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,

    -- Guest EXECUTION time, the epoch fence (3.90). NULL = use the operator's
    -- --wasm-instance-timeout.
    wasm_instance_timeout_ms   BIGINT,

    -- WALL CLOCK for one invocation including host wait (3.90). NULL = use the
    -- operator's --wasm-wall-clock-ceiling.
    wasm_wall_clock_ceiling_ms BIGINT,

    -- The threshold above which a retry policy is suspended rather than run in
    -- the host (3.88). NULL = use the deployment default.
    host_retry_budget_ms       BIGINT,

    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_tenant_settings_instance_timeout_positive
        CHECK (wasm_instance_timeout_ms IS NULL OR wasm_instance_timeout_ms > 0),
    CONSTRAINT ck_tenant_settings_wall_clock_positive
        CHECK (wasm_wall_clock_ceiling_ms IS NULL OR wasm_wall_clock_ceiling_ms > 0),
    CONSTRAINT ck_tenant_settings_retry_budget_positive
        CHECK (host_retry_budget_ms IS NULL OR host_retry_budget_ms > 0)
);

-- Row-level security, in the same shape as the eight tables 001_schema.sql
-- protects and the two 031 added. A settings table without it would be the
-- worst placement of the gap: every tenant could read and rewrite every other
-- tenant's limits, which is precisely the isolation this feature is for.
--
-- FORCE is what makes it real for the owner. Without it RLS is silently
-- bypassed for the table owner, and the role that runs migrations normally owns
-- what it creates -- see 001_schema.sql's note, and IMPROVEMENT-PLAN 1.10 for
-- the case where correct policies were bypassed by every connection that had
-- ever run against them.
ALTER TABLE tenant_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_settings FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_settings ON tenant_settings;
CREATE POLICY tenant_isolation_settings ON tenant_settings
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

-- No GRANT statement here, deliberately. 005_app_role.sql issues
-- `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE,
-- DELETE ON TABLES TO cleat_app`, which covers tables the migration role
-- creates afterwards -- this one included. That is a claim about PostgreSQL
-- behaviour rather than something visible in this file, so
-- engine/tenant_settings_rls_test.go reads the table as cleat_app instead of
-- taking it on trust.
