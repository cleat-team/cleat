-- Migration 008: Workflow versioning support
-- Adds versioning columns to workflow_defs, creates plugin_defs table,
-- and adds plugin_vers to workflow_instances for plugin resolution pinning.

-- ---------------------------------------------------------------------------
-- Extend workflow_defs with versioning metadata
-- ---------------------------------------------------------------------------
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS abi_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS plugin_deps JSONB NOT NULL DEFAULT '{}';
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS deprecated BOOLEAN NOT NULL DEFAULT false;

-- ---------------------------------------------------------------------------
-- Plugin definitions table
-- Plugins (LLM, blobstore, Slack, etc.) are versioned separately from
-- workflows using semver strings. wasm_bytes is NULL for host-native plugins.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS plugin_defs (
    name         TEXT        NOT NULL,
    version      TEXT        NOT NULL,
    wasm_bytes   BYTEA,
    config       JSONB       NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deprecated   BOOLEAN     NOT NULL DEFAULT false,
    PRIMARY KEY (name, version)
);

-- ---------------------------------------------------------------------------
-- Extend workflow_instances with resolved plugin versions
-- plugin_vers stores the pinned plugin versions resolved at instance creation
-- time, e.g. {"llm": "1.2.0", "blobstore": "2.1.3"}
-- ---------------------------------------------------------------------------
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS plugin_vers JSONB NOT NULL DEFAULT '{}';
