-- cleat migration 087 (postgres): one reversible off-switch for API keys
--
-- cleat#1702. `admin.tenant_api_keys.revoked_at` becomes `disabled_at`, the
-- single retirement spelling the contract requires. This is the first of the
-- three `no-legacy-retirement` conversions.
--
-- Migration 084 already ADDED `disabled_at` to all ten members and said what
-- this migration is for, in the column's own comment:
--
--     admin.tenant_api_keys.revoked_at is still the authority until its own
--     conversion migration
--
-- This is that migration, so it backfills and removes rather than adding, and
-- it rewrites that comment -- a COMMENT naming `revoked_at` as the authority
-- survives the column being dropped, and would then be a confident, wrong
-- answer sitting in \d+ output for the next reader.
--
-- THIS ONE IS A RENAME, AND THAT IS A MEASURED CLAIM RATHER THAN THE OBVIOUS
-- READING. "Revoked" suggests something permanent and security-final, and
-- "disabled" suggests something you can undo, so the natural first conclusion
-- is that collapsing one into the other loses a distinction. In cleat it does
-- not, because revocation here is already documented as reversible --
-- cmd/cleatctl/revokeapikey.go, on why its guard rails are lighter than
-- drop-tenant's:
--
--     This command sets a revoked_at timestamp. It is reversible by an
--     operator with the same access (UPDATE ... SET revoked_at = NULL)
--
-- Same type, same polarity, same reversibility, NULL = live on both sides. So
-- the backfill is `disabled_at = revoked_at` and it is EXACT -- not the upper
-- bound 086 had to settle for, and not 085's exactness borrowed by analogy
-- either. The column already holds the instant, so there is nothing to infer.
--
-- Stated plainly because the sibling `no-legacy-retirement` row does NOT have
-- this property: `workflow_defs.deprecated` is a BOOLEAN, records that a
-- version was retired and never when, and any timestamp for it would be a
-- bound. It is not converted here, and not only for that reason -- see the
-- note at the end of this file.
--
-- NO SECURITY-POLICY TOGGLE, AND UNLIKE 086 THAT IS NOT BECAUSE POSTGRES
-- SUPERUSERS BYPASS RLS. This table has no row-level security at all, on
-- either engine, and the reason is structural rather than an omission --
-- migration 061:
--
--     admin.tenant_api_keys is read by the authenticator before a tenant is
--     known
--
-- A predicate keyed on the current tenant cannot be evaluated by the query
-- that DISCOVERS the tenant. So there is no policy to disable here and the
-- mssql half needs no BEGIN TRY/CATCH either, which is where it stops
-- resembling 078. Checked against the migrations for both engines rather than
-- carried over from the neighbouring file.
--
-- THE INDEX IS PART OF THE MIGRATION, NOT A CONSEQUENCE OF IT.
-- `idx_api_keys_hash` is PARTIAL -- `WHERE revoked_at IS NULL` -- so it is the
-- index every authentication lookup uses, and its predicate names the column
-- being dropped. Postgres would drop the index silently along with the column;
-- doing it explicitly and recreating it on `disabled_at IS NULL` keeps the
-- lookup covered rather than leaving a seq scan on the auth path. MySQL's
-- equivalent index is unfiltered (it has no partial indexes), so its half of
-- this migration touches no index at all -- the same intent, and the difference
-- is in the engine rather than in the plan.
--
-- Idempotent. The backfill, the index drop and the column drop are all inside a
-- single guard on `revoked_at` still existing, because after the first run the
-- UPDATE would reference a column that is gone. Re-running reaches only the
-- CREATE INDEX IF NOT EXISTS, which is a no-op.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'admin'
          AND table_name = 'tenant_api_keys'
          AND column_name = 'revoked_at'
    ) THEN
        -- Exact, not a bound: revoked_at already holds the instant.
        UPDATE admin.tenant_api_keys
           SET disabled_at = revoked_at
         WHERE revoked_at IS NOT NULL
           AND disabled_at IS NULL;

        -- Before the column, because the index predicate names it.
        DROP INDEX IF EXISTS admin.idx_api_keys_hash;

        ALTER TABLE admin.tenant_api_keys DROP COLUMN revoked_at;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_api_keys_hash
    ON admin.tenant_api_keys(key_hash) WHERE disabled_at IS NULL;

-- WHY workflow_defs IS NOT IN THIS MIGRATION, since #1702 lists the two
-- together and the next reader will wonder.
--
-- `workflow_defs.deprecated` is not a retirement flag that happens to be a
-- boolean. It is the GC eligibility gate -- engine/version_gc.go:
--
--     if !def.Deprecated { continue }
--     ...
--     store.PurgeWorkflowDef(ctx, def.Name, def.Version)
--
-- and PurgeWorkflowDef is a permanent delete, after --version-gc-max-age
-- (30 days by default), of a definition an in-flight instance may still need to
-- replay. Renaming it to `disabled_at` would give one word two meanings across
-- two members of one contract: a reversible off-switch on API keys, and an
-- armed permanent deletion on definitions.
--
-- That is the defect #1702 exists to remove -- a generic "is this live?" helper
-- being right for some members and wrong for another -- so converting this one
-- blind would recreate it under a shared name, which is worse than the four
-- honest spellings it replaces. It needs a decision about whether GC keys on
-- something else, and that is the owner's, not this migration's.

-- 084's comment on this column named revoked_at as the authority. It is not,
-- as of this migration, and a stale COMMENT outlives the column it cites.
COMMENT ON COLUMN admin.tenant_api_keys.disabled_at IS
    'cleat#1702: when this API key was retired. NULL = live. The single retirement spelling; revoked_at was dropped by migration 087 and this column is now the authority. Reversible: setting it back to NULL restores the key, which is what revoked_at always meant here.';
