-- cleat#3540 (PR 1 of 4): the schema for workflow libraries. See
-- migrations/postgres/005_workflow_libraries.sql for the full reasoning; this is
-- the same change. SQL Server and PostgreSQL only (owner decision D41);
-- migrations/mysql has no 005.
--
-- What differs on SQL Server, and why:
--
-- * No default privileges to avoid. Schema-level grants (001: GRANT ... ON
--   SCHEMA::[dbo] TO [cleat_app_role]) cover a schema, so a table in a NEW schema
--   gets nothing: the application role is granted SELECT on [library], and a
--   separate role, cleat_library_publisher, SELECT, INSERT and DELETE. Nothing
--   grants UPDATE, so members are immutable. Do not grant the application role
--   anything else on [library].
--
-- * Key sizes. library is nvarchar(128), not 255: a clustered index key over 900
--   bytes is accepted at CREATE and then refuses the INSERT that exceeds it, and
--   (library, name, version) with a 255-character library is 1,024 bytes. At 128
--   it is 770. bundle_members' key adds bundle_version and is over 900 bytes
--   either way, so that primary key is NONCLUSTERED, whose limit is 1,700.
--   Whatever publishes a library has to refuse a name over 128 characters before
--   it reaches here.
--
-- * The digest check compares under a binary collation: the database's default
--   collation is usually case-insensitive, and a digest in the wrong case would
--   otherwise pass a check meant to say it equals the bytes' hash.
--
-- * admin.drop_tenant is NOT redefined here. It deletes from every table that has
--   a tenant_id column, found through sys.columns, so tenant_library_installs is
--   covered without a line of change and library.* (no tenant_id) is not touched.
--
-- * Row-level security is a security policy per table, as in 003.
--
-- Every statement is guarded, so a re-run after a failure elsewhere in the same
-- migration run is harmless.

IF SCHEMA_ID(N'library') IS NULL
    EXEC(N'CREATE SCHEMA [library]');
GO

IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_library_publisher' AND type = 'R')
    CREATE ROLE [cleat_library_publisher];
GO

IF OBJECT_ID(N'library.members', N'U') IS NULL
CREATE TABLE library.members (
    library nvarchar(128) NOT NULL,
    name nvarchar(255) NOT NULL,
    version int NOT NULL,
    wasm_bytes varbinary(max) NOT NULL,
    wasm_sha256 varchar(64) NOT NULL,
    wasm_length bigint NOT NULL,
    abi_version int NOT NULL,
    min_version int NOT NULL CONSTRAINT df_members_min_version DEFAULT ((0)),
    plugin_deps nvarchar(max) NOT NULL CONSTRAINT df_members_plugin_deps DEFAULT ('{}'),
    entry_points nvarchar(max) NOT NULL CONSTRAINT df_members_entry_points DEFAULT ('[]'),
    task_queue nvarchar(255) NOT NULL CONSTRAINT df_members_task_queue DEFAULT ('default'),
    max_history_length int NOT NULL CONSTRAINT df_members_max_history_length DEFAULT ((0)),
    dag_spec nvarchar(max) NULL,
    entry_point_schemas nvarchar(max) NULL,
    exposure nvarchar(16) NOT NULL CONSTRAINT df_members_exposure DEFAULT ('auth'),
    input_validation_disabled bit NOT NULL CONSTRAINT df_members_input_validation_disabled DEFAULT ((0)),
    published_at datetimeoffset(7) NOT NULL CONSTRAINT df_members_published_at DEFAULT (sysutcdatetime()),
    CONSTRAINT pk_members PRIMARY KEY CLUSTERED (library, name, version),
    CONSTRAINT ck_members_exposure CHECK ([exposure]='internal' OR [exposure]='auth' OR [exposure]='public'),
    CONSTRAINT ck_members_digest CHECK ([wasm_sha256] COLLATE Latin1_General_BIN2 = LOWER(CONVERT(varchar(64), HASHBYTES('SHA2_256', [wasm_bytes]), 2))),
    CONSTRAINT ck_members_length CHECK ([wasm_length] = DATALENGTH([wasm_bytes])),
    CONSTRAINT ck_members_plugin_deps CHECK (isjson([plugin_deps])=(1)),
    CONSTRAINT ck_members_dag_spec CHECK ([dag_spec] IS NULL OR isjson([dag_spec])=(1)),
    CONSTRAINT ck_members_entry_point_schemas CHECK ([entry_point_schemas] IS NULL OR isjson([entry_point_schemas])=(1))
);
GO

IF OBJECT_ID(N'library.bundles', N'U') IS NULL
CREATE TABLE library.bundles (
    library nvarchar(128) NOT NULL,
    bundle_version nvarchar(128) NOT NULL,
    published_at datetimeoffset(7) NOT NULL CONSTRAINT df_bundles_published_at DEFAULT (sysutcdatetime()),
    published_by nvarchar(255) NULL,
    CONSTRAINT pk_bundles PRIMARY KEY CLUSTERED (library, bundle_version)
);
GO

-- Which members a bundle contains; a member unchanged between two bundles is one
-- members row listed by both. NO ACTION on both keys, which is RESTRICT: a listed
-- bundle or member cannot be deleted from under the listing.
IF OBJECT_ID(N'library.bundle_members', N'U') IS NULL
CREATE TABLE library.bundle_members (
    library nvarchar(128) NOT NULL,
    bundle_version nvarchar(128) NOT NULL,
    name nvarchar(255) NOT NULL,
    version int NOT NULL,
    CONSTRAINT pk_bundle_members PRIMARY KEY NONCLUSTERED (library, bundle_version, name),
    CONSTRAINT fk_bundle_members_bundle FOREIGN KEY (library, bundle_version)
        REFERENCES library.bundles (library, bundle_version),
    CONSTRAINT fk_bundle_members_member FOREIGN KEY (library, name, version)
        REFERENCES library.members (library, name, version)
);
GO

-- Read only for the application role; the publisher writes and cannot update.
GRANT SELECT ON SCHEMA::[library] TO [cleat_app_role];
GO

GRANT SELECT ON SCHEMA::[library] TO [cleat_library_publisher];
GO

GRANT INSERT ON SCHEMA::[library] TO [cleat_library_publisher];
GO

GRANT DELETE ON SCHEMA::[library] TO [cleat_library_publisher];
GO

IF COL_LENGTH(N'dbo.workflow_defs', N'library') IS NULL
    ALTER TABLE dbo.workflow_defs ADD library nvarchar(128) NULL;
GO

-- Idempotent: setting a nullable column to NULL again changes nothing.
ALTER TABLE dbo.workflow_defs ALTER COLUMN wasm_bytes varbinary(max) NULL;
GO

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints
               WHERE name = N'ck_workflow_defs_bytes_or_library'
                 AND parent_object_id = OBJECT_ID(N'dbo.workflow_defs'))
    ALTER TABLE dbo.workflow_defs ADD CONSTRAINT ck_workflow_defs_bytes_or_library
        CHECK (([library] IS NULL AND [wasm_bytes] IS NOT NULL) OR ([library] IS NOT NULL AND [wasm_bytes] IS NULL));
GO

-- With library NULL a SQL Server foreign key is not checked, so rows with their own
-- bytes are unaffected. NO ACTION is RESTRICT: a member a tenant references cannot
-- be deleted, including a tenant the deleting session cannot see.
IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys
               WHERE name = N'fk_workflow_defs_library'
                 AND parent_object_id = OBJECT_ID(N'dbo.workflow_defs'))
    ALTER TABLE dbo.workflow_defs ADD CONSTRAINT fk_workflow_defs_library
        FOREIGN KEY (library, name, version) REFERENCES library.members (library, name, version);
GO

-- What the foreign key's delete check scans. NOT FILTERED, unlike PostgreSQL's:
-- with a filtered index (WHERE library IS NOT NULL) here, every
-- DELETE FROM library.members fails with Msg 8624, "The query processor could not
-- produce a query plan", for any caller including sysadmin, because the delete has
-- to validate this foreign key. Measured on SQL Server 2022: a delete of an
-- unreferenced member fails with the filtered index and succeeds with it dropped.
-- Rows with their own bytes have library NULL, so the unfiltered index carries an
-- entry for each of them; workflow_defs is one row per deployed version, small
-- beside the bytes it points at.
IF NOT EXISTS (SELECT 1 FROM sys.indexes
               WHERE name = N'idx_defs_library' AND object_id = OBJECT_ID(N'dbo.workflow_defs'))
    CREATE NONCLUSTERED INDEX idx_defs_library ON dbo.workflow_defs (library, name, version);
GO

IF OBJECT_ID(N'dbo.tenant_library_installs', N'U') IS NULL
CREATE TABLE dbo.tenant_library_installs (
    tenant_id uniqueidentifier NOT NULL
        CONSTRAINT df_tenant_library_installs_tenant_id DEFAULT ('00000000-0000-0000-0000-000000000000'),
    library nvarchar(128) NOT NULL,
    bundle_version nvarchar(128) NOT NULL,
    installed_at datetimeoffset(7) NOT NULL
        CONSTRAINT df_tenant_library_installs_installed_at DEFAULT (sysutcdatetime()),
    installed_by nvarchar(255) NULL,
    CONSTRAINT pk_tenant_library_installs PRIMARY KEY CLUSTERED (tenant_id, library)
);
GO

-- EXEC, so the guard and the statement sit together: CREATE SECURITY POLICY wants a
-- batch of its own. dbo.fn_tenant_filter is created by 003 and is not changed here.
IF OBJECT_ID(N'dbo.TenantFilter_LibraryInstalls', N'SP') IS NULL
    EXEC(N'CREATE SECURITY POLICY dbo.TenantFilter_LibraryInstalls
        ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_library_installs,
        ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_library_installs AFTER INSERT,
        ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_library_installs AFTER UPDATE,
        ADD BLOCK PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.tenant_library_installs BEFORE UPDATE
        WITH (STATE = ON)');
GO
