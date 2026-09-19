-- cleat migration 086 (postgres): two entities record when they were created
--
-- cleat#1702. `created_at TIMESTAMPTZ` on the two members that lack it,
-- tenant_settings and tenant_secrets. The other eight have carried it since
-- they were created; this is the last of the three timestamp clauses.
--
-- THESE TWO ARE THE INVERSE OF THE USUAL CASE, which is why they were left to
-- the end: they have `updated_at` and no `created_at`, where every other member
-- had `created_at` and no `updated_at`. Migration 085 converted the eight by
-- backfilling `updated_at` FROM `created_at`; this one goes the other way, and
-- the argument does NOT mirror.
--
-- 085 could claim its backfill was exact: a row not updated since it was
-- created was last modified when it was created, so `created_at` is the true
-- value of `updated_at`. The reverse is only a BOUND. A row that has been
-- updated was created strictly before its `updated_at`, and nothing in these
-- tables records when. So:
--
--     for a row never updated   updated_at IS created_at, exactly -- the
--                               DEFAULT now() stamped it at insert
--     for a row since updated   created_at < updated_at, and the true value
--                               is not recoverable from this database
--
-- `updated_at` is therefore the TIGHTEST AVAILABLE UPPER BOUND, and that is the
-- honest description of it. It is not a guess dressed as a fact, and it is
-- strictly better than the obvious alternative: `now()` is also an upper bound,
-- is further from the truth for every row without exception, and is false even
-- for the rows where `updated_at` is exactly right.
--
-- These two tables carry exactly one timestamp each -- `updated_at TIMESTAMPTZ
-- NOT NULL DEFAULT now()`, from migrations 039 and 081 -- so there is no third
-- source to prefer. Read from the files and confirmed against a built database
-- rather than assumed.
--
-- THREE STEPS RATHER THAN ONE, for the reason 085 gives and which applies
-- identically here. A one-step `ADD COLUMN created_at TIMESTAMPTZ NOT NULL
-- DEFAULT now()` stamps every existing row with the instant the migration ran
-- and leaves the backfill nothing to match -- silently, with the wrong value
-- already in place. The column is added WITHOUT a default so existing rows get
-- NULL and the UPDATE can see them; only then does it become NOT NULL with a
-- default for future inserts.
--
-- NO SECURITY-POLICY TOGGLE HERE, AND THAT IS NOT AN OVERSIGHT. Both tables
-- have RLS enabled AND forced (039:109-110, 081:83-84). FORCE removes the TABLE
-- OWNER's bypass; it does not touch the superuser's, which 001_schema.sql:614
-- states cannot be forced. Migrations are applied by a superuser in every
-- configuration cleat ships (005_app_role.sql), so the rows are visible to the
-- UPDATE below. mssql/078 has to disable two policies around the same backfill
-- because SQL Server's filter predicates exempt nobody, not even sysadmin --
-- two engines, one migration, and the difference is not in the SQL.
--
-- WHAT THIS DOES NOT DO: maintain the column. Nothing needs to -- `created_at`
-- is written once by its own DEFAULT and never updated, which is the one
-- timestamp clause that needs no writer. That is the opposite of 085's position
-- on `updated_at`, where the value is honest but static until a writer carries
-- it.
--
-- Idempotent: ADD COLUMN IF NOT EXISTS, a backfill predicated on IS NULL, and
-- SET NOT NULL / SET DEFAULT, all of which are no-ops on a migrated database.

-- tenant_settings ------------------------------------------------------------

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
    FOREACH r IN ARRAY ARRAY['tenant_settings', 'tenant_secrets']::regclass[] LOOP
        IF (SELECT relforcerowsecurity FROM pg_class WHERE oid = r) THEN
            EXECUTE format('ALTER TABLE %s NO FORCE ROW LEVEL SECURITY', r);
        END IF;
    END LOOP;
END $forced$;

ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ;

UPDATE tenant_settings SET created_at = updated_at WHERE created_at IS NULL;

ALTER TABLE tenant_settings ALTER COLUMN created_at SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN created_at SET DEFAULT now();

-- tenant_secrets -------------------------------------------------------------

ALTER TABLE tenant_secrets ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ;

UPDATE tenant_secrets SET created_at = updated_at WHERE created_at IS NULL;

ALTER TABLE tenant_secrets ALTER COLUMN created_at SET NOT NULL;
ALTER TABLE tenant_secrets ALTER COLUMN created_at SET DEFAULT now();
-- Owner exemption withdrawn again, on exactly the tables it was granted on.
DO $forced$
DECLARE r regclass;
BEGIN
    FOREACH r IN ARRAY ARRAY['tenant_settings', 'tenant_secrets']::regclass[] LOOP
        IF (SELECT relrowsecurity FROM pg_class WHERE oid = r) THEN
            EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', r);
        END IF;
    END LOOP;
END $forced$;
