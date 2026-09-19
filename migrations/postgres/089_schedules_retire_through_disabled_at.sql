-- cleat migration 089 (postgres): schedules retire through disabled_at
--
-- cleat#1702, the LAST of the four conversions and the one held to the end. It
-- was sequenced there on the owner's decision of 2026-09-16 -- "workflow_schedules
-- last because it is the one carrying the API change" -- and the API break was
-- accepted in the same decision. With admin.tenant_api_keys (087) and
-- workflow_defs (088) converted, nothing remains for it to be sequenced behind.
--
-- THIS IS THE ONLY CONVERSION IN THE SET THAT INVERTS POLARITY, and the inversion
-- is the reason `enabled` was the worst of the four legacy spellings:
--
--     revoked_at   set   = retired    ->  disabled_at = revoked_at     (087, exact)
--     deprecated   true  = retired    ->  a bound                      (088)
--     enabled      true  = LIVE       ->  disabled_at IS NULL          (here)
--
-- `revoked_at` and `deprecated` both read "true/set means retired". `enabled`
-- means the opposite, which is what makes a generic "is this live?" helper over
-- the class silently wrong for exactly one member. After this migration the
-- question has one answer on all ten: `disabled_at IS NULL`.
--
-- BOTH DIRECTIONS ARE WRITTEN, AND THAT IS NOT BELT-AND-BRACES. 084 added
-- `disabled_at` to all ten members with NO backfill and said so explicitly:
-- "`enabled` is still the authority until its own conversion migration". So for
-- the whole window between 084 and here the two columns are free to disagree,
-- and this migration resolves the disagreement TOWARD THE DECLARED AUTHORITY --
-- in both directions, because authority is not a direction:
--
--     NOT enabled AND disabled_at IS NULL      ->  set it      (retired, unrecorded)
--     enabled     AND disabled_at IS NOT NULL  ->  clear it    (live, spuriously set)
--
-- The second one matches no row in any shipped deployment: measured on develop,
-- `git grep disabled_at -- '*.go' | grep -i sched` finds nothing, so no code
-- path writes this column for schedules and 084 wrote no value into it. Writing
-- it anyway is cheap and it is what makes the conversion TOTAL: afterwards
-- `disabled_at IS NULL` is exactly the set of rows that had `enabled = true`,
-- with no third case. Omitting it would leave a row that flips from live to
-- retired the instant authority moves -- silently, since the flip is a
-- consequence of the read changing, not of any write.
--
-- THE BACKFILL IS AN UPPER BOUND, and 087's exactness does not transfer. A
-- BOOLEAN records THAT a schedule was disabled and never WHEN, so the best this
-- database can say is "at or before this migration". 086 had a better answer for
-- its two tables -- `updated_at` as the tightest available bound -- and 088
-- already recorded why that argument does not transfer: 085 set `updated_at =
-- created_at` for every existing row and nothing has maintained it since, so for
-- a schedule disabled before 085 the value sits EARLIER than the event and is a
-- lower bound, not an upper one. `now()` is the only honest direction.
--
-- WHY THE FUNCTION IS DROPPED RATHER THAN REPLACED. admin.get_due_schedules()
-- (024) returns `enabled boolean` in its RETURNS TABLE signature, and
-- CREATE OR REPLACE FUNCTION cannot change a return type -- it fails with
-- "cannot change return type of existing function". The whole signature has to
-- go and come back, which also means re-applying the OWNER, the REVOKE and the
-- GRANT, since a dropped function takes its ACL with it. All four are restated
-- below rather than assumed to survive.
--
-- WHY 001 STILL DECLARES `enabled`, WHICH THE OTHER TWO CONVERSIONS REMOVED.
--
-- 087 and 088 deleted their legacy column from 001_schema.sql, per that file's
-- "final column set" header. This one does not, and both alternatives were
-- tried and rejected by tests rather than by preference.
--
-- FIRST ATTEMPT -- drop `enabled` from 001. That breaks a FRESH bootstrap at
-- migration 024, four files before this one: 024 creates
-- admin.get_due_schedules() with a LANGUAGE sql body naming `s.enabled`, and
-- PostgreSQL validates SQL-language bodies AT CREATE TIME. Measured here, with
-- both controls:
--
--     LANGUAGE sql,     missing column  ->  ERROR: column ... does not exist
--     LANGUAGE sql,     real column     ->  CREATE FUNCTION
--     LANGUAGE plpgsql, missing column  ->  CREATE FUNCTION  (no validation)
--
-- SECOND ATTEMPT -- edit 024's body to the new column, so 001 could drop it.
-- That made 024's definition BYTE-IDENTICAL to this file's, and
-- engine/routine_definition_drift_test.go refused it:
--
--     admin.get_due_schedules is defined in 2 migrations (latest 089 ...) and
--     the latest introduces neither a new identifier nor a new body line, and
--     removes none, so nothing here can tell the versions apart.
--     That is a hole in this test, not a property of the schema: it means a
--     stale database would pass.
--
-- It is right. With two identical definitions in the tree there is no way to
-- distinguish a database that ran this migration from one stuck at 024, and
-- that distinction is the entire job of the drift test.
--
-- SO: 024 is untouched, and 001 declares BOTH columns. That is not a
-- concession -- between 084 and this file an existing database legitimately
-- carries both, and declaring both makes a fresh database traverse the same
-- window rather than a shortcut, so the guards and the backfill below are one
-- code path on every database. What the header's invariant actually protects is
-- re-applying 001 to an already-migrated database
-- (TestShippedSchema_IsIdempotent): CREATE TABLE IF NOT EXISTS is a no-op on
-- the second pass, and the part that genuinely breaks on a dropped column -- an
-- INDEX naming it -- is final in 001 already, on `disabled_at`.
--
-- One consequence worth stating plainly: on a fresh database the backfill below
-- runs against a table that never had a non-NULL `enabled = false` row, so it
-- changes nothing there. The backfill exists ONLY on the upgrade path, and was
-- therefore measured on one -- see the table in migrations/mssql/081.
--
-- IDEMPOTENT. Every statement that names `enabled` sits inside the guard on
-- `enabled` still existing, because on a fresh database 001 never created it.

-- ── Backfill, in both directions, while `enabled` is still the authority ─────

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
-- Restored from a recorded list rather than recomputed, because after the
-- toggle the tables no longer answer "were you forced?". ON COMMIT DROP plus
-- the runner's per-migration transaction means a failure anywhere below rolls
-- the exemption back with everything else -- applyMigration wraps each file in
-- its own transaction, and DDL is transactional here.
DROP TABLE IF EXISTS cleat_forced_089;
CREATE TEMP TABLE cleat_forced_089 ON COMMIT DROP AS
SELECT c.oid::regclass AS rel
  FROM pg_class c
 WHERE c.relforcerowsecurity
   AND c.oid = ANY (ARRAY['workflow_schedules']::regclass[]);

DO $forced$
DECLARE r regclass;
BEGIN
    FOR r IN SELECT rel FROM cleat_forced_089 LOOP
        EXECUTE format('ALTER TABLE %s NO FORCE ROW LEVEL SECURITY', r);
    END LOOP;
END $forced$;

DO $$
BEGIN
    IF EXISTS (
        -- table_schema = current_schema(), because information_schema lists
        -- every schema and cleat supports schema-per-pool: without it this
        -- guard can be satisfied by ANOTHER pool's table while the ALTER below
        -- resolves through search_path to this one. 087 and 088 use the
        -- unqualified form; it has not bitten them because each pool converts
        -- while its own column is still present, but it is the same latent
        -- bug and this file does not copy it.
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'workflow_schedules'
          AND column_name = 'enabled'
    ) THEN
        -- Retired under the authority, unrecorded here. AN UPPER BOUND.
        UPDATE workflow_schedules SET disabled_at = now()
         WHERE NOT enabled AND disabled_at IS NULL;

        -- Live under the authority, so `disabled_at` must not say otherwise.
        -- Matches nothing in a shipped deployment; see the header.
        UPDATE workflow_schedules SET disabled_at = NULL
         WHERE enabled AND disabled_at IS NOT NULL;
    END IF;
END $$;
-- Owner exemption withdrawn again, on exactly the tables it was granted on.
DO $forced$
DECLARE r regclass;
BEGIN
    FOR r IN SELECT rel FROM cleat_forced_089 LOOP
        EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', r);
    END LOOP;
END $forced$;

-- ── The due-schedule index moves to the new column ──────────────────────────
--
-- Partial rather than three-column: the scan is
-- `WHERE disabled_at IS NULL AND next_run_at <= now()`, so the retired rows do
-- not belong in the index at all. 087 made the same choice for
-- idx_api_keys_hash. MySQL has no partial indexes and keeps the column in the
-- key instead; that asymmetry is per-dialect and deliberate.

DROP INDEX IF EXISTS idx_schedules_tenant_enabled;
CREATE INDEX IF NOT EXISTS idx_schedules_tenant_due
    ON workflow_schedules(tenant_id, next_run_at) WHERE disabled_at IS NULL;

-- ── admin.get_due_schedules(): new signature, so drop and recreate ──────────

DROP FUNCTION IF EXISTS admin.get_due_schedules();

-- The column list is the contract with engine's scanDueSchedules. It matches
-- the tenant-scoped GetDueSchedules on every dialect, in order, so both feed
-- the same Go scan and cannot drift apart unnoticed.
CREATE FUNCTION admin.get_due_schedules()
RETURNS TABLE (
    name            text,
    def_name        text,
    entry_point     text,
    cron_expression text,
    input           jsonb,
    disabled_at     timestamptz,
    next_run_at     timestamptz,
    last_run_at     timestamptz,
    timezone        text,
    tenant_id       uuid,
    misfire_policy  text,
    catch_up_limit  integer,
    overlap_policy  text,
    last_run_id     text
)
LANGUAGE sql
SECURITY DEFINER
-- Pinned so the body cannot be redirected by a caller's search_path. Standard
-- hardening for SECURITY DEFINER, and not optional when the function holds an
-- RLS exemption.
SET search_path FROM CURRENT
AS $$
    SELECT s.name, s.def_name, s.entry_point, s.cron_expression, s.input,
           s.disabled_at, s.next_run_at, s.last_run_at, s.timezone, s.tenant_id,
           s.misfire_policy, s.catch_up_limit, s.overlap_policy,
           COALESCE(s.last_run_id, '')
    FROM workflow_schedules s
    WHERE s.disabled_at IS NULL
      AND s.next_run_at <= now()
    ORDER BY s.next_run_at;
$$;

-- A DROP took the ownership and the ACL with it, so both are restated. Losing
-- either is not a visible error: the wrong owner silently loses the BYPASSRLS
-- the cross-tenant read depends on, and a missing REVOKE hands the exemption
-- to PUBLIC.
-- Conditional for the reason 023 gives: on managed PostgreSQL the role cannot
-- be created, and this statement aborting the run was how a degraded 023 took
-- later files down with it.
DO $do$ BEGIN
    -- ATTEMPTED, NOT GUARDED ON THE ROLE'S EXISTENCE. "Does cleat_dispatcher
    -- exist" is the wrong question: ALTER ... OWNER TO also requires the
    -- current role to be a MEMBER of the target, so a role that exists but
    -- was created by somebody else still fails --
    --
    --   ERROR:  must be able to SET ROLE "cleat_dispatcher"   (SQLSTATE 42501)
    --
    -- which is what re-applying this file as a non-superuser hit, against a
    -- cluster where a superuser had created the role earlier. Asking the
    -- database to do it and catching the refusal answers both questions at
    -- once, and needs no version-specific reasoning about pg_has_role and
    -- PostgreSQL 16's WITH SET.
    BEGIN
        EXECUTE 'ALTER FUNCTION admin.get_due_schedules() OWNER TO cleat_dispatcher';
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE NOTICE 'cannot give the function to cleat_dispatcher (SQLSTATE %); it keeps the migrating role as its owner and will not see across tenants. Use --claim-strategy=rotate, which needs no exemption.', SQLSTATE;
    END;
END $do$;

-- REVOKE first: PostgreSQL grants EXECUTE to PUBLIC on new functions by
-- default, which would hand the exemption to every role in the database.
REVOKE ALL ON FUNCTION admin.get_due_schedules() FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        GRANT EXECUTE ON FUNCTION admin.get_due_schedules() TO cleat_app;
    END IF;
END $$;

-- ── And the column goes ──────────────────────────────────────────────────────

DO $$
BEGIN
    IF EXISTS (
        -- table_schema = current_schema(), because information_schema lists
        -- every schema and cleat supports schema-per-pool: without it this
        -- guard can be satisfied by ANOTHER pool's table while the ALTER below
        -- resolves through search_path to this one. 087 and 088 use the
        -- unqualified form; it has not bitten them because each pool converts
        -- while its own column is still present, but it is the same latent
        -- bug and this file does not copy it.
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'workflow_schedules'
          AND column_name = 'enabled'
    ) THEN
        ALTER TABLE workflow_schedules DROP COLUMN enabled;
    END IF;
END $$;

COMMENT ON COLUMN workflow_schedules.disabled_at IS
    'cleat#1702: when this schedule was retired. NULL = live, and a live schedule is the only kind admin.get_due_schedules() and GetDueSchedules return. Replaced the `enabled` BOOLEAN at migration 089, the one conversion in the class that inverted polarity -- enabled=true meant LIVE. Backfilled values are an UPPER BOUND: a boolean recorded that a schedule was disabled and never when.';
