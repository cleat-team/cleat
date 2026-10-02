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
GO
-- Filtered, mirroring idx_api_keys_hash: every lookup is "is this presented
-- hash a live key", so a disabled row is never a candidate.
CREATE NONCLUSTERED INDEX idx_operator_api_keys_hash
    ON admin.operator_api_keys (key_hash) WHERE ([disabled_at] IS NULL);
GO
