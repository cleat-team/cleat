-- Migration 008: Workflow versioning support (T-SQL for SQL Server 2017+ / Azure SQL Database)
-- Adds versioning columns to workflow_defs, creates plugin_defs table,
-- and adds plugin_vers to workflow_instances for plugin resolution pinning.

-- ===========================================================================
-- Extend workflow_defs with versioning metadata
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'abi_version')
    ALTER TABLE dbo.workflow_defs ADD abi_version INT NOT NULL DEFAULT 1;

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'plugin_deps')
    ALTER TABLE dbo.workflow_defs ADD plugin_deps NVARCHAR(MAX) NOT NULL DEFAULT '{}';

IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_defs') AND name = N'deprecated')
    ALTER TABLE dbo.workflow_defs ADD deprecated BIT NOT NULL DEFAULT 0;

-- ===========================================================================
-- Plugin definitions table
-- Plugins (LLM, blobstore, Slack, etc.) are versioned separately from
-- workflows using semver strings. wasm_bytes is NULL for host-native plugins.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.plugin_defs') AND type = N'U')
CREATE TABLE dbo.plugin_defs (
    name         NVARCHAR(255)   NOT NULL,
    version      NVARCHAR(64)    NOT NULL,
    wasm_bytes   VARBINARY(MAX)  NULL,
    config       NVARCHAR(MAX)   NOT NULL DEFAULT '{}',
    created_at   DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    deprecated   BIT             NOT NULL DEFAULT 0,
    CONSTRAINT pk_plugin_defs PRIMARY KEY (name, version),
    CONSTRAINT ck_plugin_defs_config CHECK (ISJSON(config) = 1)
);

-- ===========================================================================
-- Extend workflow_instances with resolved plugin versions
-- plugin_vers stores the pinned plugin versions resolved at instance creation
-- time, e.g. {"llm": "1.2.0", "blobstore": "2.1.3"}
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.workflow_instances') AND name = N'plugin_vers')
    ALTER TABLE dbo.workflow_instances ADD plugin_vers NVARCHAR(MAX) NOT NULL DEFAULT '{}';
