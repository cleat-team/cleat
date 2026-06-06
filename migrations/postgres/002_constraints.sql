-- cleat postgres constraints, indexes, functions

CREATE OR REPLACE FUNCTION admin.grant_plugin_to_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_table_name TEXT;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'grant_plugin_to_tenant: no role for tenant % — skipping (single-tenant mode)', p_tenant_id;
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
        RAISE WARNING 'revoke_plugin_from_tenant: no role for tenant % — skipping (single-tenant mode)', p_tenant_id;
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
CREATE INDEX IF NOT EXISTS idx_instances_ready ON workflow_instances(status, next_wake_at) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_defs_active ON workflow_defs(name, version DESC);
CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_promises_status ON workflow_promises(workflow_id, status);
CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys(workflow_id);
CREATE INDEX IF NOT EXISTS idx_instances_sticky ON workflow_instances(sticky_worker_id) WHERE sticky_worker_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_update_requests_pending ON workflow_update_requests(workflow_id, status);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON admin.tenant_api_keys(key_hash) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_defs_tenant_name_version ON workflow_defs(tenant_id, name, version DESC);
CREATE INDEX IF NOT EXISTS idx_instances_tenant_ready
    ON workflow_instances(tenant_id, status, next_wake_at) WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_event_history_tenant_wf ON event_history(tenant_id, workflow_id, step);
CREATE INDEX IF NOT EXISTS idx_signals_tenant_wf ON workflow_signals(tenant_id, workflow_id, signal_name);
CREATE INDEX IF NOT EXISTS idx_schedules_tenant_enabled ON workflow_schedules(tenant_id, enabled, next_run_at);
CREATE INDEX IF NOT EXISTS idx_instances_tenant_queue_ready
    ON workflow_instances(tenant_id, task_queue, status, next_wake_at)
    WHERE status = 'ready';
CREATE INDEX IF NOT EXISTS idx_idempotency_workflow_id
    ON idempotency_keys(workflow_id);
CREATE INDEX IF NOT EXISTS idx_idempotency_expires
    ON idempotency_keys(expires_at);
CREATE INDEX IF NOT EXISTS idx_mem_samples_def
    ON workflow_memory_samples (def_name, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_instances_created_at ON workflow_instances(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_instances_terminal_completed ON workflow_instances(tenant_id, status, completed_at) WHERE status IN ('done','failed');
CREATE INDEX IF NOT EXISTS idx_concurrency_keys_expires ON concurrency_keys(expires_at);
CREATE INDEX IF NOT EXISTS idx_instances_parent_policy ON workflow_instances(parent_workflow_id, parent_close_policy, status);
CREATE POLICY tenant_isolation_defs ON workflow_defs
    FOR ALL USING (tenant_id = COALESCE(current_setting('cleat.tenant_id', true), '00000000-0000-0000-0000-000000000000')::uuid);
CREATE POLICY tenant_isolation_instances ON workflow_instances
    FOR ALL USING (tenant_id = COALESCE(current_setting('cleat.tenant_id', true), '00000000-0000-0000-0000-000000000000')::uuid);
CREATE POLICY tenant_isolation_events ON event_history
    FOR ALL USING (tenant_id = COALESCE(current_setting('cleat.tenant_id', true), '00000000-0000-0000-0000-000000000000')::uuid);
CREATE POLICY tenant_isolation_signals ON workflow_signals
    FOR ALL USING (tenant_id = COALESCE(current_setting('cleat.tenant_id', true), '00000000-0000-0000-0000-000000000000')::uuid);
CREATE POLICY tenant_isolation_schedules ON workflow_schedules
    FOR ALL USING (tenant_id = COALESCE(current_setting('cleat.tenant_id', true), '00000000-0000-0000-0000-000000000000')::uuid);
DROP INDEX IF EXISTS idx_defs_active;
DROP INDEX IF EXISTS idx_instances_ready;
DROP INDEX IF EXISTS idx_instances_heartbeat;
DROP INDEX IF EXISTS idx_instances_stale;
ALTER TABLE workflow_defs DROP COLUMN IF EXISTS namespace;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS namespace;
ALTER TABLE workflow_defs ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS allowed_signals JSONB DEFAULT NULL;
ALTER TABLE workflow_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_signals ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_schedules ENABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_instances_ready;
DROP INDEX IF EXISTS idx_instances_tenant_ready;
-- Migration 012: Add GIN index on workflow_instances.input for full-text JSONB search.
-- Enables efficient JSONB containment queries (e.g., WHERE input @> '{"field":"value"}').
CREATE INDEX IF NOT EXISTS idx_instances_input_gin
  ON workflow_instances
  USING GIN (input jsonb_path_ops);

