-- cleat mssql default data (consolidated)
-- Data initialization compiled from migration 002.
-- Idempotent: all INSERT statements use IF NOT EXISTS guards.

-- ===========================================================================
-- Create default tenant for existing data
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM dbo.tenants WHERE tenant_id = '00000000-0000-0000-0000-000000000000')
    INSERT INTO dbo.tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant');

-- ===========================================================================
-- Backfill tenant_id on all tenant-scoped tables
--
-- In the consolidated schema (001_tables.sql), tenant_id is already NOT NULL
-- with a DEFAULT, so fresh installs never have NULL rows.
-- These backfills protect databases migrated from old schema where tenant_id
-- was initially NULL.
--
-- Wrapped in EXEC because SQL Server validates column references at parse time
-- and the column may not exist in all migration scenarios.
-- ===========================================================================
EXEC('UPDATE dbo.workflow_defs SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_instances SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.event_history SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_signals SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_schedules SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.concurrency_keys SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
