-- cleat migration 060 (mssql): scope concurrency keys to a tenant
--
-- See migrations/postgres/057 for the defect and the reasoning. cleat#1189.
--
-- The constraint is named here (pk_concurrency_keys, 001_schema.sql:329)
-- rather than server-generated, so it can be dropped by name. The column is
-- already UNIQUEIDENTIFIER NOT NULL with the all-zero default, so unlike
-- MySQL there is no nullability step.

IF EXISTS (
    SELECT 1
    FROM sys.key_constraints kc
    JOIN sys.tables t ON t.object_id = kc.parent_object_id
    WHERE kc.type = 'PK'
      AND t.name = 'concurrency_keys'
      AND (SELECT COUNT(*) FROM sys.index_columns ic
            WHERE ic.object_id = kc.parent_object_id
              AND ic.index_id = kc.unique_index_id) = 1
)
BEGIN
    DECLARE @pk sysname = (
        SELECT kc.name
        FROM sys.key_constraints kc
        JOIN sys.tables t ON t.object_id = kc.parent_object_id
        WHERE kc.type = 'PK' AND t.name = 'concurrency_keys'
    );
    DECLARE @sql nvarchar(max) =
        N'ALTER TABLE dbo.concurrency_keys DROP CONSTRAINT ' + QUOTENAME(@pk);
    EXEC sp_executesql @sql;

    ALTER TABLE dbo.concurrency_keys
        ADD CONSTRAINT pk_concurrency_keys PRIMARY KEY (key_hash, tenant_id);
END
