-- cleat migration 103 (mysql): a Slack workspace maps to one tenant
--
-- cleat#2230. The PostgreSQL counterpart (104) carries the full reasoning;
-- this header records only what differs on MySQL.
--
-- NO REVOKE, following 102's own precedent for deployment_secrets: MySQL has
-- no cleat_app counterpart at all (`git grep -n cleat_app -- migrations/mysql/`
-- is empty) -- one database login, and tenant isolation here is a database
-- per tenant rather than a role split within one, so there is nothing to
-- remove write access from without also removing the worker's own read path.
-- The operator-only-write property this table needs is enforced by cleatctl
-- alone on this dialect, the same caveat deployment_secrets' own docs
-- already carry.
--
-- team_id VARCHAR(32), unqualified `tenants(tenant_id)` FK: the same shape
-- tenant_domains' own mysql migration (068) uses for its bounded key and its
-- reference to the (per-tenant-database) tenants table.
--
-- team_id's column COLLATION IS utf8mb4_bin, not the database default
-- (utf8mb4_0900_ai_ci) -- measured wrong under the default (cleat-review,
-- cleat#2230). The database's default collation is accent- AND
-- case-INSENSITIVE, so under it 'tlower1' satisfies
-- `CHECK (team_id REGEXP '^[TE][A-Z0-9]+$')` (REGEXP folds case the same way
-- string comparison does), and 'TUPPER3' collides with 'tupper3' on the
-- PRIMARY KEY -- two different Slack workspaces would be one row. A binary
-- column collation fixes both at once: REGEXP becomes case-sensitive over
-- that column, and the PK compares bytes rather than folded text. This is a
-- narrower fix than REGEXP_LIKE(...,'c'), which would leave the PK's fold
-- untouched.

CREATE TABLE IF NOT EXISTS slack_workspace (
    team_id    VARCHAR(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    tenant_id  CHAR(36)    NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),

    PRIMARY KEY (team_id),
    KEY idx_slack_workspace_tenant (tenant_id),

    CONSTRAINT fk_slack_workspace_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id) ON DELETE CASCADE,

    CONSTRAINT ck_slack_workspace_team_id_shape
        CHECK (team_id REGEXP '^[TE][A-Z0-9]+$')
);
