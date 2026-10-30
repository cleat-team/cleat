-- cleat#3600 (owner D52): which plugin NAMES some worker has registered, at least once. See
-- migrations/postgres/008 for the full reasoning; this is the same change.
--
-- Guarded on the table, so a re-run is a no-op. Never deleted from by anything.
--
-- plugin_name is compared BINARY (Latin1_General_100_BIN2). The database's default collation folds
-- case, so "Slack-Notify" would match a registered "slack-notify", while a worker's own claim-time
-- check (checkPluginDeps) is an exact map lookup and would refuse it: the deploy check must say
-- what the worker will say.
IF OBJECT_ID(N'admin.registered_plugins', N'U') IS NULL
    CREATE TABLE admin.registered_plugins (
        plugin_name   NVARCHAR(128) COLLATE Latin1_General_100_BIN2 NOT NULL,
        first_seen_at DATETIMEOFFSET(7) NOT NULL
            CONSTRAINT df_registered_plugins_first DEFAULT SYSUTCDATETIME(),
        last_seen_at  DATETIMEOFFSET(7) NOT NULL
            CONSTRAINT df_registered_plugins_last DEFAULT SYSUTCDATETIME(),
        CONSTRAINT pk_registered_plugins PRIMARY KEY CLUSTERED (plugin_name),
        CONSTRAINT ck_registered_plugins_name CHECK (LEN(plugin_name) BETWEEN 1 AND 128)
    );
GO

-- The application role already has SELECT, INSERT and UPDATE on the admin schema (001), which
-- is all registration and the check need. DELETE is DENIED on this table, as 001 does for
-- deployment_secrets: "nothing deletes from this table" is a privilege and not only a
-- promise in a comment, so a future sweep written against it fails with a permission error
-- instead of quietly turning a deploy into "some worker is live now".
DENY DELETE ON [admin].[registered_plugins] TO [cleat_app_role];
GO

-- cleat#3600: one row for every use of the explicit override. See migrations/postgres/008 for the
-- full reasoning; this is the same change. Guarded on the table, so a re-run is a no-op.
IF OBJECT_ID(N'admin.plugin_dep_overrides', N'U') IS NULL
    CREATE TABLE admin.plugin_dep_overrides (
        override_id        NVARCHAR(64) NOT NULL,
        occurred_at        DATETIMEOFFSET(7) NOT NULL
            CONSTRAINT df_plugin_dep_overrides_at DEFAULT SYSUTCDATETIME(),
        tenant_id          NVARCHAR(64) NOT NULL,
        definition_name    NVARCHAR(255) NOT NULL,
        definition_version INT NOT NULL,
        via                NVARCHAR(64) NOT NULL,
        actor              NVARCHAR(255) NOT NULL
            CONSTRAINT df_plugin_dep_overrides_actor DEFAULT N'',
        missing_plugins    NVARCHAR(MAX) NOT NULL,
        CONSTRAINT pk_plugin_dep_overrides PRIMARY KEY CLUSTERED (override_id)
    );
GO

-- Append-only: the application role keeps the admin schema's SELECT and INSERT and may not update or
-- delete a row.
DENY UPDATE ON [admin].[plugin_dep_overrides] TO [cleat_app_role];
GO
DENY DELETE ON [admin].[plugin_dep_overrides] TO [cleat_app_role];
GO
