-- cleat migration 093 (mysql): a queue holder occupies a declared slot
--
-- See migrations/postgres/094_a_queue_holder_occupies_a_declared_slot.sql for
-- the full reasoning: cleat#1116 second piece, transient holder state (not
-- #1702-conforming), shaped like concurrency_keys rather than queues, tenant
-- scoping by primary key plus explicit tenant_id. This header records only
-- what differs here.
--
-- NO ROW-LEVEL SECURITY, because MySQL has none -- tenant isolation on this
-- dialect is a database per tenant, following concurrency_keys (001) and every
-- other table in this directory.
--
-- No admin schema, matching every other table here: `tenants`, not
-- `admin.tenants`.
--
-- VARCHAR(128) for queue_name, matching queues.name (092) and its own
-- reasoning: it is half the primary key, and MySQL cannot index TEXT without a
-- prefix length.

CREATE TABLE IF NOT EXISTS queue_holders (
    tenant_id    CHAR(36)      NOT NULL,
    queue_name   VARCHAR(128)  NOT NULL,
    workflow_id  VARCHAR(255)  NOT NULL,
    expires_at   TIMESTAMP(6)  NOT NULL,

    PRIMARY KEY (tenant_id, queue_name, workflow_id),

    CONSTRAINT fk_queue_holders_workflow FOREIGN KEY (workflow_id)
        REFERENCES workflow_instances(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE INDEX idx_queue_holders_workflow ON queue_holders(workflow_id);
CREATE INDEX idx_queue_holders_expires ON queue_holders(expires_at);
