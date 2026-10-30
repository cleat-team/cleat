-- cleat#3600 (owner D52): which plugin NAMES some worker has registered, at least once. See
-- migrations/postgres/008 for the full reasoning; this is the same change.
--
-- Guarded on the table, so a re-run is a no-op. Never deleted from by anything.
IF OBJECT_ID(N'admin.registered_plugins', N'U') IS NULL
    CREATE TABLE admin.registered_plugins (
        plugin_name   NVARCHAR(128) NOT NULL,
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
