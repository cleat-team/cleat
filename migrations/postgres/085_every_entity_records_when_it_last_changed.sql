-- cleat migration 085 (postgres): every entity records when it last changed
--
-- cleat#1702. `updated_at TIMESTAMPTZ` on the eight members that lack it. The
-- other two members, tenant_settings and tenant_secrets, have had it since
-- migrations 039 and 081 and are not touched.
--
-- THE VALUE FOR EXISTING ROWS IS created_at, NOT now(). A one-step
-- `ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now()` would stamp every
-- existing row with the instant the migration ran, which is a claim about those
-- rows that is false: none of them were modified then. `created_at` is the
-- true answer -- a row that has not been updated since it was created was last
-- modified when it was created -- and it is available, because all eight
-- targets carry `created_at NOT NULL` in all three dialects. Measured on a
-- database built from the full migration set rather than read out of the files:
--
--     information_schema.columns, 8 of 8, created_at NOT NULL DEFAULT now()
--
-- Hence three steps rather than one. The column is added WITHOUT a default so
-- that existing rows get NULL and the backfill can see them; only then does it
-- become NOT NULL with a default for future inserts. Adding the default first
-- would fill the rows with now() and leave the UPDATE nothing to match --
-- silently, and with the wrong value already in place.
--
-- WHY THE BACKFILL WORKS HERE AND NEEDS A POLICY TOGGLE ON SQL SERVER. Five of
-- these eight tables carry RLS, enabled AND forced -- measured, pg_class
-- relrowsecurity and relforcerowsecurity are both true for workflow_defs,
-- workflow_schedules, workflow_routing, workflow_tags and tenant_domains. That
-- looks like it should hide the rows from this UPDATE and it does not, because
-- FORCE closes a DIFFERENT exemption than the one in play. 001_schema.sql:614
-- says it exactly: FORCE removes the TABLE OWNER's bypass; a superuser bypasses
-- RLS unconditionally and "cannot be forced". Migrations are applied by a
-- superuser in every configuration cleat ships (005_app_role.sql), so the rows
-- are visible here.
--
-- SQL Server has no equivalent exemption for sysadmin, which is why mssql/077
-- has to disable five security policies around the same backfill and this file
-- does not. Two engines, one migration, and the difference is not in the SQL.
--
-- WHAT THIS DOES NOT DO: maintain the column. Nothing in the engine writes
-- `updated_at` on these eight tables yet, so after this migration the value is
-- honest but static -- it says "last changed at creation", which is true until
-- something changes the row without saying so. That is the same position
-- tenant_settings was in until cleatctl's read-modify-write started carrying
-- it (cmd/cleatctl/settenantsetting.go), where it is now a concurrency
-- precondition rather than an audit stamp. The contract clause is the column;
-- the writers follow per entity, with the uniform list/get/disable surface.
--
-- Idempotent: ADD COLUMN IF NOT EXISTS, a backfill predicated on IS NULL, and
-- SET NOT NULL / SET DEFAULT, all of which are no-ops on a migrated database.
--

ALTER TABLE admin.tenant_api_keys ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE admin.tenant_api_keys SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE admin.tenant_api_keys ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE admin.tenant_api_keys ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE admin.tenant_roles ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE admin.tenant_roles SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE admin.tenant_roles ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE admin.tenant_roles ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE admin.tenant_egress_allow ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE admin.tenant_egress_allow SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE admin.tenant_egress_allow ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE admin.tenant_egress_allow ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE workflow_defs SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE workflow_defs ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE workflow_defs ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE workflow_schedules ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE workflow_schedules SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE workflow_schedules ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE workflow_schedules ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE workflow_routing ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE workflow_routing SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE workflow_routing ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE workflow_routing ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE workflow_tags ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE workflow_tags SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE workflow_tags ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE workflow_tags ALTER COLUMN updated_at SET DEFAULT now();

ALTER TABLE tenant_domains ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
UPDATE tenant_domains SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE tenant_domains ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE tenant_domains ALTER COLUMN updated_at SET DEFAULT now();


COMMENT ON COLUMN admin.tenant_api_keys.updated_at IS
    'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';

COMMENT ON COLUMN admin.tenant_roles.updated_at IS
    'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';

COMMENT ON COLUMN admin.tenant_egress_allow.updated_at IS
    'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';

COMMENT ON COLUMN workflow_defs.updated_at IS
    'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';

COMMENT ON COLUMN workflow_schedules.updated_at IS
    'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';

COMMENT ON COLUMN workflow_routing.updated_at IS
    'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';

COMMENT ON COLUMN workflow_tags.updated_at IS
    'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';

COMMENT ON COLUMN tenant_domains.updated_at IS
    'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';
