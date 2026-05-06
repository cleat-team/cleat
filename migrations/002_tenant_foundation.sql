-- cleat tenant foundation migration
-- Adds multi-tenant data isolation, API key authentication, and a future-proof
-- schema for multi-tenancy.
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS where applicable.

-- 1. Create tenants metadata table
CREATE TABLE IF NOT EXISTS tenants (
    tenant_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    suspended   BOOLEAN NOT NULL DEFAULT false
);

-- 2. Create tenant API keys table
CREATE TABLE IF NOT EXISTS tenant_api_keys (
    key_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(tenant_id),
    key_hash    BYTEA NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON tenant_api_keys(key_hash) WHERE revoked_at IS NULL;

-- 3. Add tenant_id to workflow_defs
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS tenant_id UUID;
-- 4. Add tenant_id to workflow_instances
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS tenant_id UUID;
-- 5. Add tenant_id to event_history
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS tenant_id UUID;
-- 6. Add tenant_id to workflow_signals
ALTER TABLE workflow_signals ADD COLUMN IF NOT EXISTS tenant_id UUID;
-- 7. Add tenant_id to workflow_schedules
ALTER TABLE workflow_schedules ADD COLUMN IF NOT EXISTS tenant_id UUID;

-- 8. Create a default tenant for existing data (idempotent)
INSERT INTO tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant')
    ON CONFLICT (tenant_id) DO NOTHING;

-- 9. Backfill existing rows with default tenant
UPDATE workflow_defs SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_instances SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE event_history SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_signals SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_schedules SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;

-- 10. Make tenant_id NOT NULL
ALTER TABLE workflow_defs ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE workflow_instances ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE event_history ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE workflow_signals ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE workflow_schedules ALTER COLUMN tenant_id SET NOT NULL;

-- 11. Composite indexes for tenant-scoped queries
-- workflow_defs: replace/update existing indexes
DROP INDEX IF EXISTS idx_defs_active;
CREATE INDEX IF NOT EXISTS idx_defs_tenant_name_version ON workflow_defs(tenant_id, name, version DESC);

-- workflow_instances: tenant-scoped ready queue
DROP INDEX IF EXISTS idx_instances_ready;
CREATE INDEX IF NOT EXISTS idx_instances_tenant_ready
    ON workflow_instances(tenant_id, status, next_wake_at) WHERE status = 'ready';

-- Also keep the non-tenant heartbeat and stale indexes
DROP INDEX IF EXISTS idx_instances_heartbeat;
CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at) WHERE status = 'running';

DROP INDEX IF EXISTS idx_instances_stale;
CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances(status, heartbeat_at) WHERE status = 'running';

-- event_history: tenant-scoped PK equivalent index
CREATE INDEX IF NOT EXISTS idx_event_history_tenant_wf ON event_history(tenant_id, workflow_id, step);

-- workflow_signals: tenant-scoped lookup
CREATE INDEX IF NOT EXISTS idx_signals_tenant_wf ON workflow_signals(tenant_id, workflow_id, signal_name);

-- workflow_schedules: tenant-scoped due lookup
CREATE INDEX IF NOT EXISTS idx_schedules_tenant_enabled ON workflow_schedules(tenant_id, enabled, next_run_at);

-- 12. Drop old namespace columns (replaced by tenant_id)
ALTER TABLE workflow_defs DROP COLUMN IF EXISTS namespace;
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS namespace;

-- 13. Enable Row-Level Security (defense in depth)
ALTER TABLE workflow_defs ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_signals ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_schedules ENABLE ROW LEVEL SECURITY;

-- Policies use the session variable cleat.tenant_id
-- If not set, rows are invisible (fail-closed)
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
