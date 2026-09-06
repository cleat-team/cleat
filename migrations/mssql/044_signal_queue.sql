-- cleat migration 044 (mssql): a signal is a delivery, not a row
--
-- SQL Server half of migrations/postgres/041_signal_queue.sql, which carries
-- the rationale. IMPROVEMENT-PLAN 3.215.
--
-- No ordering constraint here, unlike MySQL. SQL Server does not require an
-- index on a foreign key's referencing column, so fk_signals_workflow does not
-- object to the primary key going away, and the index can be created in
-- whichever order reads best.
--
-- dbo.workflow_signals carries a FILTER PREDICATE security policy
-- (migrations/mssql/012_admin_role.sql). The predicate is dbo.fn_tenant_filter
-- over tenant_id, and a schemabound predicate binds to the columns it names --
-- so adding an unrelated column and swapping the primary key do not touch it.
-- Same conclusion as the primary-key swap in 038, and for the same reason.
--
-- Both constraint names are declared explicitly in 001_schema.sql, so unlike
-- PostgreSQL there is no generated name to look up.

IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID(N'dbo.workflow_signals')
      AND name = N'id'
)
BEGIN
    ALTER TABLE dbo.workflow_signals DROP CONSTRAINT pk_workflow_signals;
    ALTER TABLE dbo.workflow_signals
        ADD id BIGINT IDENTITY(1,1) NOT NULL
            CONSTRAINT pk_workflow_signals PRIMARY KEY;
END
GO

IF NOT EXISTS (
    SELECT 1 FROM sys.indexes
    WHERE object_id = OBJECT_ID(N'dbo.workflow_signals')
      AND name = N'idx_workflow_signals_queue'
)
    CREATE INDEX idx_workflow_signals_queue
        ON dbo.workflow_signals (workflow_id, signal_name);
GO
