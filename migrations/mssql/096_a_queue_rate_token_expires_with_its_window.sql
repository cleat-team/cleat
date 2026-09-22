-- cleat migration 096 (mssql): a queue rate token expires with its window
--
-- See migrations/postgres/097_a_queue_rate_token_expires_with_its_window.sql
-- for the full reasoning: cleat#1918, expires_at rather than admitted_at, no
-- FK on workflow_id (a rate token's lifetime must not couple to
-- -completed-workflow-retention-days), shaped like queue_holders rather than
-- queues. This header records only what differs here.
--
-- NO SECURITY POLICY, NO TENANT-ROLE GRANT, matching queue_holders (093) for
-- the same reason: no per-tenant login role on this dialect, and this table
-- is written and read by the same claim path under the explicit
-- `AND tenant_id = @pN` predicate that is the whole of the scoping here.
--
-- FK ON tenant_id, UNLIKE queue_holders. queue_holders relies entirely on its
-- workflow_id cascade for cleanup; this table has no workflow_id FK (see the
-- postgres header for why), so a tenant FK is what keeps a dropped tenant's
-- rate tokens from outliving the tenant.
--
-- NVARCHAR(128) for queue_name, matching queues.name and queue_holders'
-- own reasoning.

CREATE TABLE dbo.queue_rate_tokens (
    tenant_id    UNIQUEIDENTIFIER NOT NULL,
    queue_name   NVARCHAR(128)    NOT NULL,
    workflow_id  NVARCHAR(255)    NOT NULL,
    expires_at   DATETIMEOFFSET   NOT NULL,

    CONSTRAINT fk_queue_rate_tokens_tenant FOREIGN KEY (tenant_id)
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE
);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_queue_rate_tokens_window' AND object_id = OBJECT_ID(N'dbo.queue_rate_tokens'))
    CREATE INDEX idx_queue_rate_tokens_window ON dbo.queue_rate_tokens(tenant_id, queue_name, expires_at);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_queue_rate_tokens_expires' AND object_id = OBJECT_ID(N'dbo.queue_rate_tokens'))
    CREATE INDEX idx_queue_rate_tokens_expires ON dbo.queue_rate_tokens(expires_at);
GO
