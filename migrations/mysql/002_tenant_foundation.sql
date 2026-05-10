-- cleat MySQL migration 002: tenant foundation
-- Adds multi-tenant data isolation, API key authentication, and a future-proof
-- schema for multi-tenancy.
--
-- MySQL differences from PostgreSQL:
--   - UUID columns become CHAR(36); UUIDs generated in Go application code
--   - gen_random_uuid() DEFAULT is omitted
--   - BYTEA becomes LONGBLOB
--   - BOOLEAN becomes TINYINT(1)
--   - TIMESTAMPTZ becomes TIMESTAMP(6)
--   - Partial indexes (WHERE clause) omitted — MySQL does not support them
--   - RLS (Row-Level Security) is not available in MySQL. Tenant isolation is
--     enforced at the application layer via WHERE tenant_id = ? on every query.
--   - SET SCHEMA / ALTER TABLE SET SCHEMA are omitted — MySQL has no schemas
--   - now() becomes NOW(6)
--   - ON CONFLICT DO NOTHING becomes INSERT IGNORE
--   - ALTER COLUMN SET NOT NULL becomes MODIFY COLUMN ... NOT NULL
--   - DROP COLUMN IF EXISTS — ensure column exists before running
--   - Idempotent: CREATE TABLE IF NOT EXISTS, INSERT IGNORE for default tenant

-- 1. Create tenants metadata table
-- Note: gen_random_uuid() DEFAULT omitted. UUIDs are generated in Go code.
CREATE TABLE IF NOT EXISTS tenants (
    tenant_id          CHAR(36) NOT NULL,
    name               VARCHAR(255) NOT NULL,
    display_name       VARCHAR(255) NOT NULL DEFAULT '',
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    suspended          TINYINT(1) NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id),
    UNIQUE KEY uq_tenants_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 2. Create tenant API keys table
-- Note: gen_random_uuid() DEFAULT omitted. UUIDs generated in Go code.
CREATE TABLE IF NOT EXISTS tenant_api_keys (
    key_id             CHAR(36) NOT NULL,
    tenant_id          CHAR(36) NOT NULL,
    key_hash           LONGBLOB NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    revoked_at         TIMESTAMP(6),
    PRIMARY KEY (key_id),
    FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Note: Partial index WHERE revoked_at IS NULL omitted. MySQL does not support
-- partial indexes. Application-level filtering for revoked_at IS NULL required.
CREATE INDEX idx_api_keys_hash ON tenant_api_keys(key_hash(255));

-- 3-7. Add tenant_id to existing tables
-- ensure column does not exist before running
ALTER TABLE workflow_defs ADD COLUMN tenant_id CHAR(36);
ALTER TABLE workflow_instances ADD COLUMN tenant_id CHAR(36);
ALTER TABLE event_history ADD COLUMN tenant_id CHAR(36);
ALTER TABLE workflow_signals ADD COLUMN tenant_id CHAR(36);
ALTER TABLE workflow_schedules ADD COLUMN tenant_id CHAR(36);

-- 8. Create a default tenant for existing data (idempotent)
INSERT IGNORE INTO tenants (tenant_id, name, display_name)
VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant');

-- 9. Backfill existing rows with default tenant
UPDATE workflow_defs SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_instances SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE event_history SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_signals SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_schedules SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;

-- 10. Make tenant_id NOT NULL (MySQL uses MODIFY COLUMN)
-- ensure column does not exist before running
ALTER TABLE workflow_defs MODIFY COLUMN tenant_id CHAR(36) NOT NULL;
ALTER TABLE workflow_instances MODIFY COLUMN tenant_id CHAR(36) NOT NULL;
ALTER TABLE event_history MODIFY COLUMN tenant_id CHAR(36) NOT NULL;
ALTER TABLE workflow_signals MODIFY COLUMN tenant_id CHAR(36) NOT NULL;
ALTER TABLE workflow_schedules MODIFY COLUMN tenant_id CHAR(36) NOT NULL;

-- 11. Composite indexes for tenant-scoped queries
-- Note: DROP INDEX IF EXISTS not supported in MySQL. Ensure indexes exist before dropping.
DROP INDEX idx_defs_active ON workflow_defs;
CREATE INDEX idx_defs_tenant_name_version ON workflow_defs(tenant_id, name, version);

DROP INDEX idx_instances_ready ON workflow_instances;
-- Note: Partial index WHERE status = 'ready' omitted.
CREATE INDEX idx_instances_tenant_ready ON workflow_instances(tenant_id, status, next_wake_at);

DROP INDEX idx_instances_heartbeat ON workflow_instances;
-- Note: Partial index WHERE status = 'running' omitted.
CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at);

DROP INDEX idx_instances_stale ON workflow_instances;
-- Note: Partial index WHERE status = 'running' omitted.
CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at);

CREATE INDEX idx_event_history_tenant_wf ON event_history(tenant_id, workflow_id, step);

CREATE INDEX idx_signals_tenant_wf ON workflow_signals(tenant_id, workflow_id, signal_name);

CREATE INDEX idx_schedules_tenant_enabled ON workflow_schedules(tenant_id, enabled, next_run_at);

-- 12. Note: namespace columns are kept alongside tenant_id for compatibility.
-- The PostgresStore references namespace in claim queries and the interface
-- passes namespace as a parameter. These can be dropped when fully deprecated.
-- ALTER TABLE workflow_defs DROP COLUMN IF EXISTS namespace;
-- ALTER TABLE workflow_instances DROP COLUMN IF EXISTS namespace;

-- 13. RLS policies omitted — MySQL does not support Row-Level Security.
-- Tenant isolation is enforced at the application layer via WHERE tenant_id = ?
-- on every query. See internal/host/mysql_store.go for implementation.

-- 14. SET SCHEMA omitted — MySQL has no schema concept. Tables remain in the
-- current database with their original names.
