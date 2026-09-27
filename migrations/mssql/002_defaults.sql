-- cleat mssql default data (HAND-ASSEMBLED)
--
-- This file is NOT generated. scripts/gen-mssql-baseline writes 001 and 003
-- only, because seed data is the one part of a schema a catalogue read cannot
-- reconstruct: sys.* records the shape a table ended up with, not the rows a
-- migration put in it. Keep it small, and re-read it whenever the chain it
-- replaces is re-read.
--
-- Three rows, and they are the whole of it -- measured on a chain-built
-- database: of the 30 user tables, only these three hold anything after a full
-- migration run. Everything else is empty on a fresh install.
--
-- Ordering is forced by a foreign key: admin.tenants.org_id references
-- admin.orgs, so the org row must exist first.
--
-- Timestamps are left to the column defaults (SYSUTCDATETIME()), so the values
-- a fresh install gets differ from any other install's by construction. They
-- are not part of the state a baseline reproduces, and a data comparison that
-- includes them would be comparing two clocks.

-- ===========================================================================
-- The default org
--
-- Was seeded by 082_an_org_groups_a_customers_tenants.sql, whose IF NOT EXISTS
-- guard this preserves.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM admin.orgs WHERE org_id = '00000000-0000-0000-0000-000000000000')
    INSERT INTO admin.orgs (org_id, name)
    VALUES ('00000000-0000-0000-0000-000000000000', N'default');

-- ===========================================================================
-- The default tenant
--
-- admin.tenants, not dbo.tenants. This file used to seed the dbo pair, which
-- meant the default tenant row never existed in the table anything reads:
-- auth.TenantStore writes admin.*, and migrations/postgres/002_defaults.sql
-- seeds admin.tenants. The two dialects disagreed about which table this row
-- belongs in, and SQL Server was the one that was wrong. The dbo pair is gone
-- from the baseline entirely -- 013_drop_duplicate_tenant_tables.sql dropped it
-- before this baseline was cut, so 001_schema.sql has no dbo.tenants to seed.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM admin.tenants WHERE tenant_id = '00000000-0000-0000-0000-000000000000')
    INSERT INTO admin.tenants (tenant_id, name, display_name, org_id)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant',
            '00000000-0000-0000-0000-000000000000');

-- ===========================================================================
-- The RLS predicate form in use
--
-- One row, enforced by the table's own PRIMARY KEY (only_row) and its
-- CHECK (only_row = 1): two rows disagreeing about the predicate is a state no
-- reader could resolve. 075_the_admin_bypass_is_opt_in.sql installs it with a
-- MERGE and written as 'plain'; the shape is kept here so the file reads the
-- same as the migration it replaces.
--
-- This row must be written BEFORE 003 creates the security policies, which is
-- why it lives here rather than there: it records which predicate the policies
-- were installed against.
-- ===========================================================================
MERGE admin.rls_predicate_form AS t
USING (SELECT 1 AS only_row, N'plain' AS form) AS s
   ON t.only_row = s.only_row
WHEN MATCHED THEN UPDATE SET form = s.form, applied_at = SYSUTCDATETIME()
WHEN NOT MATCHED THEN INSERT (only_row, form) VALUES (s.only_row, s.form);

-- ===========================================================================
-- Backfill tenant_id on tenant-scoped tables
--
-- A no-op on a database this baseline created: 001_schema.sql declares every
-- tenant_id NOT NULL with a DEFAULT, so there is no NULL row to find. It is
-- kept because the alternative -- reasoning that it can never fire -- is how a
-- backfill that WAS needed gets deleted, and because it is the only reason
-- these tables are listed anywhere near the seed path.
--
-- Wrapped in EXEC because SQL Server validates column references at parse
-- time, so a bare UPDATE against a table that does not exist fails the whole
-- file rather than the statement.
--
-- These run in 002, and the security policies are in 003, deliberately: a
-- BLOCK predicate applies to sa as much as to anyone, so an UPDATE against a
-- policy-covered table after 003 would be refused.
-- ===========================================================================
EXEC('UPDATE dbo.workflow_defs SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_instances SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.event_history SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_signals SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.workflow_schedules SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
EXEC('UPDATE dbo.concurrency_keys SET tenant_id = ''00000000-0000-0000-0000-000000000000'' WHERE tenant_id IS NULL');
