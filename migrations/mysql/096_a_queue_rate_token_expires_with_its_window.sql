-- cleat migration 096 (mysql): a queue rate token expires with its window
--
-- See migrations/postgres/097_a_queue_rate_token_expires_with_its_window.sql
-- for the full reasoning: cleat#1918, expires_at rather than admitted_at (one
-- comparison answers both "is this inside the window" and "can this be
-- reaped"), no FK on workflow_id (a rate token's lifetime must not couple to
-- -completed-workflow-retention-days), shaped like queue_holders rather than
-- queues. This header records only what differs here.
--
-- NO ROW-LEVEL SECURITY, because MySQL has none -- tenant isolation on this
-- dialect is a database per tenant, following queue_holders (093) and every
-- other table in this directory.
--
-- VARCHAR(128) for queue_name, matching queue_holders' own reasoning: MySQL
-- cannot index TEXT without a prefix length, and this table's whole purpose is
-- an index-driven count.

CREATE TABLE IF NOT EXISTS queue_rate_tokens (
    tenant_id    CHAR(36)      NOT NULL,
    queue_name   VARCHAR(128)  NOT NULL,
    workflow_id  VARCHAR(255)  NOT NULL,
    expires_at   TIMESTAMP(6)  NOT NULL,

    CONSTRAINT fk_queue_rate_tokens_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE INDEX idx_queue_rate_tokens_window ON queue_rate_tokens(tenant_id, queue_name, expires_at);
CREATE INDEX idx_queue_rate_tokens_expires ON queue_rate_tokens(expires_at);
