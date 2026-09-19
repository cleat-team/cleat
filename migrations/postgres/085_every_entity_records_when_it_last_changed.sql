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


-- ── The backfill needs the owner's exemption back for the length of it ──────
--
-- WHY THIS IS HERE AT ALL. The comment above says the rows are visible
-- because "migrations are applied by a superuser in every configuration cleat
-- ships". That was true when it was written and is no longer the configuration
-- cleat supports: managed PostgreSQL -- RDS, Cloud SQL, Azure -- has no
-- superuser at all, so the migrating role is subject to these policies like
-- any other, and the UPDATEs below do not return fewer rows, they RAISE:
--
--   ERROR:  cleat.tenant_id is not set -- tenant context required for
--           RLS-scoped query
--
-- because 001_schema.sql's policies are fail-closed through
-- cleat.assert_tenant_set() rather than COALESCE-ing to a default. The
-- statement is refused before it matches anything, so this fails even on a
-- fresh database where the backfill has nothing to do.
--
-- NO FORCE, NOT DISABLE. mssql/077 turns five security policies OFF around the
-- same backfill because SQL Server has no owner exemption to restore. Here
-- there is one: FORCE is what subjects the TABLE OWNER to its own policies
-- (001_schema.sql:614), so dropping FORCE restores the owner's exemption and
-- nothing else. Every other role stays constrained for the whole window, which
-- DISABLE would not give.
--
-- THE LIST IS A LITERAL IN BOTH BLOCKS, not state carried between them.
-- The first version recorded the forced tables in a TEMP TABLE ... ON COMMIT
-- DROP, which works under the Go runner -- applyMigration wraps each file in a
-- transaction -- and is destroyed instantly under the OTHER applier:
-- deploy/postgres/100-apply-migrations.sh runs `psql -f` with no -1, so every
-- statement autocommits and the temp table is dropped by its own CREATE. The
-- cluster deployment caught that; a local harness that had been made to match
-- the runner did not, because it only ever modelled one of the two appliers.
--
-- Restoring is guarded on RLS being ENABLED rather than on what was recorded:
-- FORCE is meaningless without it, and every table here has had both since
-- 001_schema.sql.
DO $forced$
DECLARE r regclass;
BEGIN
    FOREACH r IN ARRAY ARRAY['admin.tenant_api_keys', 'admin.tenant_roles', 'admin.tenant_egress_allow', 'workflow_defs', 'workflow_schedules', 'workflow_routing', 'workflow_tags', 'tenant_domains']::regclass[] LOOP
        IF (SELECT relforcerowsecurity FROM pg_class WHERE oid = r) THEN
            EXECUTE format('ALTER TABLE %s NO FORCE ROW LEVEL SECURITY', r);
        END IF;
    END LOOP;
END $forced$;

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

-- Owner exemption withdrawn again, on exactly the tables it was granted on.
DO $forced$
DECLARE r regclass;
BEGIN
    FOREACH r IN ARRAY ARRAY['admin.tenant_api_keys', 'admin.tenant_roles', 'admin.tenant_egress_allow', 'workflow_defs', 'workflow_schedules', 'workflow_routing', 'workflow_tags', 'tenant_domains']::regclass[] LOOP
        IF (SELECT relrowsecurity FROM pg_class WHERE oid = r) THEN
            EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', r);
        END IF;
    END LOOP;
END $forced$;



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
