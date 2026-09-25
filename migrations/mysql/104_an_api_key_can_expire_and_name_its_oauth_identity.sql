-- cleat migration 104 (mysql): an API key can expire and name its OAuth identity
--
-- See migrations/postgres/105_an_api_key_can_expire_and_name_its_oauth_identity.sql
-- for the full reasoning: cleat#2352, nullable expires_at (general capability,
-- enforced on every dialect) and nullable oauth_identity (OAuth-only, unused
-- here today), both NULL for every existing key. TIMESTAMP(6), matching this
-- table's own created_at.
--
-- GUARDED, following 099's own precedent -- two columns, so two guarded
-- statements rather than one, each independently idempotent.

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenant_api_keys'
               AND COLUMN_NAME = 'expires_at');
SET @sql := IF(@col = 0,
    'ALTER TABLE tenant_api_keys ADD COLUMN expires_at TIMESTAMP(6) NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @col := (SELECT COUNT(*) FROM information_schema.COLUMNS
             WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenant_api_keys'
               AND COLUMN_NAME = 'oauth_identity');
SET @sql := IF(@col = 0,
    'ALTER TABLE tenant_api_keys ADD COLUMN oauth_identity VARCHAR(512) NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
