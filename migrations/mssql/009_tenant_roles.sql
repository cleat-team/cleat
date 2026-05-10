-- cleat tenant roles and admin schema migration (T-SQL for SQL Server 2017+ / Azure SQL Database)
--
-- This is a minimal T-SQL version of the PostgreSQL tenant roles migration.
-- PostgreSQL uses CREATE ROLE + per-tenant schemas + SECURITY DEFINER functions
-- for tenant-level isolation. SQL Server uses a different security model
-- (schemas, database roles, and SESSION_CONTEXT-based RLS).
--
-- The admin schema houses cleat-internal tenant metadata tables.
-- Application-level tenant isolation is enforced via Security Policy predicates
-- (see 002_tenant_foundation.sql) rather than per-tenant database roles.
--
-- Stored procedures for dynamic tenant provisioning are implemented in the
-- application layer (Go) rather than in T-SQL, to avoid the complexity of
-- dynamic SQL with elevated permissions.

-- ===========================================================================
-- 1. Create admin schema for cleat-internal tables.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.schemas WHERE name = N'admin')
    EXEC('CREATE SCHEMA admin');

-- ===========================================================================
-- 2. Create tenants metadata table in admin schema
-- (idempotent fallback if 002 already created it in dbo)
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.tenants') AND type = N'U')
    CREATE TABLE admin.tenants (
        tenant_id    UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
        name         NVARCHAR(255)    NOT NULL,
        display_name NVARCHAR(MAX)    NOT NULL DEFAULT '',
        created_at   DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
        suspended    BIT              NOT NULL DEFAULT 0,
        CONSTRAINT pk_admin_tenants PRIMARY KEY (tenant_id),
        CONSTRAINT uq_admin_tenants_name UNIQUE (name)
    );

-- ===========================================================================
-- 3. Create tenant API keys table in admin schema
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.tenant_api_keys') AND type = N'U')
    CREATE TABLE admin.tenant_api_keys (
        key_id      UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
        tenant_id   UNIQUEIDENTIFIER NOT NULL,
        key_hash    VARBINARY(MAX)   NOT NULL,
        description NVARCHAR(MAX)    NOT NULL DEFAULT '',
        created_at  DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
        revoked_at  DATETIMEOFFSET   NULL,
        CONSTRAINT pk_admin_tenant_api_keys PRIMARY KEY (key_id),
        CONSTRAINT fk_admin_api_keys_tenant FOREIGN KEY (tenant_id)
            REFERENCES admin.tenants(tenant_id)
    );

-- ===========================================================================
-- 4. Table to track plugin tables per plugin (for GRANT management).
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.plugin_tables') AND type = N'U')
    CREATE TABLE admin.plugin_tables (
        plugin_name NVARCHAR(200) NOT NULL,
        table_name  NVARCHAR(200) NOT NULL,
        CONSTRAINT pk_admin_plugin_tables PRIMARY KEY (plugin_name, table_name)
    );

-- ===========================================================================
-- 5. Table to store tenant role credentials.
-- In SQL Server, tenant-level access is managed via database roles or
-- application-level authorization, not via SQL Server LOGIN roles.
-- This table stores role metadata for application-level tenant switching.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.tenant_roles') AND type = N'U')
    CREATE TABLE admin.tenant_roles (
        tenant_id  UNIQUEIDENTIFIER NOT NULL,
        role_name  NVARCHAR(MAX)    NOT NULL,
        password   NVARCHAR(MAX)    NOT NULL,
        created_at DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
        CONSTRAINT pk_admin_tenant_roles PRIMARY KEY (tenant_id),
        CONSTRAINT fk_admin_roles_tenant FOREIGN KEY (tenant_id)
            REFERENCES admin.tenants(tenant_id)
    );

-- ===========================================================================
-- NOTE: PostgreSQL-specific stored procedures for dynamic role creation,
-- schema management, and per-plugin GRANT/REVOKE operations are not
-- translated to T-SQL. The corresponding functionality is implemented
-- in the Go application layer using:
--   - Application-level tenant context switching via SESSION_CONTEXT
--   - Security Policies for row-level tenant isolation (see 002)
--   - Database roles provisioned by the application at startup
-- ===========================================================================
