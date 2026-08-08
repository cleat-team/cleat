-- cleat migration 021 (mssql): schedule timezone
--
-- See migrations/postgres/021_schedule_timezone.sql for why this column
-- exists and why the default is 'UTC' rather than the server's zone. The
-- short version: a cron expression is a statement about a wall clock, and
-- until this column existed the worker evaluated it in the worker PROCESS's
-- local zone, so one row meant different instants on different machines.
--
-- NVARCHAR(64) to match the other name-ish columns in this schema. The
-- longest IANA zone name in tzdata is comfortably under that.
--
-- The DEFAULT constraint is named explicitly. SQL Server auto-generates a name
-- for an unnamed default (DF__workflow___timez__<hex>), and that generated
-- name differs between databases, so a later migration wanting to drop it
-- would have to go looking in sys.default_constraints rather than just naming
-- it.

IF NOT EXISTS (
    SELECT 1 FROM sys.columns
    WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules')
      AND name = N'timezone'
)
    ALTER TABLE dbo.workflow_schedules
        ADD timezone NVARCHAR(64) NOT NULL
            CONSTRAINT df_workflow_schedules_timezone DEFAULT 'UTC';
