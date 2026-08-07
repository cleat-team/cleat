-- cleat mssql default data (consolidated)
-- Data initialization compiled from migration 003_defaults.
-- Idempotent: all INSERT statements use IF NOT EXISTS guards.

-- ===========================================================================
-- Create default tenant for existing data
--
-- admin.tenants, not dbo.tenants. 001_schema.sql used to create both pairs and
-- this file seeded the dbo one, so on SQL Server the default tenant row has
-- never existed in the table anything reads: auth.TenantStore writes admin.*,
-- and migrations/postgres/002_defaults.sql seeds admin.tenants. The two
-- dialects disagreed about which table this row belongs in, and SQL Server was
-- the one that was wrong.
--
-- 013_drop_duplicate_tenant_tables.sql removes the dbo pair, so leaving this
-- alone would have failed the whole migration run at 002 on a fresh database:
--
--   migration 002_defaults.sql: execute: mssql: Invalid object name 'dbo.tenants'
--
-- Note the ordering that makes this safe on an existing database too: 002 runs
-- before 013, so dbo.tenants may still be present here -- but the row belongs
-- in admin.tenants either way, and 013 carries across anything the dbo table
-- picked up by hand before dropping it.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM admin.tenants WHERE tenant_id = '00000000-0000-0000-0000-000000000000')
    INSERT INTO admin.tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant');

-- ===========================================================================
-- Backfill tenant_id on all tenant-scoped tables
--
-- In the consolidated schema (001_schema.sql), tenant_id is already NOT NULL
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
