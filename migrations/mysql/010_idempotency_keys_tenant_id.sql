-- cleat migration 010 (mysql): scope idempotency keys to a tenant
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
-- MySQL has no row-level security, so the Go-level `AND tenant_id = ?` that
-- lands with this migration is the only tenant scoping this table has. It is
-- also the only scoping it *could* have had: RLS filters on a tenant column,
-- and until now there was no tenant column to filter on.
--
-- Fix: add tenant_id and make the primary key (key_hash, tenant_id), so a
-- key is unique *within* a tenant and two tenants can hold the same key.
-- Existing rows take the default tenant, so a single-tenant deployment keeps
-- deduplicating across the upgrade. See the postgres file of the same number
-- for why the cheaper fix -- folding the tenant into the hash -- is wrong.
--
-- Idempotent: both statements are guarded on information_schema, because the
-- MySQL ALTER TABLE forms used here have no IF NOT EXISTS.

SET @have_tenant_id := (
    SELECT COUNT(*) FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'idempotency_keys'
      AND COLUMN_NAME = 'tenant_id'
);

SET @stmt := IF(@have_tenant_id = 0,
    'ALTER TABLE idempotency_keys ADD COLUMN tenant_id CHAR(36) NOT NULL DEFAULT ''00000000-0000-0000-0000-000000000000''',
    'DO 0');

PREPARE add_tenant_id FROM @stmt;
EXECUTE add_tenant_id;
DEALLOCATE PREPARE add_tenant_id;

-- Swap the single-column primary key for the composite one. STATISTICS has
-- one row per key column, so a PRIMARY of one row is the old shape.
SET @pk_columns := (
    SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'idempotency_keys'
      AND INDEX_NAME = 'PRIMARY'
);

SET @stmt := IF(@pk_columns = 1,
    'ALTER TABLE idempotency_keys DROP PRIMARY KEY, ADD PRIMARY KEY (key_hash, tenant_id)',
    IF(@pk_columns = 0,
        'ALTER TABLE idempotency_keys ADD PRIMARY KEY (key_hash, tenant_id)',
        'DO 0'));

PREPARE swap_pk FROM @stmt;
EXECUTE swap_pk;
DEALLOCATE PREPARE swap_pk;
