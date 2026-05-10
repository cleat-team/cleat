-- cleat MySQL migration 005: exactly-once semantics
-- Adds idempotency key support for exactly-once workflow start semantics.
-- When a workflow is started with an Idempotency-Key header, the key is stored
-- in this table so that retried API calls return the existing workflow ID instead
-- of creating a duplicate.
--
-- Keys automatically expire after 7 days and are periodically cleaned up.
--
-- MySQL differences from PostgreSQL:
--   - BYTEA becomes VARBINARY(64) for SHA-256 hash
--   - TIMESTAMPTZ becomes TIMESTAMP(6)
--   - JSONB becomes JSON
--   - now() becomes NOW(6)
--   - INTERVAL '7 days' becomes INTERVAL 7 DAY (no quotes)
--   - Partial index (WHERE) omitted -- MySQL does not support them
--   - Idempotent: CREATE TABLE IF NOT EXISTS

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key_hash           VARBINARY(64) NOT NULL,
    workflow_id        VARCHAR(255) NOT NULL,
    result             JSON,
    error_msg          TEXT,
    created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    expires_at         TIMESTAMP(6) NOT NULL DEFAULT (NOW(6) + INTERVAL 7 DAY),
    PRIMARY KEY (key_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_idempotency_workflow_id ON idempotency_keys(workflow_id);

-- Note: Partial index WHERE expires_at < now() omitted. MySQL does not
-- support partial indexes. Application-level filtering for expired rows
-- is required when querying the expiration cleanup cursor.
CREATE INDEX idx_idempotency_expires ON idempotency_keys(expires_at);
