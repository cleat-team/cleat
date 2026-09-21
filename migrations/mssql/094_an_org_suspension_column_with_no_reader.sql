-- cleat migration 094 (mssql): an org suspension column with no reader
--
-- cleat#1943. See
-- migrations/postgres/095_an_org_suspension_column_with_no_reader.sql for the
-- full reasoning: the `suspended` column on admin.orgs has no reader and no
-- writer (org suspension was never built), and it contradicts the org table's
-- "identity only" design. Drop it so the table is what its own comment claims.
--
-- The column carries a DEFAULT (0), which SQL Server models as a named default
-- constraint (DF__orgs__suspended__…), and DROP COLUMN refuses while a
-- dependent constraint exists. The constraint name is auto-generated, so it is
-- looked up and dropped first, not hardcoded.

DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql = 'ALTER TABLE admin.orgs DROP CONSTRAINT ' + QUOTENAME(dc.name)
FROM sys.default_constraints dc
JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id
WHERE dc.parent_object_id = OBJECT_ID(N'admin.orgs') AND c.name = N'suspended';

IF @sql <> N''
    EXEC sp_executesql @sql;

IF EXISTS (SELECT 1 FROM sys.columns
           WHERE object_id = OBJECT_ID(N'admin.orgs') AND name = N'suspended')
    ALTER TABLE admin.orgs DROP COLUMN suspended;
GO
