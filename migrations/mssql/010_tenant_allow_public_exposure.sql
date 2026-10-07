-- cleat#1986: see migrations/postgres/011 for the full rationale; this is the
-- same column on the SQL Server dialect.
--
-- Guarded through sys.columns, the same shape
-- 004_workflow_defs_exposure_class.sql's `exposure` column and
-- 006_input_validation_disabled.sql use -- a bare ALTER would fail "Column
-- names ... already exist" on a second run, and this file (like every file
-- above the baseline) is replayed by migration.NewRunner's schema_migrations
-- dedupe only, not by SQL Server itself refusing a duplicate.
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('admin.tenants') AND name = 'allow_public_exposure')
    ALTER TABLE admin.tenants ADD allow_public_exposure bit NOT NULL CONSTRAINT df_admin_tenants_allow_public_exposure DEFAULT ((0));
GO
