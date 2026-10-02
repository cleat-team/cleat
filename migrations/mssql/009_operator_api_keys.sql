-- cleat#2169: an operator credential for admin HTTP routes. See
-- migrations/postgres/010_operator_api_keys.sql for what the credential is and
-- why it is the shape of tenant_api_keys minus tenant_id; this file records
-- only what differs on SQL Server.
--
-- NO GRANT HERE, and that is a property rather than an omission.
-- 008_app_login.sql grants at SCHEMA level:
--   GRANT SELECT, INSERT, UPDATE, DELETE ON SCHEMA::admin TO cleat_app_role;
-- and SQL Server applies a schema-level grant to whatever the schema contains
-- at PERMISSION-CHECK TIME, not only to what existed when the GRANT ran -- the
-- same property 008_app_login.sql's own comment names when it explains why
-- plugin tables added under dbo later need no further grant. So this table is
-- covered by the grant that already exists, and adding a second one would be
-- redundant rather than safe.
--
-- The "outside the tenant-scoped surface" property: SQL Server DOES have row
-- level security, and this table has no security policy and no
-- SECURITY POLICY ... ADD FILTER/PREDICATE naming it -- so the route here is
-- the same one the Postgres migration takes, an object with no policy on it,
-- rather than MySQL's "there is no RLS to be outside of".
-- RE-RUNNABLE, and on this dialect that is not free. PostgreSQL and MySQL write
-- CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT EXISTS; SQL Server has
-- NEITHER, so the guarded form is `IF OBJECT_ID(<name>, 'U') IS NULL` -- the
-- idiom plugin migrations already use for the same reason
-- (engine/a_tenant_can_be_dropped_on_sql_server_test.go, plugin/migration_down_test.go).
--
-- Both statements need it, and the index needs its OWN guard rather than riding
-- on the table's: on a re-run the table exists AND the index exists, so an
-- unguarded CREATE INDEX fails with 1913 even though the CREATE TABLE was
-- correctly skipped -- fixing one and not the other just moves the error.
--
-- WHY THIS MATTERS, and it was found by a test rather than by reading.
-- cmd/cleat-worker/a_migration_is_a_deploy_step_test.go step 6 deletes the
-- NEWEST migration's row from schema_migrations and re-applies it, which is the
-- repair path for a worker whose tracking table lost its last version. So the
-- newest migration must be re-runnable against a database where its objects
-- already exist -- and this file is the FIRST MSSQL migration since
-- 001_schema.sql to create a table, so nothing before it had exercised that.
-- Unguarded it failed with:
--
--   migration 009_operator_api_keys.sql: execute: mssql: There is already an
--   object named 'operator_api_keys' in the database. (2714)
--
-- on the mssql arm only, in Tier 1 Gate, while every fresh-database job passed.
IF OBJECT_ID('admin.operator_api_keys', 'U') IS NULL
BEGIN
    CREATE TABLE admin.operator_api_keys (
        key_id uniqueidentifier NOT NULL CONSTRAINT df_operator_api_keys_key_id DEFAULT (newid()),
        key_hash varbinary(32) NOT NULL,
        description nvarchar(max) NOT NULL CONSTRAINT df_operator_api_keys_description DEFAULT (''),
        created_at datetimeoffset(7) NOT NULL CONSTRAINT df_operator_api_keys_created_at DEFAULT (sysutcdatetime()),
        disabled_at datetimeoffset(7),
        expires_at datetimeoffset(7),
        updated_at datetimeoffset(7) NOT NULL CONSTRAINT df_operator_api_keys_updated_at DEFAULT (sysutcdatetime()),
        CONSTRAINT pk_admin_operator_api_keys PRIMARY KEY CLUSTERED (key_id)
    );
END
GO
-- Filtered, mirroring idx_api_keys_hash: every lookup is "is this presented
-- hash a live key", so a disabled row is never a candidate. Guarded by name
-- against the table, for the reason above.
IF NOT EXISTS (SELECT 1 FROM sys.indexes
               WHERE name = 'idx_operator_api_keys_hash'
                 AND object_id = OBJECT_ID('admin.operator_api_keys'))
BEGIN
    CREATE NONCLUSTERED INDEX idx_operator_api_keys_hash
        ON admin.operator_api_keys (key_hash) WHERE ([disabled_at] IS NULL);
END
GO
