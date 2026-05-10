-- cleat MySQL migration 009: tenant roles and admin tables
-- Creates tenant metadata tables previously in the PostgreSQL admin schema.
--
-- MySQL differences from PostgreSQL:
--   - No schema concept: tables live in the current database, not in an
--     "admin" schema. All table names remain as-is (tenants, tenant_api_keys,
--     plugin_tables, tenant_roles).
--   - UUID columns become CHAR(36); UUIDs generated in Go application code
--   - gen_random_uuid() DEFAULT is omitted
--   - BYTEA becomes LONGBLOB
--   - BOOLEAN becomes TINYINT(1)
--   - TIMESTAMPTZ becomes TIMESTAMP(6)
--   - now() becomes NOW(6)
--   - SCHEMA creation/ALTER TABLE SET SCHEMA omitted entirely
--   - All PL/pgSQL functions (create_tenant_role, grant_plugin_to_tenant,
--     revoke_plugin_from_tenant, drop_tenant) replaced by Go application code
--   - The DO $$ backfill block is omitted
--   - Idempotent: CREATE TABLE IF NOT EXISTS

-- 1. Tenants table (stays in current database, no admin schema)
-- Note: gen_random_uuid() DEFAULT omitted. UUIDs generated in Go code.
CREATE TABLE IF NOT EXISTS tenants (
    tenant_id          CHAR(36) NOT NULL,
    name               VARCHAR(255) NOT NULL,
    display_name       VARCHAR(255) NOT NULL DEFAULT '',
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    suspended          TINYINT(1) NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id),
    UNIQUE KEY uq_tenants_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 2. Tenant API keys table (stays in current database)
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

-- 3. Table to track plugin tables per plugin (for GRANT management).
--    In MySQL, grants are managed at the application level rather than via
--    PostgreSQL-style role-based table grants.
CREATE TABLE IF NOT EXISTS plugin_tables (
    plugin_name        VARCHAR(255) NOT NULL,
    table_name         VARCHAR(255) NOT NULL,
    PRIMARY KEY (plugin_name, table_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 4. Table to store tenant role credentials.
--    In MySQL, tenant roles are managed by Go application code.
CREATE TABLE IF NOT EXISTS tenant_roles (
    tenant_id          CHAR(36) NOT NULL,
    role_name          VARCHAR(255) NOT NULL,
    password           TEXT NOT NULL,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (tenant_id),
    UNIQUE KEY uq_tenant_roles_role_name (role_name),
    FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- PL/pgSQL functions omitted
-- ---------------------------------------------------------------------------
-- The following PostgreSQL functions are replaced by Go application code
-- in internal/host/mysql_store.go:
--
--   admin.create_tenant_role(p_tenant_id UUID) RETURNS TEXT
--   admin.grant_plugin_to_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void
--   admin.revoke_plugin_from_tenant(p_plugin_name TEXT, p_tenant_id UUID) RETURNS void
--   admin.drop_tenant(p_tenant_id UUID) RETURNS void
--
-- These functions required PostgreSQL-specific features (CREATE ROLE,
-- ALTER ROLE, search_path, EXECUTE format(), gen_random_bytes, pg_roles)
-- which have no MySQL equivalent.
--
-- Tenant provisioning, API key management, and plugin access control are
-- handled entirely in application code, with tenant_id-based filtering
-- on every query (see application-layer tenant isolation pattern).
--
-- The DO $$ block that backfills roles for existing tenants is also omitted.
-- Backfill is handled by the application on first use.
