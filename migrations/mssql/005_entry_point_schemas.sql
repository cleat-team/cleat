-- cleat#1980: typed invocation, part 1. See migrations/postgres/006 for the
-- full rationale; this is the same column on the MSSQL dialect. nvarchar(max)
-- is this table's existing spelling for an untyped JSON document (dag_spec,
-- plugin_deps), since SQL Server has no native JSON column type.
--
-- Guarded through sys.columns, the same shape
-- migrations/mssql/004_workflow_defs_exposure_class.sql's `exposure` column
-- (cleat#1986) and plugins/webhookingest/migrations.go's UpMSSQL arms use for
-- an identical ADD COLUMN -- a bare ALTER would fail "Column names ...
-- already exist" on a second run, and this file (like every file above the
-- baseline) is replayed by migration.NewRunner's schema_migrations dedupe
-- only, not by SQL Server itself refusing a duplicate. Confirmed the hazard
-- is real, not theoretical, by TestMigrationIsADeployStepOnEveryDialect's
-- step 6 (cleat#2117), which deletes the newest schema_migrations row and
-- re-runs --migrate-only to confirm the runner repairs a "one behind" schema.
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('dbo.workflow_defs') AND name = 'entry_point_schemas')
    ALTER TABLE dbo.workflow_defs ADD entry_point_schemas nvarchar(max) NULL;
GO
