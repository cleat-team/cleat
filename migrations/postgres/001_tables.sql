-- cleat postgres tables

CREATE SCHEMA IF NOT EXISTS admin;
CREATE SCHEMA IF NOT EXISTS cleat;
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
            RAISE WARNING 'create_tenant_role: cannot create role % (SQLSTATE: %) — skipping (single-tenant mode)', v_role_name, SQLSTATE;
            RETURN NULL;
        END;
    ELSE
        -- Role already exists (e.g. after database drop/recreate).
        -- Sync the password so admin.tenant_roles and pg_roles agree.
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
CREATE EXTENSION IF NOT EXISTS pgcrypto;
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
CREATE TABLE IF NOT EXISTS workflow_defs (
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    wasm_bytes BYTEA NOT NULL,
    entry_points TEXT[] NOT NULL DEFAULT '{}',
    min_version INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    max_history_length INTEGER NOT NULL DEFAULT 0,
    dag_spec JSONB DEFAULT NULL,
    tenant_id UUID,
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
    tenant_id UUID,
    task_queue TEXT NOT NULL DEFAULT 'default',
    compaction_state JSONB,
    compacted_at TIMESTAMPTZ,
    compaction_step INTEGER,
    plugin_vers JSONB NOT NULL DEFAULT '{}',
    event_count BIGINT NOT NULL DEFAULT 0,
    allowed_signals JSONB DEFAULT NULL,
    FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
);
CREATE TABLE IF NOT EXISTS event_history (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
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
    tenant_id UUID,
    payload JSONB,
    checksum TEXT,

    thread_id TEXT NOT NULL DEFAULT 'main',
    local_step INTEGER NOT NULL DEFAULT 0,
    global_seq BIGINT NOT NULL DEFAULT 0,
    CONSTRAINT event_history_pkey PRIMARY KEY (workflow_id, step)
);
CREATE TABLE IF NOT EXISTS workflow_signals (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    signal_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID,
    PRIMARY KEY (workflow_id, signal_name)
);
CREATE TABLE IF NOT EXISTS workflow_promises (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    promise_id TEXT NOT NULL,
    promise_name TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, promise_id)
);
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
    tenant_id UUID
);
CREATE TABLE IF NOT EXISTS concurrency_keys (
    key_hash BYTEA PRIMARY KEY,
    key_text TEXT NOT NULL,
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    tenant_id UUID
);
CREATE TABLE IF NOT EXISTS idempotency_keys (
    key_hash    BYTEA NOT NULL PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    result      JSONB,
    error_msg   TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '7 days'
);
CREATE TABLE IF NOT EXISTS workflow_update_requests (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
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
