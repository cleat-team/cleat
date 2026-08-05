-- cleat migration 010 (mssql): scope idempotency keys to a tenant
--
-- Bug: idempotency_keys is keyed by key_hash alone and has no tenant_id
-- column, and the hash is sha256(idempotencyKey) with the tenant nowhere in
-- it. An Idempotency-Key is therefore global across every tenant in the
-- deployment. Two tenants that pick the same string -- "order-123",
-- "daily-report", "1" -- collide: the second is handed the *first* tenant's
-- workflow ID with already_started = true, and its own workflow is never
-- started. Idempotency-Key is a client-supplied request header, so this is
-- the expected outcome of two customers naming things the way people name
-- things, not an attack. IMPROVEMENT-PLAN 3.10.
--
-- SQL Server's security policies filter on a tenant column, so -- as on the
-- other two dialects -- there was nothing this table could have been fenced
-- with. The Go-level `AND tenant_id = @p` that lands with this migration is
-- its first tenant scoping of any kind.
--
-- Fix: add tenant_id and make the primary key (key_hash, tenant_id), so a
-- key is unique *within* a tenant and two tenants can hold the same key.
-- Existing rows take the default tenant, so a single-tenant deployment keeps
-- deduplicating across the upgrade. See the postgres file of the same number
-- for why the cheaper fix -- folding the tenant into the hash -- is wrong.
--
-- Idempotent: every statement is guarded on sys.* catalogue views.

IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID(N'dbo.idempotency_keys')
      AND name = N'tenant_id'
)
    ALTER TABLE dbo.idempotency_keys
        ADD tenant_id UNIQUEIDENTIFIER NOT NULL
        CONSTRAINT df_idempotency_keys_tenant_id
        DEFAULT '00000000-0000-0000-0000-000000000000';
GO

-- Swap the single-column primary key for the composite one. The column must
-- exist before it can be named in a key, and ALTER TABLE ... ADD above is not
-- visible to the rest of its own batch -- hence the GO.
-- Dropped by its catalogue name rather than the literal pk_idempotency_keys:
-- 001_schema.sql names the constraint, but a table created by a bare
-- `key_hash VARBINARY(32) NOT NULL PRIMARY KEY` carries a generated name, and
-- dropping the wrong name fails the whole migration.
DECLARE @pk_name SYSNAME = (
    SELECT kc.name
    FROM sys.key_constraints kc
    WHERE kc.parent_object_id = OBJECT_ID(N'dbo.idempotency_keys')
      AND kc.type = 'PK'
      AND (
          SELECT COUNT(*)
          FROM sys.index_columns ic
          WHERE ic.object_id = kc.parent_object_id
            AND ic.index_id = kc.unique_index_id
      ) = 1
);
IF @pk_name IS NOT NULL
    EXEC('ALTER TABLE dbo.idempotency_keys DROP CONSTRAINT [' + @pk_name + ']');
GO

IF NOT EXISTS (
    SELECT 1 FROM sys.key_constraints
    WHERE parent_object_id = OBJECT_ID(N'dbo.idempotency_keys')
      AND type = 'PK'
)
    ALTER TABLE dbo.idempotency_keys
        ADD CONSTRAINT pk_idempotency_keys PRIMARY KEY (key_hash, tenant_id);
GO
