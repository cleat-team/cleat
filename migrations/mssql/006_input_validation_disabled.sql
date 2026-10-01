-- cleat#1981: typed invocation, part 2. See migrations/postgres/007 for the
-- full rationale; this is the same column on the MSSQL dialect.
--
-- Guarded through sys.columns, the same shape
-- 004_workflow_defs_exposure_class.sql's `exposure` column and
-- 005_entry_point_schemas.sql use -- a bare ALTER would fail "Column names
-- ... already exist" on a second run, and this file (like every file above
-- the baseline) is replayed by migration.NewRunner's schema_migrations
-- dedupe only, not by SQL Server itself refusing a duplicate.
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('dbo.workflow_defs') AND name = 'input_validation_disabled')
    ALTER TABLE dbo.workflow_defs ADD input_validation_disabled bit NOT NULL CONSTRAINT df_workflow_defs_input_validation_disabled DEFAULT ((0));
GO
