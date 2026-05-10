-- cleat MySQL migration 008: workflow versioning support
-- Adds versioning columns to workflow_defs, creates plugin_defs table,
-- and adds plugin_vers to workflow_instances for plugin resolution pinning.
--
-- MySQL differences from PostgreSQL:
--   - JSONB becomes JSON
--   - BOOLEAN becomes TINYINT(1)
--   - BYTEA becomes LONGBLOB
--   - TIMESTAMPTZ becomes TIMESTAMP(6)
--   - now() becomes NOW(6)
--   - ADD COLUMN IF NOT EXISTS becomes ADD COLUMN with safety comment
--   - Idempotent: CREATE TABLE IF NOT EXISTS; verify column absence for ALTER

-- ---------------------------------------------------------------------------
-- Extend workflow_defs with versioning metadata
-- ---------------------------------------------------------------------------
-- ensure column does not exist before running
ALTER TABLE workflow_defs ADD COLUMN abi_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE workflow_defs ADD COLUMN plugin_deps JSON NOT NULL DEFAULT ('{}');
ALTER TABLE workflow_defs ADD COLUMN deprecated TINYINT(1) NOT NULL DEFAULT 0;

-- ---------------------------------------------------------------------------
-- Plugin definitions table
-- Plugins (LLM, blobstore, Slack, etc.) are versioned separately from
-- workflows using semver strings. wasm_bytes is NULL for host-native plugins.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS plugin_defs (
    name               VARCHAR(255) NOT NULL,
    version            VARCHAR(255) NOT NULL,
    wasm_bytes         LONGBLOB,
    config             JSON NOT NULL DEFAULT ('{}'),
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    deprecated         TINYINT(1) NOT NULL DEFAULT 0,
    PRIMARY KEY (name, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Extend workflow_instances with resolved plugin versions
-- plugin_vers stores the pinned plugin versions resolved at instance creation
-- time, e.g. {"llm": "1.2.0", "blobstore": "2.1.3"}
-- ---------------------------------------------------------------------------
-- ensure column does not exist before running
ALTER TABLE workflow_instances ADD COLUMN plugin_vers JSON NOT NULL DEFAULT ('{}');
