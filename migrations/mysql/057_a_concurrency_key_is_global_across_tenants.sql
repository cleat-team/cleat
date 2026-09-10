-- cleat migration 057 (mysql): scope concurrency keys to a tenant
--
-- See migrations/postgres/057 for the defect and the reasoning. cleat#1189.
--
-- MySQL needs one step the other dialects do not: concurrency_keys.tenant_id
-- is `CHAR(36)` with no NOT NULL here, while PostgreSQL and SQL Server both
-- declare it NOT NULL with the all-zero default. A nullable column cannot sit
-- in a primary key -- MySQL would silently coerce it to NOT NULL and take the
-- implicit '' default, which is a different tenant id from the all-zero one
-- every other dialect uses. So the column is made NOT NULL with the same
-- default the others carry, explicitly, before the key changes.
--
-- MySQL ALTER TABLE forms used here have no IF NOT EXISTS.

SET @tenant_nullable := (
    SELECT COUNT(*) FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'concurrency_keys'
      AND COLUMN_NAME = 'tenant_id'
      AND IS_NULLABLE = 'YES'
);

SET @stmt := IF(@tenant_nullable = 1,
    'ALTER TABLE concurrency_keys MODIFY COLUMN tenant_id CHAR(36) NOT NULL DEFAULT ''00000000-0000-0000-0000-000000000000''',
    'DO 0');

PREPARE fix_tenant_null FROM @stmt;
EXECUTE fix_tenant_null;
DEALLOCATE PREPARE fix_tenant_null;

-- Any pre-existing NULL takes the same tenant the other dialects default to,
-- rather than whatever MODIFY would coerce it to.
UPDATE concurrency_keys
   SET tenant_id = '00000000-0000-0000-0000-000000000000'
 WHERE tenant_id IS NULL;

-- Swap the single-column primary key for the composite one. STATISTICS has
-- one row per key column, so a PRIMARY of one row is the old shape.
SET @pk_columns := (
    SELECT COUNT(*) FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'concurrency_keys'
      AND INDEX_NAME = 'PRIMARY'
);

SET @stmt := IF(@pk_columns = 1,
    'ALTER TABLE concurrency_keys DROP PRIMARY KEY, ADD PRIMARY KEY (key_hash, tenant_id)',
    'DO 0');

PREPARE swap_pk FROM @stmt;
EXECUTE swap_pk;
DEALLOCATE PREPARE swap_pk;
