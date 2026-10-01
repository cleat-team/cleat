-- cleat#1986. Every workflow definition gets an exposure class, one of a
-- closed set of three: 'auth' (the default -- reachable from the HTTP API,
-- authentication required, today's behaviour), 'internal' (not reachable from
-- the external HTTP surface at all), or 'public' (reachable without
-- authentication, gated separately on an operator opt-in).
--
-- Guarded through sys.columns, the same shape
-- plugins/webhookingest/migrations.go's UpMSSQL arms use for an identical
-- ADD COLUMN -- a bare ALTER would fail "Column names ... already exist" on a
-- second run, and this file (like every file above the baseline) is replayed
-- by migration.NewRunner's schema_migrations dedupe only, not by SQL Server
-- itself refusing a duplicate.
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('dbo.workflow_defs') AND name = 'exposure')
    ALTER TABLE dbo.workflow_defs ADD exposure NVARCHAR(16) NOT NULL CONSTRAINT df_workflow_defs_exposure DEFAULT ('auth');
GO

-- Same guard shape for the CHECK constraint. SQL Server does enforce CHECK
-- constraints by default (WITH CHECK, the default ADD CONSTRAINT behaviour),
-- and every existing row already satisfies it via the DEFAULT above, so no
-- WITH NOCHECK is needed here the way ck_workflow_defs_dag_spec's does.
IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = 'ck_workflow_defs_exposure')
    ALTER TABLE dbo.workflow_defs ADD CONSTRAINT ck_workflow_defs_exposure CHECK (exposure IN ('public', 'auth', 'internal'));
GO
