-- cleat migration 075 (mysql): one reversible off-switch for API keys
--
-- cleat#1702, the MySQL half of postgres/087. Migration 072 already added
-- `disabled_at` to every member, so this backfills and removes rather than
-- adding. See that file for why
-- `revoked_at` -> `disabled_at` is a rename with an EXACT backfill rather than
-- the upper bound 086 had to take, and for why `workflow_defs.deprecated` is
-- not converted alongside it.
--
-- NO INDEX WORK HERE, AND THAT IS THE ONE PLACE THIS DIVERGES FROM BOTH
-- SIBLINGS. `idx_api_keys_hash` is partial on postgres and mssql --
-- `WHERE revoked_at IS NULL` -- so on those engines the index predicate names
-- the column being dropped and has to be rebuilt. MySQL has no partial
-- indexes, so its `idx_api_keys_hash` is a plain index on `key_hash`
-- (001_schema.sql:278), names no other column, and is unaffected by the drop.
--
-- Read from the three schema files rather than assumed from the postgres half:
-- transliterating 087 here would have produced a DROP INDEX for a predicate
-- MySQL never had, and a CREATE INDEX with a WHERE clause it cannot parse.
--
-- This table is also the one member with no tenant scoping on any engine --
-- the authenticator reads it before a tenant is known (postgres/061) -- so
-- there is no RLS to consider and nothing to disable around the backfill.
--
-- IDEMPOTENT VIA PREPARED STATEMENTS, following 074. The reason is not style:
-- MySQL has no ADD COLUMN IF NOT EXISTS, and more importantly the backfill
-- names `revoked_at`, so on a second run the server would fail PARSING a
-- statement whose branch should never execute. Building the statement as text
-- and preparing it only when the column is there defers that.

-- Exact, not a bound: revoked_at already holds the instant.
SET @backfill_disabled_at := (
    SELECT IF(COUNT(*) = 1,
        'UPDATE tenant_api_keys SET disabled_at = revoked_at WHERE revoked_at IS NOT NULL AND disabled_at IS NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_api_keys'
      AND COLUMN_NAME = 'revoked_at'
);
PREPARE backfill_disabled_at FROM @backfill_disabled_at; EXECUTE backfill_disabled_at; DEALLOCATE PREPARE backfill_disabled_at;

SET @drop_revoked_at := (
    SELECT IF(COUNT(*) = 1,
        'ALTER TABLE tenant_api_keys DROP COLUMN revoked_at',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_api_keys'
      AND COLUMN_NAME = 'revoked_at'
);
PREPARE drop_revoked_at FROM @drop_revoked_at; EXECUTE drop_revoked_at; DEALLOCATE PREPARE drop_revoked_at;
