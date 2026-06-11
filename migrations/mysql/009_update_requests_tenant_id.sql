-- 009_update_requests_tenant_id.sql: Add tenant_id column to workflow_update_requests
-- for tenant-scoped filtering (consistent with Postgres and MSSQL schemas).

SET @s = IF(
    (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE()
       AND TABLE_NAME = 'workflow_update_requests'
       AND COLUMN_NAME = 'tenant_id') = 0,
    'ALTER TABLE workflow_update_requests ADD COLUMN tenant_id CHAR(36) NOT NULL DEFAULT ''00000000-0000-0000-0000-000000000000''',
    'SELECT 1'
);
PREPARE stmt FROM @s;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;
