-- cleat migration 104 (postgres): a Slack workspace maps to one tenant
--
-- cleat#2230. slacknotify's interactive-callback route (POST
-- /slack/interactive, cleat#2172) is auth-exempt -- it carries no cleat API
-- key and no Host binding, only Slack's own HMAC signature -- so it has no
-- way to know which tenant a button click belongs to. Before this table, a
-- verified click was signalled unscoped, landing on the process-wide default
-- tenant's store (cmd/cleat-worker/main.go's signalPluginWorkflow doc
-- comment), which is the same shape cleat#2209 already fixed for
-- webhookingest/eventtriggers. cleat#2253's interim fix refuses every click
-- with 404 until this table exists.
--
-- OPERATOR-OWNED, NOT TENANT-ASSERTED (owner decision, relayed on
-- cleat#2230, "3A"). A tenant-supplied team_id would let any tenant switch
-- off, or squat on, another tenant's Slack workspace -- there is no proof
-- step (no OAuth install flow exists in this plugin) that would make a
-- tenant's claim to a team_id trustworthy. So this table is written only by
-- an operator, through cleatctl (see the REVOKE below), the same asymmetry
-- deployment_secrets and tenant_domains already have for their own
-- operator-only writes.
--
-- NO ROW-LEVEL SECURITY, unlike tenant_domains (080), which this table
-- otherwise resembles (a hostname/team_id primary key, tenant_id FK,
-- CASCADE). tenant_domains's RLS policy applies AFTER a tenant is already in
-- context -- HostBindingMiddleware only ever VERIFIES an already-resolved
-- tenant against Host, it never derives one from it (see its own doc
-- comment). The interactive callback is the opposite shape: team_id is the
-- ONLY thing known at that point, and the query's whole job is to RESOLVE a
-- tenant from it, so there is no tenant_id to set in the session before the
-- lookup runs. A table with no tenant already available to scope by has
-- nothing for a row-level policy to filter -- the same reasoning
-- deployment_secrets' header gives for carrying no tenant_id column at all.
-- Here there IS a tenant_id column (needed for the CASCADE and for
-- cleatctl's reverse lookups), so the precedent to follow is
-- deployment_secrets' REVOKE, not tenant_domains' policy.
--
-- team_id IS BOUNDED AND CHECKED, not NVARCHAR(MAX)/TEXT-unconstrained: it's
-- the lookup key on every interactive callback and SQL Server cannot index
-- an unbounded column (same reasoning as tenant_domains' hostname). Slack's
-- own team_id shape is a leading 'T' (or 'E' for an Enterprise Grid org
-- unit) followed by alphanumerics; the CHECK constrains this table to what
-- Slack actually issues rather than accepting arbitrary strings an operator
-- fat-fingers.
--
-- ON DELETE CASCADE: admin.drop_tenant deletes admin.tenants last, so a
-- tenant drop takes its workspace mapping(s) with it on every dialect
-- without a hand-maintained delete step (039's reasoning, restated by 080).
-- The table is also added to cleatctl's dropTenantTables, for the count it
-- reports.

CREATE TABLE IF NOT EXISTS slack_workspace (
    team_id    TEXT PRIMARY KEY,

    tenant_id  UUID NOT NULL
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_slack_workspace_team_id_shape
        CHECK (team_id ~ '^[TE][A-Z0-9]+$'),
    CONSTRAINT ck_slack_workspace_team_id_length
        CHECK (length(team_id) <= 32)
);

-- The reverse lookup: an operator listing which workspace(s) map to a
-- tenant, and cleatctl's drop-tenant preview count.
CREATE INDEX IF NOT EXISTS idx_slack_workspace_tenant ON slack_workspace(tenant_id);

REVOKE ALL ON slack_workspace FROM PUBLIC;

-- cleat_app keeps SELECT (granted by 005_app_role.sql's default privileges,
-- and needed for the worker's per-request team_id -> tenant_id lookup on
-- POST /slack/interactive); INSERT/UPDATE/DELETE are removed so a worker
-- process cannot also write a mapping. Writes stay on cleatctl's elevated
-- connection -- `cleatctl slack map-workspace` / `unmap-workspace` -- the
-- same split deployment_secrets already has.
REVOKE INSERT, UPDATE, DELETE ON slack_workspace FROM cleat_app;
