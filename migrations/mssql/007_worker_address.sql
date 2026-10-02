-- cleat#2196. The reaper-to-worker veto channel needs a way to reach a
-- specific worker, and the owner's decision was DNS via a headless Service
-- (k8s/service.yaml, charts/cleat/templates/service.yaml), not a captured
-- pod IP. admin.workers.hostname is diagnostic only (os.Hostname(), not
-- necessarily routable); this is the address another worker can actually
-- dial, published only when --worker-service-name names a headless Service
-- that selects this pod (see cmd/cleat-worker/connection_share.go's
-- podAddress). Empty for every worker outside that configuration, which is
-- every worker today -- nothing yet reads this column (cleat#2196 step 4).
--
-- Guarded through sys.columns, the same shape 004_workflow_defs_exposure_
-- class.sql uses for an identical ADD COLUMN on dbo.workflow_defs -- a bare
-- ALTER would fail "Column names ... already exist" on a second run, and
-- this file is replayed by migration.NewRunner's schema_migrations dedupe
-- only, not by SQL Server itself refusing a duplicate.
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('admin.workers') AND name = 'address')
    ALTER TABLE admin.workers ADD address NVARCHAR(255) NOT NULL CONSTRAINT df_workers_address DEFAULT ('');
GO
