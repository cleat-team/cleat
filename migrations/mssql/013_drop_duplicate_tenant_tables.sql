-- 013: drop the duplicate dbo.tenants / dbo.tenant_api_keys pair.
--
-- 001_schema.sql created two tenant table pairs: admin.tenants +
-- admin.tenant_api_keys, and dbo.tenants + dbo.tenant_api_keys. Only the admin
-- pair is real. auth.TenantStore writes admin.*, and PostgreSQL's equivalent
-- lookup reads admin.tenant_api_keys, so the dbo pair was never written by
-- anything in the tree.
--
-- It was not merely unused. MSSQLStore.ResolveTenantFromAPIKey read
-- `tenant_api_keys` unqualified, which resolves against the connecting
-- principal's default schema and therefore hit dbo -- the empty one. API-key
-- tenant resolution could not succeed on SQL Server at all. The index meant to
-- support that lookup, idx_api_keys_hash, was on the dbo table too, so once the
-- query was corrected the read had no index behind it.
--
-- 001_schema.sql no longer creates the dbo pair and now puts idx_api_keys_hash
-- on admin.tenant_api_keys. This migration brings databases built from the
-- earlier version to the same shape.

-- ---------------------------------------------------------------------------
-- 1. The index the auth path actually needs.
--
-- Every authenticated request filters admin.tenant_api_keys on key_hash. Create
-- it before dropping anything, so no window exists where the lookup is both
-- correct and unindexed.
-- ---------------------------------------------------------------------------
IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_api_keys_hash' AND object_id = OBJECT_ID(N'admin.tenant_api_keys'))
    CREATE INDEX idx_api_keys_hash ON admin.tenant_api_keys(key_hash) WHERE revoked_at IS NULL;
GO

-- ---------------------------------------------------------------------------
-- 2. Rescue any rows somebody put in the dbo pair by hand.
--
-- Nothing in the tree writes these tables, so in every deployment we know of
-- they are empty and this is a no-op. It is here because "no code writes it"
-- and "it has no rows" are different claims, and only the second one makes a
-- DROP safe. A tenant that exists in dbo and not in admin would otherwise take
-- its API keys with it.
-- ---------------------------------------------------------------------------
IF OBJECT_ID(N'dbo.tenants', N'U') IS NOT NULL
    INSERT INTO admin.tenants (tenant_id, name, display_name, created_at, suspended)
    SELECT d.tenant_id, d.name, d.display_name, d.created_at, d.suspended
    FROM dbo.tenants d
    WHERE NOT EXISTS (SELECT 1 FROM admin.tenants a WHERE a.tenant_id = d.tenant_id)
      AND NOT EXISTS (SELECT 1 FROM admin.tenants a WHERE a.name = d.name);
GO

IF OBJECT_ID(N'dbo.tenant_api_keys', N'U') IS NOT NULL
    INSERT INTO admin.tenant_api_keys (key_id, tenant_id, key_hash, description, created_at, revoked_at)
    SELECT d.key_id, d.tenant_id, d.key_hash, d.description, d.created_at, d.revoked_at
    FROM dbo.tenant_api_keys d
    WHERE EXISTS (SELECT 1 FROM admin.tenants a WHERE a.tenant_id = d.tenant_id)
      AND NOT EXISTS (SELECT 1 FROM admin.tenant_api_keys a WHERE a.key_id = d.key_id);
GO

-- ---------------------------------------------------------------------------
-- 3. Drop, child first: dbo.tenant_api_keys has fk_api_keys_tenant referencing
--    dbo.tenants, so the parent cannot go first.
-- ---------------------------------------------------------------------------
IF OBJECT_ID(N'dbo.tenant_api_keys', N'U') IS NOT NULL
    DROP TABLE dbo.tenant_api_keys;
GO

IF OBJECT_ID(N'dbo.tenants', N'U') IS NOT NULL
    DROP TABLE dbo.tenants;
GO
