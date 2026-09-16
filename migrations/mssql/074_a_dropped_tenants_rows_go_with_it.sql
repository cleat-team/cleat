-- cleat migration 074 (mssql): a tenant can be dropped, and its rows go with it
--
-- cleat#1635. SQL Server had no counterpart to admin.drop_tenant, so
-- `cleatctl drop-tenant` refused on this dialect (cmd/cleatctl/ported.go) and a
-- tenant could not be deleted at all. That is a data-retention hole on a tier-1
-- dialect: there was no supported way to remove a customer's data.
--
-- WHAT #1629 CHANGED, AND WHY IT MADE THE MANUAL WORKAROUND WORSE. Plugin
-- tables now carry a security policy, and a FILTER PREDICATE hides rows from
-- DELETE exactly as it hides them from SELECT. So the cleanup an operator would
-- type, having been refused by cleatctl:
--
--     DELETE FROM dbo.kv_store WHERE tenant_id = '<dropped tenant>';
--     -- (0 rows affected)
--
-- No error, no warning, nothing deleted -- and "(0 rows affected)" is
-- indistinguishable from "already clean". Measured on SQL Server 2022 against
-- the shipped predicate, together with the discriminator: with the tenant key
-- set first, the identical statement reports 1 row affected and the row is
-- gone. The same measurement also disproves the "unreachable rows" reading this
-- issue was filed with -- the predicate's cross_tenant disjunct admits, and so
-- does the dropped tenant's own key. The rows were always reachable. What was
-- missing was anything that reached them.
--
-- THE TABLE SET IS DERIVED, NOT LISTED, and that is the main design decision
-- here. admin.drop_tenant on PostgreSQL deletes from a hand-maintained list,
-- and that list has drifted three times: tenant_settings (missing for twenty
-- migrations, per its own comment in cmd/cleatctl/droptenant.go), workflow_defs
-- (cleat#1201), and workflow_memory_{stats,samples} (cleat#1644, found while
-- writing this file and still open on PostgreSQL). A list cannot answer "is
-- there a tenant-owned table I do not know about"; sys.columns can.
--
-- So the sweep below deletes from every table carrying a tenant_id column,
-- which covers core tables, plugin tables, and any table a future migration
-- adds, with nobody editing this procedure. It needs no plugin registry: on
-- SQL Server plugin tables live in dbo alongside the core ones (WithSchema is
-- PostgreSQL-only), so one query finds both. admin.plugin_tables exists here
-- (001_schema.sql) and is deliberately left alone -- it holds the pre-066
-- two-column shape, has never had a producer on this dialect, and adding one
-- would be a second thing to keep in step with a question sys.columns already
-- answers.
--
-- WHAT IS STILL EXPLICIT, AND WHY. Five tables are deleted by name before the
-- sweep, because they are the only ones with foreign keys among the
-- tenant-owned set and a DELETE must precede the table it references:
--
--     workflow_tags      -> workflow_defs        NO_ACTION
--     workflow_routing   -> workflow_defs        NO_ACTION
--     event_history      -> workflow_instances   CASCADE
--     workflow_instances -> workflow_defs        NO_ACTION
--     workflow_defs
--
-- Re-derive that set rather than trusting this comment:
--
--     SELECT OBJECT_NAME(parent_object_id), OBJECT_NAME(referenced_object_id),
--            delete_referential_action_desc
--     FROM sys.foreign_keys;
--
-- event_history is listed even though workflow_instances cascades it on this
-- dialect (it does NOT on PostgreSQL, where 003_procedures.sql dropped that
-- cascade deliberately) -- deleting it first is free and keeps the two dialects
-- reading the same way.
--
-- A tenant-owned table added LATER with a foreign key would land in the sweep
-- and could be deleted out of order. That fails loudly with a foreign key
-- violation inside the transaction below, which rolls back; it does not
-- silently skip rows. Loud-and-safe is the failure mode chosen here, because
-- the alternative -- a topological sort over sys.foreign_keys -- is cleverness
-- in a destructive procedure, and the ordering question has had exactly five
-- members for the life of the schema.
--
-- THE SESSION KEY IS SET AND RESTORED. Every DELETE below is subject to the
-- same policies as any other statement -- SQL Server applies RLS to sysadmin
-- and db_owner too, measured in 012_admin_role.sql and again here. So the
-- procedure sets SESSION_CONTEXT('tenant_id') to the tenant being dropped,
-- which is what PostgreSQL's set_config('cleat.tenant_id', ..., true) does in
-- admin.drop_tenant.
--
-- It is NOT the cross_tenant key. That key would admit every tenant's rows to
-- every statement in this procedure, so a predicate typo would delete another
-- tenant's data. The tenant's own key makes the policy a second check on the
-- WHERE clause rather than a bystander: a DELETE whose predicate named the
-- wrong tenant removes nothing instead of removing the wrong rows.
--
-- sp_set_session_context is SESSION-scoped, not transaction-scoped, so a
-- ROLLBACK does not undo it. The prior value is saved and restored on both the
-- success and the failure path, or the caller's connection would silently
-- continue as the deleted tenant.
--
-- NOT PORTED FROM POSTGRESQL: the role and schema drops. admin.drop_tenant
-- there does DROP OWNED BY / DROP SCHEMA / DROP ROLE for a per-tenant role and
-- schema. SQL Server has neither -- 012_admin_role.sql creates one database
-- role, dbo.cleat_admin, shared by the deployment and not per tenant, and
-- WithSchema is PostgreSQL-only so there is no tenant_<uuid> schema. There is
-- nothing to drop, which is why this procedure is shorter than its counterpart
-- rather than incomplete.

IF OBJECT_ID(N'admin.drop_tenant', N'P') IS NOT NULL
    DROP PROCEDURE admin.drop_tenant;
GO

-- A procedure CAPTURES these two settings at CREATE time and runs with them
-- forever, whatever the caller has set. Every table this procedure deletes from
-- is bound to a security policy whose predicate function is WITH SCHEMABINDING,
-- and SQL Server refuses a DELETE against such a table unless QUOTED_IDENTIFIER
-- was ON when the statement's batch was compiled:
--
--   Msg 1934 ... DELETE failed because the following SET options have incorrect
--   settings: 'QUOTED_IDENTIFIER'.
--
-- Measured: creating this procedure from sqlcmd, which leaves QUOTED_IDENTIFIER
-- OFF for a -i script, produced exactly that on the first DELETE -- with the
-- transaction rolled back and the session key correctly restored, so the
-- failure was safe, but the procedure was unusable.
--
-- The nine other procedures in migrations/mssql/ carry no such SET and work,
-- because go-mssqldb's login turns both ON and the migration runner is the only
-- thing that has ever created them. That is a client default holding a schema
-- decision up. Stated here rather than inherited, so this procedure means the
-- same thing whoever applies the file.
SET QUOTED_IDENTIFIER ON;
SET ANSI_NULLS ON;
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
