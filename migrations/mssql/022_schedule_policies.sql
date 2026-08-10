-- cleat migration 022 (mssql): misfire and overlap policy
--
-- See migrations/postgres/022_schedule_policies.sql for what these mean and
-- why the defaults are what they are.
--
-- DEFAULT constraints are named explicitly: SQL Server auto-generates a name
-- for an unnamed default and that generated name differs between databases,
-- so a later migration wanting to drop one would have to go looking in
-- sys.default_constraints rather than just naming it.

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules') AND name = N'misfire_policy')
    ALTER TABLE dbo.workflow_schedules
        ADD misfire_policy NVARCHAR(16) NOT NULL
            CONSTRAINT df_workflow_schedules_misfire_policy DEFAULT 'catch_up';

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules') AND name = N'catch_up_limit')
    ALTER TABLE dbo.workflow_schedules
        ADD catch_up_limit INT NOT NULL
            CONSTRAINT df_workflow_schedules_catch_up_limit DEFAULT 60;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules') AND name = N'overlap_policy')
    ALTER TABLE dbo.workflow_schedules
        ADD overlap_policy NVARCHAR(16) NOT NULL
            CONSTRAINT df_workflow_schedules_overlap_policy DEFAULT 'allow';

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_schedules') AND name = N'last_run_id')
    ALTER TABLE dbo.workflow_schedules ADD last_run_id NVARCHAR(255) NULL;
GO

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_schedules_misfire_policy')
    ALTER TABLE dbo.workflow_schedules ADD CONSTRAINT ck_schedules_misfire_policy
        CHECK (misfire_policy IN ('catch_up', 'skip'));

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_schedules_overlap_policy')
    ALTER TABLE dbo.workflow_schedules ADD CONSTRAINT ck_schedules_overlap_policy
        CHECK (overlap_policy IN ('allow', 'skip'));

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_schedules_catch_up_limit')
    ALTER TABLE dbo.workflow_schedules ADD CONSTRAINT ck_schedules_catch_up_limit
        CHECK (catch_up_limit >= 0);
