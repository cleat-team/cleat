-- cleat migration 092 (mysql): a queue declares its own concurrency limit
--
-- See migrations/postgres/093_a_queue_declares_its_own_concurrency_limit.sql
-- for the full reasoning: cleat#1116, explicit registration rather than
-- implicit-on-first-use, #1702-conforming (created_at/updated_at/disabled_at,
-- no new retirement spelling). This header records only what differs here.
--
-- NO ROW-LEVEL SECURITY, because MySQL has none -- tenant isolation on this
-- dialect is a database per tenant, following tenant_secrets (069) and every
-- other tenant-scoped table in this directory.
--
-- No admin schema, matching every other table here: `tenants`, not
-- `admin.tenants`.
--
-- VARCHAR(128) for name, matching tenant_secrets's own reasoning: half the
-- primary key, and MySQL cannot index TEXT without a prefix length.

CREATE TABLE IF NOT EXISTS queues (
    tenant_id           CHAR(36)     NOT NULL,
    name                VARCHAR(128) NOT NULL,
    concurrency_limit   INT          NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    updated_at          TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    disabled_at         TIMESTAMP(6) NULL,

    PRIMARY KEY (tenant_id, name),

    CONSTRAINT fk_queues_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id) ON DELETE CASCADE,

    CONSTRAINT ck_queues_name_charset
        CHECK (name REGEXP '^[A-Za-z0-9_.-]{1,128}$'),
    CONSTRAINT ck_queues_concurrency_limit_positive
        CHECK (concurrency_limit >= 1)
);
