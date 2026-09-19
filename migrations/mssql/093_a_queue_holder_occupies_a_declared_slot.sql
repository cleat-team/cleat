-- cleat migration 093 (mssql): a queue holder occupies a declared slot
--
-- See migrations/postgres/094_a_queue_holder_occupies_a_declared_slot.sql for
-- the full reasoning: cleat#1116 second piece, transient holder state (not
-- #1702-conforming), shaped like concurrency_keys rather than queues, tenant
-- scoping by primary key plus explicit tenant_id. This header records only what
-- differs here.
--
-- NO SECURITY POLICY. concurrency_keys (001) carries none on this dialect, for
-- the reason recorded on claimWorkflowsOnce: the candidate read's explicit
-- `AND tenant_id = @p4` is the whole of the scoping, because dbo.fn_tenant_filter
-- is off for the admin role. queue_holders is written and read by the same claim
-- path and under the same explicit predicate, so a policy would only re-state it.
--
-- NO TENANT-ROLE GRANT, matching 092 and 064's own reasoning: there is no
-- per-tenant login role on this dialect to grant to -- session context is the
-- mechanism.
--
-- UNGUARDED CREATE TABLE, matching 092's own precedent for a brand-new table on
-- this dialect (checked rather than assumed: a brand-new table has no existence
-- guard to check).
--
-- NVARCHAR(128) for queue_name, matching queues.name (092).

CREATE TABLE dbo.queue_holders (
    tenant_id    UNIQUEIDENTIFIER NOT NULL,
    queue_name   NVARCHAR(128)    NOT NULL,
    workflow_id  NVARCHAR(255)    NOT NULL,
    expires_at   DATETIMEOFFSET   NOT NULL,

    CONSTRAINT pk_queue_holders PRIMARY KEY (tenant_id, queue_name, workflow_id),
    CONSTRAINT fk_queue_holders_workflow FOREIGN KEY (workflow_id)
        REFERENCES dbo.workflow_instances(id) ON DELETE CASCADE
);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_queue_holders_workflow' AND object_id = OBJECT_ID(N'dbo.queue_holders'))
    CREATE INDEX idx_queue_holders_workflow ON dbo.queue_holders(workflow_id);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_queue_holders_expires' AND object_id = OBJECT_ID(N'dbo.queue_holders'))
    CREATE INDEX idx_queue_holders_expires ON dbo.queue_holders(expires_at);
GO
