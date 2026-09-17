-- cleat migration 088 (postgres): collection eligibility is not retirement
--
-- cleat#1702. `workflow_defs.deprecated` carried TWO roles, and the contract
-- can only convert it once the roles are separated. This migration splits them,
-- which is what the owner decided on 2026-09-17 in preference to converting the
-- column as it stood.
--
-- THE TWO ROLES, both measured in the tree rather than inferred from the name:
--
--   ADMISSION CONTROL -- a deprecated version cannot be started
--   (cmd/cleat-worker/server.go:840), cannot be routed to (:1484), cannot be
--   pointed at by a tag (:1629), and is not chosen for a child workflow
--   (cmd/cleat/main.go:1722). Four SQL predicates in engine/child_version.go
--   and several `deprecated = 0` selects in the store files enforce it.
--
--   COLLECTION ELIGIBILITY -- engine/version_gc.go, at :111 and :158:
--
--       if !def.Deprecated { continue }
--       ...
--       store.PurgeWorkflowDef(ctx, def.Name, def.Version)
--
--   PurgeWorkflowDef is a PERMANENT DELETE, after --version-gc-max-age (30 days
--   by default), of a definition an in-flight instance may still need to replay.
--
-- WHY THAT BLOCKS THE CONVERSION. Renaming `deprecated` to `disabled_at` would
-- give one word two meanings across two members of one contract: a reversible
-- off-switch on admin.tenant_api_keys (migration 087) and an armed permanent
-- deletion here. #1702 exists because a generic "is this live?" helper is right
-- for some members and backwards for another; a shared spelling with unshared
-- semantics is worse than the four honest spellings it replaces, because the
-- shared name is what invites the generic helper.
--
-- So: `disabled_at` takes admission control, and collection eligibility moves
-- to a column of its own.
--
-- WHY `gc_eligible` AND NOT A TIMESTAMP, and the name is doing work.
--
-- A timestamp would match the contract's shape and would be WRONG here twice
-- over. First, GC measures age from `created_at`, so a `collectable_at` would
-- either be decorative or would silently re-base the 30-day window and change
-- which versions are collected -- and the decision was explicitly that nothing
-- becomes newly collectable. Second, `deprecated` is a BOOLEAN: it records THAT
-- a version was retired and never WHEN, so any timestamp backfilled for it is a
-- bound, and a bound feeding an age comparison is a behaviour change wearing a
-- migration's clothes. A boolean is the exact shape of the role being moved.
--
-- The NAME is chosen to repel a generic writer. `disabled_at` reads like a
-- lifecycle column any entity helper may set, which is precisely the hazard;
-- `gc_eligible` names a mechanism, so nothing reaches for it by analogy with
-- the other nine members. That asymmetry is deliberate.
--
-- THE TWO BACKFILLS ARE NOT BOTH EXACT, and the difference is the whole of what
-- can be claimed here.
--
--   gc_eligible = deprecated        EXACT. Boolean to boolean, same meaning,
--                                   same rows. Nothing becomes newly
--                                   collectable or newly uncollectable.
--
--   disabled_at = now() if deprecated    AN UPPER BOUND, and the only one this
--                                   database can offer.
--
-- Migration 086 faced this and had a better answer: it used `updated_at` as the
-- tightest available upper bound. THAT ARGUMENT DOES NOT TRANSFER, and reusing
-- its phrasing here would have been a claim this migration cannot support.
-- Measured: 085 set `workflow_defs.updated_at = created_at` for every existing
-- row (085:67-69), and NOTHING in the tree maintains the column since --
-- `git grep updated_at -- '*.go' | grep -v _test.go` finds no writer for
-- workflow_defs. So for a version deprecated before 085 the value now sits at
-- `created_at`, which is EARLIER than the deprecation: a lower bound, not an
-- upper one. `now()` is further from the truth than 086's choice and is the
-- only honest direction available, because a disabled version was disabled at
-- some instant at or before this migration runs and nothing recorded which.
--
-- THE EQUALITY OF THE TWO COLUMNS AFTER THIS IS INCIDENTAL, NOT INVARIANT, AND
-- THIS IS THE NOTE THAT MATTERS MOST TO A FUTURE READER. `cleatctl versions
-- deprecate` writes both, and it is the only shipped path that writes either.
-- So in every deployment the two will agree, and a reader will eventually
-- conclude one is redundant. They are not redundant: the entire point is that a
-- write to `disabled_at` -- from an entity helper, from a future
-- /api/definitions/{id}/disable -- must NOT arm a deletion. That difference is
-- unexercised by any shipped writer, so it is pinned by tests instead:
-- engine/gc_eligibility_is_not_retirement_test.go writes each column alone and
-- asserts the other role is unaffected. Do not merge these columns because
-- production data shows them equal.
--
-- Idempotent: the backfills and the DROP are guarded on `deprecated` still
-- existing, since afterwards they would name a column that is gone.

ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS gc_eligible BOOLEAN NOT NULL DEFAULT false;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'workflow_defs' AND column_name = 'deprecated'
    ) THEN
        -- EXACT: boolean to boolean, same rows.
        UPDATE workflow_defs SET gc_eligible = deprecated WHERE gc_eligible <> deprecated;

        -- AN UPPER BOUND: nothing recorded when the version was retired. See
        -- the note above on why 086's updated_at argument does not apply.
        UPDATE workflow_defs SET disabled_at = now()
         WHERE deprecated AND disabled_at IS NULL;

        ALTER TABLE workflow_defs DROP COLUMN deprecated;
    END IF;
END $$;

COMMENT ON COLUMN workflow_defs.disabled_at IS
    'cleat#1702: ADMISSION CONTROL. NULL = live. A disabled version cannot be started, routed to, tagged, or chosen for a child. It does NOT make the version collectable -- that is gc_eligible, deliberately separate, because a generic entity helper writing this column must not arm a permanent deletion. Backfilled from the former `deprecated` boolean at migration 088, where the value is an upper bound: nothing recorded when.';

COMMENT ON COLUMN workflow_defs.gc_eligible IS
    'cleat#1702: COLLECTION ELIGIBILITY, and nothing else. true = version_gc.go may permanently delete this version once it is older than --version-gc-max-age. Named for the mechanism rather than the lifecycle so that nothing reaches for it by analogy with the other entity members. Equality with disabled_at is INCIDENTAL -- cleatctl versions deprecate writes both, and is the only shipped writer of either -- not an invariant, and not a reason to merge them.';
