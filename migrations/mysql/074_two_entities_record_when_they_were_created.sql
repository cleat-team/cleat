-- cleat migration 074 (mysql): two entities record when they were created
--
-- cleat#1702, the MySQL half of postgres/086. See that file for why existing
-- rows are backfilled from `updated_at`, and in particular for why that is an
-- UPPER BOUND here rather than the exact value 085 could claim when it went the
-- other way -- these two members are the inverse case, carrying `updated_at`
-- and no `created_at`.
--
-- NO SCHEMAS IN THIS DIALECT, so every table is named bare where Postgres would
-- qualify it. Both tables here are `public.` on Postgres, so nothing changes
-- but it is the same rule (cleat#1719).
--
-- MySQL HAS NO `ADD COLUMN IF NOT EXISTS`, so each add is a prepared statement
-- built from information_schema -- the idiom this tree already uses, in
-- migrations 072 and 073 next door. `DO 0` is the no-op branch.
--
-- TIMESTAMP(6) NOT NULL DEFAULT NOW(6), matching `updated_at` on tenant_settings
-- (039) and tenant_secrets (069) exactly.
--
-- THE EXPLICIT `NULL DEFAULT NULL` ON THE ADD IS LOAD-BEARING, and doubly so
-- here. It is what makes existing rows visible to the backfill, as in every
-- dialect. It also stops MySQL attaching its implicit `DEFAULT CURRENT_TIMESTAMP
-- ON UPDATE CURRENT_TIMESTAMP` to a TIMESTAMP column -- which would make this
-- dialect alone maintain the value, so the same schema would mean different
-- things on different engines. 073 records that hazard for the first TIMESTAMP
-- column in a table; both tables here already have `updated_at` as theirs, so
-- an appended column would not be first anyway. Spelling it out regardless,
-- because relying on column ORDER for a semantic property is a dependency on
-- something no other dialect shares and nothing here asserts.
--
-- Idempotent: the add is guarded on information_schema, the backfill on
-- IS NULL, and MODIFY COLUMN is a no-op against the shape it already has.

SET @cre_tenant_settings := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_settings ADD COLUMN created_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_settings'
      AND COLUMN_NAME = 'created_at'
);
PREPARE cre_tenant_settings FROM @cre_tenant_settings; EXECUTE cre_tenant_settings; DEALLOCATE PREPARE cre_tenant_settings;
UPDATE tenant_settings SET created_at = updated_at WHERE created_at IS NULL;
ALTER TABLE tenant_settings MODIFY COLUMN created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);

SET @cre_tenant_secrets := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_secrets ADD COLUMN created_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_secrets'
      AND COLUMN_NAME = 'created_at'
);
PREPARE cre_tenant_secrets FROM @cre_tenant_secrets; EXECUTE cre_tenant_secrets; DEALLOCATE PREPARE cre_tenant_secrets;
UPDATE tenant_secrets SET created_at = updated_at WHERE created_at IS NULL;
ALTER TABLE tenant_secrets MODIFY COLUMN created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);
