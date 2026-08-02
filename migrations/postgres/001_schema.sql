-- cleat consolidated schema (001)
-- Combines: 001_tables, 002_constraints, 005_priority, 006_priority_promises_updates,
--           007_event_history_cascade, 007_fk_cascade, 008_rls_fail_closed, 009_generation,
--           010_workflow_tags_routing, 011_update_requests_tenant_id, 015_claim_workflows_index
--
-- All CREATE TABLE statements include the final column set.  No ALTER TABLE ADD COLUMN.
-- All FOREIGN KEYs referencing workflow_instances include ON DELETE CASCADE inline.
-- RLS policies use cleat.assert_tenant_set() (fail-closed).  No COALESCE-based policies.
-- All admin functions use CREATE OR REPLACE.  All tables/indexes use idempotent guards.

-- ── Extensions & schemas ──────────────────────────────────────────────────────
-- Pin the creation target. The default search_path is "$user", public, so
-- every unqualified CREATE below lands in a schema named after the connecting
-- role whenever such a schema exists. This file creates a schema called
-- "cleat", and docker-compose.cluster.yml connects as POSTGRES_USER=cleat --
-- so the entire schema was being built inside the "cleat" schema instead of
-- public. Verified on PostgreSQL 16: 14 tables and finalize_workflow_status
-- all landed in "cleat", psql still found them via "$user" so it looked
-- healthy, and anything addressing public.* failed (create_tenant_role's
-- GRANTs on public.workflow_defs among them). Without this line the shipped
-- cluster deployment is broken by nothing more than its own username.
SET search_path = public;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE SCHEMA IF NOT EXISTS admin;
CREATE SCHEMA IF NOT EXISTS cleat;

-- ── RLS assert function (fail-closed) ─────────────────────────────────────────
CREATE OR REPLACE FUNCTION cleat.assert_tenant_set()
RETURNS uuid AS $$
DECLARE
    tid text;
BEGIN
    tid := current_setting('cleat.tenant_id', true);
    IF tid IS NULL THEN
        RAISE EXCEPTION 'cleat.tenant_id is not set -- tenant context required for RLS-scoped query';
    END IF;
    RETURN tid::uuid;
END;
$$ LANGUAGE plpgsql;

-- ── Admin functions ──────────────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION admin.create_tenant_role(p_tenant_id UUID) RETURNS TEXT AS $$
DECLARE
    v_role_name TEXT;
    v_password TEXT;
BEGIN
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');
    v_password := encode(gen_random_bytes(32), 'hex');

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        BEGIN
            EXECUTE format(
                'CREATE ROLE %I WITH LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT CONNECTION LIMIT 10',
                v_role_name, v_password
            );
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot create role % (SQLSTATE: %) -- skipping (single-tenant mode)', v_role_name, SQLSTATE;
            RETURN NULL;
        END;
    ELSE
        BEGIN
            EXECUTE format('ALTER ROLE %I WITH PASSWORD %L', v_role_name, v_password);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot alter password for role % (SQLSTATE: %)', v_role_name, SQLSTATE;
        END;
    END IF;

    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I AUTHORIZATION %I',
        'tenant_' || replace(p_tenant_id::text, '-', '_'), v_role_name);

    EXECUTE format('ALTER ROLE %I SET search_path = %L, public', v_role_name,
        'tenant_' || replace(p_tenant_id::text, '-', '_'));
    EXECUTE format('ALTER ROLE %I SET cleat.tenant_id = %L', v_role_name, p_tenant_id);

    EXECUTE format('GRANT USAGE ON SCHEMA public TO %I', v_role_name);
    EXECUTE format('GRANT USAGE ON SCHEMA admin TO %I', v_role_name);

    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_defs TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_instances TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.event_history TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_signals TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_schedules TO %I', v_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.workflow_promises TO %I', v_role_name);

    INSERT INTO admin.tenant_roles (tenant_id, role_name, password)
    VALUES (p_tenant_id, v_role_name, v_password)
    ON CONFLICT (tenant_id) DO UPDATE SET
        role_name = EXCLUDED.role_name,
        password  = EXCLUDED.password;

    RETURN v_role_name;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

CREATE OR REPLACE FUNCTION admin.grant_plugin_to_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_table_name TEXT;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'grant_plugin_to_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');

    FOR v_table_name IN
        SELECT t.table_name FROM admin.plugin_tables t WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO %I',
            v_schema_name, v_table_name, v_role_name);
    END LOOP;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

CREATE OR REPLACE FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_table_name TEXT;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'revoke_plugin_from_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');

    FOR v_table_name IN
        SELECT t.table_name FROM admin.plugin_tables t WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('REVOKE ALL ON %I.%I FROM %I',
            v_schema_name, v_table_name, v_role_name);
    END LOOP;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

CREATE OR REPLACE FUNCTION admin.drop_tenant(p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
BEGIN
    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_schema_name);
    EXECUTE format('DROP ROLE IF EXISTS %I', v_role_name);

    DELETE FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    DELETE FROM admin.tenants WHERE tenant_id = p_tenant_id;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

-- ── Admin tables ─────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS admin.tenants (
    tenant_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    suspended   BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS admin.plugin_tables (
    plugin_name TEXT NOT NULL,
    table_name  TEXT NOT NULL,
    PRIMARY KEY (plugin_name, table_name)
);

CREATE TABLE IF NOT EXISTS admin.tenant_roles (
    tenant_id UUID PRIMARY KEY REFERENCES admin.tenants(tenant_id),
    role_name TEXT NOT NULL UNIQUE,
    password  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS admin.tenant_api_keys (
    key_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES admin.tenants(tenant_id),
    key_hash    BYTEA NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

-- ── Workflow definition tables ──────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workflow_defs (
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    wasm_bytes BYTEA NOT NULL,
    entry_points TEXT[] NOT NULL DEFAULT '{}',
    min_version INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    max_history_length INTEGER NOT NULL DEFAULT 0,
    dag_spec JSONB DEFAULT NULL,
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    task_queue TEXT NOT NULL DEFAULT 'default',
    abi_version INTEGER NOT NULL DEFAULT 1,
    plugin_deps JSONB NOT NULL DEFAULT '{}',
    deprecated BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (name, version)
);

CREATE TABLE IF NOT EXISTS plugin_defs (
    name         TEXT        NOT NULL,
    version      TEXT        NOT NULL,
    wasm_bytes   BYTEA,
    config       JSONB       NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deprecated   BOOLEAN     NOT NULL DEFAULT false,
    PRIMARY KEY (name, version)
);

-- ── Workflow instances ──────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workflow_instances (
    id TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    def_version INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'ready',
    input JSONB NOT NULL DEFAULT '{}',
    assigned_to TEXT,
    heartbeat_at TIMESTAMPTZ,
    next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    cancellation_requested BOOLEAN NOT NULL DEFAULT false,
    cancellation_reason TEXT,
    result JSONB,
    error_msg TEXT,
    error_code TEXT,
    error_op TEXT,
    parent_workflow_id TEXT,
    parent_close_policy TEXT DEFAULT 'ABANDON',
    query_state JSONB DEFAULT '{}',
    trace_id TEXT,
    sticky_worker_id TEXT,
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    task_queue TEXT NOT NULL DEFAULT 'default',
    compaction_state JSONB,
    compacted_at TIMESTAMPTZ,
    compaction_step INTEGER,
    plugin_vers JSONB NOT NULL DEFAULT '{}',
    event_count BIGINT NOT NULL DEFAULT 0,
    allowed_signals JSONB DEFAULT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    generation BIGINT NOT NULL DEFAULT 0,
    FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
);

-- ── Event history (FK named for compatibility with cleanup procedure) ───────

CREATE TABLE IF NOT EXISTS event_history (
    workflow_id TEXT NOT NULL,
    step INTEGER NOT NULL,
    service TEXT,
    operation TEXT,
    request TEXT,
    response TEXT,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type TEXT NOT NULL DEFAULT 'call',
    duration_ms BIGINT,
    signal_names TEXT,
    timeout_ms BIGINT,
    signal_name TEXT,
    signal_payload TEXT,
    defer_description TEXT,
    defer_id TEXT,
    child_name TEXT,
    child_input TEXT,
    run_id TEXT,
    new_input TEXT,
    plugin_name TEXT,
    plugin_func TEXT,
    plugin_input TEXT,
    plugin_output TEXT,
    plugin_error TEXT,
    promise_name TEXT,
    promise_id TEXT,
    promise_result TEXT,
    promise_error TEXT,
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    payload JSONB,
    checksum TEXT,

    thread_id TEXT NOT NULL DEFAULT 'main',
    local_step INTEGER NOT NULL DEFAULT 0,
    global_seq BIGINT NOT NULL DEFAULT 0,
    CONSTRAINT event_history_pkey PRIMARY KEY (workflow_id, step),
    CONSTRAINT fk_event_history_workflow FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE
);

-- ── Workflow signals ────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workflow_signals (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id) ON DELETE CASCADE,
    signal_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    PRIMARY KEY (workflow_id, signal_name)
);

-- ── Workflow promises ───────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workflow_promises (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id) ON DELETE CASCADE,
    promise_id TEXT NOT NULL,
    promise_name TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    PRIMARY KEY (workflow_id, promise_id)
);

-- ── Workflow schedules ──────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workflow_schedules (
    name TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    entry_point TEXT NOT NULL DEFAULT '',
    cron_expression TEXT NOT NULL,
    input JSONB NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT true,
    next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_run_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'
);

-- ── Concurrency keys ────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS concurrency_keys (
    key_hash BYTEA PRIMARY KEY,
    key_text TEXT NOT NULL,
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id) ON DELETE CASCADE,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'
);

-- ── Idempotency keys ────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key_hash    BYTEA NOT NULL PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    result      JSONB,
    error_msg   TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '7 days'
);

-- ── Workflow update requests ────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workflow_update_requests (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id) ON DELETE CASCADE,
    update_name TEXT NOT NULL,
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    priority INTEGER NOT NULL DEFAULT 0,
    payload JSONB NOT NULL DEFAULT '{}',
    promise_id TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, update_name)
);

-- ── Memory tracking tables ──────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workflow_memory_samples (
    id            BIGSERIAL PRIMARY KEY,
    def_name      TEXT NOT NULL,
    sample_bytes  BIGINT NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workflow_memory_stats (
    def_name      TEXT PRIMARY KEY,
    mean_bytes    DOUBLE PRECISION NOT NULL DEFAULT 0,
    sample_count  INTEGER NOT NULL DEFAULT 0,
    alpha         DOUBLE PRECISION NOT NULL DEFAULT 0.3,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── Workflow tags and routing (010) ─────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workflow_tags (
    workflow_name TEXT NOT NULL,
    version INTEGER NOT NULL,
    tag TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    PRIMARY KEY (workflow_name, tag),
    FOREIGN KEY (workflow_name, version) REFERENCES workflow_defs(name, version)
);

CREATE TABLE IF NOT EXISTS workflow_routing (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_name TEXT NOT NULL,
    target_version INTEGER NOT NULL,
    weight REAL NOT NULL DEFAULT 1.0 CHECK (weight >= 0 AND weight <= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    FOREIGN KEY (workflow_name, target_version) REFERENCES workflow_defs(name, version)
);

-- ── Indexes (final definitions only; no DROP/RECREATE) ──────────────────────

-- General instance indexes
CREATE INDEX IF NOT EXISTS idx_instances_ready ON workflow_instances(status, next_wake_at) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_instances_sticky ON workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_instances_parent_policy ON workflow_instances(parent_workflow_id, parent_close_policy, status);

-- Definition indexes
CREATE INDEX IF NOT EXISTS idx_defs_active ON workflow_defs(name, version DESC);
CREATE INDEX IF NOT EXISTS idx_defs_tenant_name_version ON workflow_defs(tenant_id, name, version DESC);

-- Promise / concurrency / idempotency indexes
CREATE INDEX IF NOT EXISTS idx_promises_status ON workflow_promises(workflow_id, status);
CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id);
CREATE INDEX IF NOT EXISTS idx_concurrency_keys_expires ON concurrency_keys(expires_at);
CREATE INDEX IF NOT EXISTS idx_idempotency_workflow_id ON idempotency_keys(workflow_id);
CREATE INDEX IF NOT EXISTS idx_idempotency_expires ON idempotency_keys(expires_at);

-- Update requests
CREATE INDEX IF NOT EXISTS idx_update_requests_pending ON workflow_update_requests(workflow_id, status);

-- API key
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON admin.tenant_api_keys(key_hash) WHERE revoked_at IS NULL;

-- Tenant-scoped indexes
CREATE INDEX IF NOT EXISTS idx_instances_tenant_ready ON workflow_instances(tenant_id, status, next_wake_at) WHERE status = 'ready';

-- Tenant + queue + priority ordering (final version from 005)
CREATE INDEX IF NOT EXISTS idx_instances_tenant_queue_ready
    ON workflow_instances(tenant_id, task_queue, status, priority ASC, next_wake_at)
    WHERE status = 'ready';

-- Tenant + queue + priority + created_at for claim ordering (from 015)
CREATE INDEX IF NOT EXISTS idx_instances_ready_claim
    ON workflow_instances (tenant_id, task_queue, priority, created_at)
    WHERE status = 'ready';

-- Tenant-scoped lookups
CREATE INDEX IF NOT EXISTS idx_event_history_tenant_wf ON event_history(tenant_id, workflow_id, step);
CREATE INDEX IF NOT EXISTS idx_signals_tenant_wf ON workflow_signals(tenant_id, workflow_id, signal_name);
CREATE INDEX IF NOT EXISTS idx_schedules_tenant_enabled ON workflow_schedules(tenant_id, enabled, next_run_at);
CREATE INDEX IF NOT EXISTS idx_instances_created_at ON workflow_instances(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_instances_terminal_completed
    ON workflow_instances(tenant_id, status, completed_at)
    WHERE status IN ('done','failed');

-- Memory sample lookups
CREATE INDEX IF NOT EXISTS idx_mem_samples_def ON workflow_memory_samples (def_name, recorded_at DESC);

-- GIN index on input for JSONB containment queries
CREATE INDEX IF NOT EXISTS idx_instances_input_gin
    ON workflow_instances
    USING GIN (input jsonb_path_ops);

-- ── Row-Level Security ──────────────────────────────────────────────────────

ALTER TABLE workflow_defs ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_signals ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_routing ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_promises ENABLE ROW LEVEL SECURITY;

-- Fail-closed policies using cleat.assert_tenant_set() (from 008, no COALESCE)
--
-- workflow_defs is a partial exception: its PRIMARY KEY is (name, version)
-- with no tenant_id component, and DeployWorkflowDef (engine/store_deployment.go)
-- always writes tenant_id = '00000000-...' (DefaultTenantUUID) regardless of
-- the calling store's tenant, because workflow definitions are a
-- shared/global registry, not tenant-partitioned data (see the "visible to
-- all tenants" comment on TestTenantSelfAccess in
-- engine/tenant_isolation_test.go). A strict tenant_id = assert_tenant_set()
-- policy here would make DeployWorkflowDef fail-closed for every tenant
-- except the default one as soon as RLS is enforced by a non-superuser,
-- non-owner connection, which is the opposite of the intended behavior.
-- The policy below therefore also allows the shared default-tenant rows to
-- be read (and written, matching what DeployWorkflowDef already does
-- unconditionally today) by any tenant.
DROP POLICY IF EXISTS tenant_isolation_defs ON workflow_defs;
CREATE POLICY tenant_isolation_defs ON workflow_defs
    FOR ALL USING (
        tenant_id = cleat.assert_tenant_set()
        OR tenant_id = '00000000-0000-0000-0000-000000000000'
    );

DROP POLICY IF EXISTS tenant_isolation_instances ON workflow_instances;
CREATE POLICY tenant_isolation_instances ON workflow_instances
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_events ON event_history;
CREATE POLICY tenant_isolation_events ON event_history
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_signals ON workflow_signals;
CREATE POLICY tenant_isolation_signals ON workflow_signals
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_schedules ON workflow_schedules;
CREATE POLICY tenant_isolation_schedules ON workflow_schedules
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_promises ON workflow_promises;
CREATE POLICY tenant_isolation_promises ON workflow_promises
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_tags ON workflow_tags;
CREATE POLICY tenant_isolation_tags ON workflow_tags
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_routing ON workflow_routing;
CREATE POLICY tenant_isolation_routing ON workflow_routing
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

-- FORCE ROW LEVEL SECURITY: without this, RLS is silently bypassed for the
-- table OWNER (in addition to superusers, who bypass RLS unconditionally
-- and cannot be forced). Since the role that runs migrations is normally
-- also the role the worker connects as -- i.e. the table owner -- omitting
-- FORCE means the policies above enforce nothing for that role. FORCE does
-- NOT help against a superuser connection (Postgres never applies RLS to
-- superusers, forced or not); a non-superuser, non-owner connecting role is
-- the only way to get real enforcement from a superuser-provisioned
-- database. See docs/reference/multi-tenancy.md.
ALTER TABLE workflow_defs FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_instances FORCE ROW LEVEL SECURITY;
ALTER TABLE event_history FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_signals FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_schedules FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_tags FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_routing FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_promises FORCE ROW LEVEL SECURITY;
