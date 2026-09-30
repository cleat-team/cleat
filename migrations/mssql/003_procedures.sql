-- cleat mssql routines and security policies (generated)
-- Do not hand-edit; regenerate. See docs/contributor/migrations.md.
--
-- Bodies are sys.sql_modules.definition as stored, EXCEPT for the leading
-- declaration, which is normalised back to the form the source used (see
-- normalizeModuleHead). Everything after it is untouched -- half the modules
-- here begin with a leading comment, so anything that parses the head or
-- re-prefixes CREATE corrupts them.
--
-- Replayable: the policies are dropped first, then the routines they depend
-- on, then both are recreated. That order is forced -- DROP FUNCTION fails
-- with error 3729 while any TenantFilter_* policy exists -- and it is what
-- lets engine/testutil restore the plain predicate by replaying this file.
-- Applying these statements twice leaves the database in the same state.

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Defs')
    DROP SECURITY POLICY dbo.TenantFilter_Defs;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Domains')
    DROP SECURITY POLICY dbo.TenantFilter_Domains;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_EventHistory')
    DROP SECURITY POLICY dbo.TenantFilter_EventHistory;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Instances')
    DROP SECURITY POLICY dbo.TenantFilter_Instances;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_MemorySamples')
    DROP SECURITY POLICY dbo.TenantFilter_MemorySamples;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_MemoryStats')
    DROP SECURITY POLICY dbo.TenantFilter_MemoryStats;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Promises')
    DROP SECURITY POLICY dbo.TenantFilter_Promises;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Queues')
    DROP SECURITY POLICY dbo.TenantFilter_Queues;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Routing')
    DROP SECURITY POLICY dbo.TenantFilter_Routing;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Schedules')
    DROP SECURITY POLICY dbo.TenantFilter_Schedules;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Secrets')
    DROP SECURITY POLICY dbo.TenantFilter_Secrets;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Settings')
    DROP SECURITY POLICY dbo.TenantFilter_Settings;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Signals')
    DROP SECURITY POLICY dbo.TenantFilter_Signals;
GO

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Tags')
    DROP SECURITY POLICY dbo.TenantFilter_Tags;
GO

IF OBJECT_ID(N'admin.drop_tenant', N'P') IS NOT NULL
    DROP PROCEDURE admin.drop_tenant;
GO

CREATE PROCEDURE admin.drop_tenant
    @tenant_id UNIQUEIDENTIFIER
AS
BEGIN
    SET NOCOUNT ON;
    -- Any error aborts the batch and rolls the transaction back, rather than
    -- continuing with the next statement and half-deleting a tenant.
    SET XACT_ABORT ON;

    IF @tenant_id = '00000000-0000-0000-0000-000000000000'
        THROW 50001, 'admin.drop_tenant: refusing to delete the default tenant (00000000-0000-0000-0000-000000000000) -- it is shared by every single-tenant deployment', 1;

    -- NO "no such tenant" GUARD, deliberately. The state this procedure most
    -- needs to be usable in is the one cleat#1635 describes: the admin.tenants
    -- row already deleted by hand and the data still there. A guard on that row
    -- would refuse exactly the cleanup it exists to perform. Deleting a tenant
    -- that is already gone removes whatever is left and reports nothing, which
    -- is the correct answer for an idempotent destructive operation.

    DECLARE @prior_tenant NVARCHAR(4000) = CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(4000));
    DECLARE @tenant_text NVARCHAR(4000) = CAST(@tenant_id AS NVARCHAR(4000));

    BEGIN TRY
        EXEC sp_set_session_context @key = N'tenant_id', @value = @tenant_text;

        BEGIN TRANSACTION;

        -- The five foreign-key-ordered tables. See the header for the query
        -- that re-derives this set from sys.foreign_keys.
        DELETE FROM dbo.workflow_tags       WHERE tenant_id = @tenant_id;
        DELETE FROM dbo.workflow_routing    WHERE tenant_id = @tenant_id;
        DELETE FROM dbo.event_history       WHERE tenant_id = @tenant_id;
        DELETE FROM dbo.workflow_instances  WHERE tenant_id = @tenant_id;
        DELETE FROM dbo.workflow_defs       WHERE tenant_id = @tenant_id;

        -- Everything else that carries a tenant_id: core tables with no
        -- foreign keys, every plugin table, and anything a later migration
        -- adds. admin.tenants is excluded because it is deleted last, below.
        --
        -- The five names in the NOT IN below must stay equal to the five
        -- DELETEs above: drop one from there and leave it here and the table is
        -- silently skipped. That cannot land unnoticed --
        -- TestATenantCanBeDroppedOnSQLServer reads the same universe from
        -- sys.columns and names every table that still holds the dropped
        -- tenant's rows, so a list that has fallen out of step goes red
        -- carrying the table's name.
        --
        -- QUOTENAME on both parts, not string concatenation: these names come
        -- from sys.tables rather than from a request, but a plugin choosing an
        -- unfortunate table name should produce a quoted identifier, not
        -- arbitrary DDL running with this procedure's privileges.
        DECLARE @schema_name SYSNAME, @table_name SYSNAME, @sql NVARCHAR(MAX);
        DECLARE tenant_tables CURSOR LOCAL FAST_FORWARD FOR
            SELECT s.name, t.name
            FROM sys.tables t
            JOIN sys.schemas s ON s.schema_id = t.schema_id
            WHERE EXISTS (SELECT 1 FROM sys.columns c
                          WHERE c.object_id = t.object_id AND c.name = 'tenant_id')
              AND NOT (s.name = 'admin' AND t.name = 'tenants')
              AND NOT (s.name = 'dbo' AND t.name IN
                       ('workflow_tags', 'workflow_routing', 'event_history',
                        'workflow_instances', 'workflow_defs'))
            ORDER BY s.name, t.name;

        OPEN tenant_tables;
        FETCH NEXT FROM tenant_tables INTO @schema_name, @table_name;
        WHILE @@FETCH_STATUS = 0
        BEGIN
            SET @sql = N'DELETE FROM ' + QUOTENAME(@schema_name) + N'.' + QUOTENAME(@table_name)
                     + N' WHERE tenant_id = @tid';
            EXEC sp_executesql @sql, N'@tid UNIQUEIDENTIFIER', @tid = @tenant_id;
            FETCH NEXT FROM tenant_tables INTO @schema_name, @table_name;
        END
        CLOSE tenant_tables;
        DEALLOCATE tenant_tables;

        -- Last: tenant_settings, tenant_secrets, tenant_domains and
        -- tenant_egress_allow cascade from here, and the sweep above has
        -- already emptied them.
        DELETE FROM admin.tenants WHERE tenant_id = @tenant_id;

        COMMIT TRANSACTION;
    END TRY
    BEGIN CATCH
        IF CURSOR_STATUS('local', 'tenant_tables') >= 0
        BEGIN
            CLOSE tenant_tables;
            DEALLOCATE tenant_tables;
        END
        IF XACT_STATE() <> 0
            ROLLBACK TRANSACTION;
        -- Before the rethrow: a rollback does not restore session context, so
        -- without this the caller's connection would keep running as the
        -- tenant this procedure failed to delete.
        EXEC sp_set_session_context @key = N'tenant_id', @value = @prior_tenant;
        THROW;
    END CATCH

    EXEC sp_set_session_context @key = N'tenant_id', @value = @prior_tenant;
END
GO

IF OBJECT_ID(N'admin.trg_tenants_org_id_immutable', N'TR') IS NOT NULL
    DROP TRIGGER admin.trg_tenants_org_id_immutable;
GO

CREATE TRIGGER admin.trg_tenants_org_id_immutable
ON admin.tenants
AFTER UPDATE
AS
BEGIN
    SET NOCOUNT ON;
    IF EXISTS (
        SELECT 1
        FROM inserted i
        JOIN deleted d ON i.tenant_id = d.tenant_id
        WHERE i.org_id <> d.org_id
    )
    BEGIN
        RAISERROR(N'admin.tenants.org_id is immutable and cannot be changed', 16, 1);
        ROLLBACK TRANSACTION;
    END
END;
GO

IF OBJECT_ID(N'dbo.finalize_workflow_status', N'P') IS NOT NULL
    DROP PROCEDURE dbo.finalize_workflow_status;
GO

-- finalize_workflow_status no longer has a 'failed' branch.
--
-- cleat#1973. No production code path ever calls this procedure with
-- @p_final_status = 'failed'. FinalizeWorkflowSegment's one production call
-- site (cmd/cleat-worker/setup.go) only ever passes 'done' or 'ready'; the
-- real failure path is store.FailWorkflow (engine/mssql_lifecycle.go), a
-- plain UPDATE that never calls this procedure at all. So the 'failed'
-- branch below -- including its event_history DELETE -- has never run in
-- production. See migrations/postgres/101_the_finalize_procedure_stops_deleting_failed_history.sql
-- for the full reasoning; this is the same change on SQL Server.
--
-- A dormant delete is the risk, not a current bug: removing it is removing a
-- branch that one call-site change would silently reactivate, deleting a
-- failed workflow's replay history at finalize instead of leaving it for
-- --retention-days to sweep (cleat#1973's corrected behaviour).
--
-- NOTE ON 056'S OWN COMMENT. 056's header says the 'failed' branch's
-- idempotency_keys write is "untouched" and protects FailWorkflow and
-- MoveToDeadLetterQueue. That was wrong the same way this whole issue was:
-- neither of those call sites ever reaches this procedure. Both already
-- write idempotency_keys.error_msg themselves, directly
-- (engine/mssql_lifecycle.go), independent of this procedure. Removing the
-- branch here loses that update from exactly nowhere it was actually coming
-- from.
--
-- WHAT CHANGES, versus 056:
--   * The @p_final_status = 'failed' branch of the status IF/ELSE chain is
--     gone. A caller that passes 'failed' now hits the final ELSE and gets
--     the same "unknown final status" THROW any other unrecognized value
--     gets.
--   * The terminal-status IF narrows from
--     (@p_final_status = 'done' OR @p_final_status = 'failed') to
--     @p_final_status = 'done' alone.
--   * The idempotency_keys error_msg UPDATE that only ran for 'failed' is
--     removed with it, per the note above.
--   * The @p_final_status parameter comment drops 'failed' from its list.
--
-- WHAT DOES NOT CHANGE: the 'done' branch, byte for byte, and its DELETE FROM
-- dbo.event_history.
--
-- Everything else here is 056 verbatim, minus the 'failed' branch. Whole
-- redefinition rather than a patch: CREATE OR ALTER is idempotent, and
-- engine/store_backends_procedures_test.go re-applies every listed procedure
-- migration by name, so this file must be complete on its own.

CREATE OR ALTER PROCEDURE dbo.finalize_workflow_status
    @p_workflow_id      NVARCHAR(255),
    @p_worker_id        NVARCHAR(255),
    @p_generation       BIGINT,
    @p_final_status     NVARCHAR(32),     -- 'done' or 'ready'
    @p_result           NVARCHAR(MAX),
    @p_error_code       NVARCHAR(255),
    @p_error_op         NVARCHAR(255),
    @p_query_state      NVARCHAR(MAX),
    @p_next_wake_at     DATETIMEOFFSET,
    @p_notify_channel   NVARCHAR(255)
AS
BEGIN
    SET NOCOUNT ON;

    DECLARE @rows_updated INT = 0;

    BEGIN TRY
        -- Update workflow status, fenced on (assigned_to, generation) so a
        -- caller that no longer owns the workflow cannot modify it.
        IF @p_final_status = 'done'
        BEGIN
            UPDATE dbo.workflow_instances
            SET status = 'done',
                result = @p_result,
                completed_at = SYSUTCDATETIME(),
                assigned_to = NULL,
                query_state = @p_query_state
            WHERE id = @p_workflow_id
              AND assigned_to = @p_worker_id
              AND generation = @p_generation;
            SET @rows_updated = @@ROWCOUNT;
        END
        ELSE IF @p_final_status = 'ready'
        BEGIN
            UPDATE dbo.workflow_instances
            SET status = 'ready',
                assigned_to = NULL,
                next_wake_at = CASE
                                    WHEN signal_seq <> signal_seq_at_claim
                                      OR signal_consumed_seq <> signal_consumed_at_claim
                                    THEN SYSUTCDATETIME() ELSE @p_next_wake_at END,
                query_state = @p_query_state
            WHERE id = @p_workflow_id
              AND assigned_to = @p_worker_id
              AND generation = @p_generation;
            SET @rows_updated = @@ROWCOUNT;
        END
        ELSE
        BEGIN
            THROW 50000, 'finalize_workflow_status: unknown final status', 1;
        END

        -- Terminal status side-effects -- only run if the fenced UPDATE
        -- above actually matched this caller's (worker_id, generation), and
        -- only for 'done': a real failure never reaches this procedure
        -- (cleat#1973).
        IF @rows_updated > 0 AND @p_final_status = 'done'
        BEGIN
            -- Wake parent workflow atomically.
            UPDATE dbo.workflow_instances
            SET next_wake_at = SYSUTCDATETIME()
            WHERE id = (
                SELECT parent_workflow_id
                FROM dbo.workflow_instances
                WHERE id = @p_workflow_id
            )
            AND status IN ('ready', 'suspended');


            -- Delete this workflow's events -- they are no longer needed
            -- for replay once the workflow has reached a terminal state.
            -- This keeps event_history bounded to active workflows only,
            -- preventing unbounded table growth that slows per-step INSERTs.
            DELETE FROM dbo.event_history WHERE workflow_id = @p_workflow_id;
        END

        -- Dispatch hint: MSSQL has no native pg_notify equivalent.
        -- External notification (Service Broker, polling, etc.) is handled
        -- by the caller. @p_notify_channel is accepted for interface
        -- compatibility with the PostgreSQL signature.

        -- Report whether the fence held so Go callers can distinguish a
        -- lost fence (normal under reaping) from a real error.
        SELECT CASE WHEN @rows_updated > 0 THEN CAST(1 AS BIT) ELSE CAST(0 AS BIT) END AS fence_held;
    END TRY
    BEGIN CATCH
        THROW;
    END CATCH
END;
GO

IF OBJECT_ID(N'dbo.fn_tenant_filter', N'FN') IS NOT NULL
    DROP FUNCTION dbo.fn_tenant_filter;
GO

-- Defined as a plain CREATE OR ALTER in its own batch rather than inside
-- EXEC(N'...'), and that is not style. engine's routine-definition drift guard
-- reads this file TEXTUALLY to learn what each routine should be; wrapped in
-- dynamic SQL it read the whole migration -- MERGE, QUOTENAME, sp_executesql --
-- as part of the function body and reported drift against a database that was
-- correct. A definition a reader cannot extract is one a guard cannot check.
--
-- The captured policy set survives the batch boundary because it is a #temp
-- table: those live for the session, and migration.Runner.applyMigration runs
-- every batch of a file on one connection inside one transaction.
CREATE OR ALTER FUNCTION dbo.fn_tenant_filter(@tenant_id UNIQUEIDENTIFIER)
RETURNS TABLE
WITH SCHEMABINDING
AS
RETURN SELECT 1 AS access
    WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Defs
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_defs,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_defs AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_defs AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_defs BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Domains
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_domains,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_domains AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_domains AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_domains BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_EventHistory
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.event_history,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.event_history AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.event_history AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.event_history BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Instances
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_instances,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_instances AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_instances AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_instances BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_MemorySamples
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_samples,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_samples AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_samples AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_samples BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_MemoryStats
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_stats,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_stats AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_stats AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_memory_stats BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Promises
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_promises,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_promises AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_promises AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_promises BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Queues
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.queues,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.queues AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.queues AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.queues BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Routing
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_routing,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_routing AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_routing AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_routing BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Schedules
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_schedules,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_schedules AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_schedules AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_schedules BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Secrets
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_secrets,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_secrets AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_secrets AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_secrets BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Settings
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_settings,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_settings AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_settings AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_settings BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Signals
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_signals,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_signals AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_signals AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_signals BEFORE UPDATE
    WITH (STATE = ON);
GO

CREATE SECURITY POLICY dbo.TenantFilter_Tags
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_tags,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_tags AFTER INSERT,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_tags AFTER UPDATE,
    ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_tags BEFORE UPDATE
    WITH (STATE = ON);
GO

