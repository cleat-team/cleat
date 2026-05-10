-- cleat exactly-once semantics migration
-- Adds idempotency key support for exactly-once workflow start semantics.
-- When a workflow is started with an Idempotency-Key header, the key is stored
-- in this table so that retried API calls return the existing workflow ID instead
-- of creating a duplicate.
--
-- Keys automatically expire after 7 days and are periodically cleaned up.
--
-- Idempotent: all statements use IF NOT EXISTS / IF EXISTS where applicable.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key_hash    BYTEA NOT NULL PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    result      JSONB,
    error_msg   TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '7 days'
);

CREATE INDEX IF NOT EXISTS idx_idempotency_workflow_id
    ON idempotency_keys(workflow_id);

CREATE INDEX IF NOT EXISTS idx_idempotency_expires
    ON idempotency_keys(expires_at) WHERE expires_at < now();
