-- cleat migration 080 (mssql): collection eligibility is not retirement
--
-- cleat#1702, the SQL Server half of postgres/088. See that file for the two
-- roles `workflow_defs.deprecated` carried, for why it had to be SPLIT rather
-- than renamed, for the choice of a boolean named for its mechanism, and for
-- why the two columns' equality is INCIDENTAL, NOT INVARIANT.
--
-- THE BACKFILLS: `gc_eligible = deprecated` is EXACT; `disabled_at` for a
-- retired version is an UPPER BOUND, since a boolean never recorded when. 086's
-- updated_at argument does not transfer -- see postgres/088.
--
-- THE SECURITY POLICY MUST BE OFF FOR THE BACKFILLS, exactly as in 078.
-- dbo.TenantFilter_Defs is a FILTER predicate on this table
-- (001_schema.sql:446), and SQL Server's filter predicates exempt NOBODY, not
-- even sysadmin. A migration runner sets no SESSION_CONTEXT, so an un-toggled
-- UPDATE here matches ZERO ROWS and reports success -- the column would be
-- added, the backfill would silently do nothing, and `deprecated` would be
-- dropped taking the only copy of the value with it.
--
-- AN EARLIER DRAFT OF THIS FILE ASSERTED THE OPPOSITE, and the way it went
-- wrong is worth more than the fix. It said the absence of a policy had been
-- "verified against every ALTER SECURITY POLICY in the dialect". That check
-- runs clean and answers a different question: a policy is DEFINED by CREATE
-- SECURITY POLICY and only toggled by ALTER, so grepping the toggles finds
-- nothing on a table whose policy is created once in 001 and never touched
-- again. Measured on a live database instead:
--
--     sys.dm_db_partition_stats  ->  6 rows
--     SELECT COUNT(*)            ->  0 rows
--
-- Six rows present, none visible, no error anywhere. That is the shape this
-- comment previously claimed could not occur here.
--
-- EXEC() FOR ANYTHING NAMING `deprecated`, as in 079. SQL Server resolves a
-- missing TABLE late but not a missing COLUMN, so a batch naming the column
-- fails to compile once it is dropped -- even inside an IF whose branch is not
-- taken.
--
-- SYSUTCDATETIME() rather than GETDATE(): every timestamp this schema writes is
-- UTC, and DATETIMEOFFSET with a local-clock source would record the bound in
-- whatever zone the server happens to run in.

IF COL_LENGTH(N'dbo.workflow_defs', N'gc_eligible') IS NULL
    ALTER TABLE dbo.workflow_defs ADD gc_eligible BIT NOT NULL CONSTRAINT df_workflow_defs_gc_eligible DEFAULT 0;
GO

ALTER SECURITY POLICY dbo.TenantFilter_Defs WITH (STATE = OFF);
BEGIN TRY
    -- EXACT: boolean to boolean, same rows.
    IF COL_LENGTH(N'dbo.workflow_defs', N'deprecated') IS NOT NULL
        EXEC(N'UPDATE dbo.workflow_defs
                  SET gc_eligible = deprecated
                WHERE gc_eligible <> deprecated');

    -- AN UPPER BOUND: nothing recorded when the version was retired.
    IF COL_LENGTH(N'dbo.workflow_defs', N'deprecated') IS NOT NULL
        EXEC(N'UPDATE dbo.workflow_defs
                  SET disabled_at = SYSUTCDATETIME()
                WHERE deprecated = 1
                  AND disabled_at IS NULL');
END TRY
BEGIN CATCH
    ALTER SECURITY POLICY dbo.TenantFilter_Defs WITH (STATE = ON);
    THROW;
END CATCH;
ALTER SECURITY POLICY dbo.TenantFilter_Defs WITH (STATE = ON);
GO

-- The column may carry a default constraint from 001; SQL Server refuses to
-- drop a column while one references it, and the constraint name is
-- system-generated on some deployments, so it is looked up rather than guessed.
IF COL_LENGTH(N'dbo.workflow_defs', N'deprecated') IS NOT NULL
BEGIN
    DECLARE @df sysname, @sql nvarchar(max);
    SELECT @df = dc.name
      FROM sys.default_constraints dc
      JOIN sys.columns c ON c.object_id = dc.parent_object_id
                        AND c.column_id = dc.parent_column_id
     WHERE dc.parent_object_id = OBJECT_ID(N'dbo.workflow_defs')
       AND c.name = N'deprecated';
    IF @df IS NOT NULL
    BEGIN
        -- sp_executesql on a VARIABLE, not EXEC on an expression: EXEC() takes
        -- a string literal or a variable and will not parse a concatenation,
        -- which fails with "Incorrect syntax near 'QUOTENAME'" (measured).
        SET @sql = N'ALTER TABLE dbo.workflow_defs DROP CONSTRAINT ' + QUOTENAME(@df);
        EXEC sp_executesql @sql;
    END
END
GO

IF COL_LENGTH(N'dbo.workflow_defs', N'deprecated') IS NOT NULL
    ALTER TABLE dbo.workflow_defs DROP COLUMN deprecated;
GO
