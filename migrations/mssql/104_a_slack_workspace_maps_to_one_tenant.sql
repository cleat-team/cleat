-- cleat migration 104 (mssql): a Slack workspace maps to one tenant
--
-- cleat#2230. The PostgreSQL counterpart (104) carries the full reasoning;
-- this header records only what differs on SQL Server.
--
-- SCHEMA-QUALIFIED (dbo. for this table, admin. for the tenants FK target),
-- the lesson cleat#1568 cost tenant_domains' own mssql migration (072).
--
-- NO SECURITY POLICY. This table has no tenant already in context to filter
-- by at read time -- the query's job is to RESOLVE a tenant FROM team_id,
-- the same reasoning the PostgreSQL header gives for using REVOKE instead of
-- a row-level policy. There is also no cleat_app-equivalent role split on
-- this dialect to REVOKE from (092's, 102's own precedent) -- every
-- principal, including sysadmin, is subject to the same predicate
-- mechanism, and this table has no predicate. Protection is cleatctl-only,
-- the same caveat 102's own header already carries for deployment_secrets.
--
-- NO CHECK CONSTRAINT ON team_id's SHAPE, following 092's and 073's own
-- reasoning: SQL Server has no regexp in a CHECK, and a LIKE pattern cannot
-- express a bounded repeat of a character class. The guarantee is Go-level
-- (cleatctl's team_id validator), which every write goes through -- this
-- table's writes all come from cleatctl, unlike most tables on this dialect.
-- NVARCHAR(32) still bounds the STORED LENGTH, which is not a character-class
-- problem and needs no CHECK to enforce.
--
-- GUARDED BY OBJECT EXISTENCE, following 102's own precedent: cleat#2117's
-- deploy-step test exercises "the newest migration's tracking row is
-- missing, --migrate-only repairs it" against whichever migration is
-- highest-numbered, which this one now is on this dialect as of writing.

IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.slack_workspace') AND type = N'U')
CREATE TABLE dbo.slack_workspace (
    team_id    NVARCHAR(32)     NOT NULL,
    tenant_id  UNIQUEIDENTIFIER NOT NULL,
    created_at DATETIMEOFFSET   NOT NULL CONSTRAINT df_slack_workspace_created DEFAULT SYSUTCDATETIME(),

    CONSTRAINT pk_slack_workspace PRIMARY KEY (team_id),
    CONSTRAINT fk_slack_workspace_tenant FOREIGN KEY (tenant_id)
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE
);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_slack_workspace_tenant' AND object_id = OBJECT_ID(N'dbo.slack_workspace'))
CREATE INDEX idx_slack_workspace_tenant ON dbo.slack_workspace(tenant_id);
GO
