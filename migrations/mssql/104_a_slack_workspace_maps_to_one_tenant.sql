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
-- CHECK CONSTRAINT ON team_id's SHAPE, unlike 092's and 073's own tables --
-- this file said "a LIKE pattern cannot express a bounded repeat of a
-- character class" until it was measured wrong (cleat-review, cleat#2230).
-- A LIKE pattern cannot express a bounded REPEAT, but it does not need to
-- here: this shape has no upper bound of its own (NVARCHAR(32) already
-- bounds the stored length, a separate concern), so two LIKE clauses over
-- an explicitly BINARY collation suffice -- the leading character, then
-- "nothing outside [A-Z0-9] anywhere in the rest of the string":
--
--     team_id COLLATE Latin1_General_BIN2 LIKE N'[TE][A-Z0-9]%'
--     AND team_id COLLATE Latin1_General_BIN2 NOT LIKE N'%[^A-Z0-9]%'
--
-- COLLATE ... BIN2 is not decoration: SQL Server's default collations are
-- case-INSENSITIVE, so LIKE N'[A-Z0-9]%' against that default would also
-- accept lowercase -- the same fold hazard MySQL's CHECK has below, and the
-- reason this needs a binary collation named explicitly rather than
-- inherited from the column or database. Measured directly (junk inserts):
-- lowercase, a hyphen, and a bare "T" with no second character all conflict
-- with the constraint; T0123ABCD and E0123ABCD both succeed.
--
-- This still does not replace cleatctl's Go-level validator -- this table's
-- writes all come from cleatctl, and the two together are what "every write
-- goes through the Go validator" upgrades to "every write is provably bound
-- to this shape even from a raw connection", the same reasoning migration
-- 104's mysql sibling gives for REGEXP_LIKE(...,'c') there.
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
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,
    CONSTRAINT ck_slack_workspace_team_id_shape
        CHECK (team_id COLLATE Latin1_General_BIN2 LIKE N'[TE][A-Z0-9]%'
           AND team_id COLLATE Latin1_General_BIN2 NOT LIKE N'%[^A-Z0-9]%')
);
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_slack_workspace_tenant' AND object_id = OBJECT_ID(N'dbo.slack_workspace'))
CREATE INDEX idx_slack_workspace_tenant ON dbo.slack_workspace(tenant_id);
GO
