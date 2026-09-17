-- cleat migration 076 (mysql): collection eligibility is not retirement
--
-- cleat#1702, the MySQL half of postgres/088. See that file for the two roles
-- `workflow_defs.deprecated` carried, for why the column had to be SPLIT rather
-- than renamed, for why the eligibility column is a boolean named for its
-- mechanism, and -- most importantly for anyone reading these columns later --
-- for why their equality is INCIDENTAL, NOT INVARIANT.
--
-- THE TWO BACKFILLS DIFFER IN WHAT THEY CAN CLAIM, identically to the postgres
-- half: `gc_eligible = deprecated` is EXACT, boolean to boolean; `disabled_at`
-- for a retired version is an UPPER BOUND, because a boolean records that a
-- version was retired and never when. Migration 086's "tightest available upper
-- bound" argument does not transfer -- 074 set `workflow_defs.updated_at =
-- created_at` and nothing maintains it, so that value is a LOWER bound here.
--
-- DYNAMIC SQL, as in 075. MySQL has no ADD COLUMN IF NOT EXISTS, and the
-- backfills name `deprecated`, so on a second run the server would fail PARSING
-- statements whose branch should never execute. Building each as text and
-- preparing it only when the column is present defers that.
--
-- NOW(6) rather than NOW(): every timestamp column in this dialect carries
-- microsecond precision, and a NOW() here would silently truncate the bound to
-- the second. Matching the column, not the shortest spelling.

SET @add_gc_eligible := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_defs ADD COLUMN gc_eligible TINYINT(1) NOT NULL DEFAULT 0',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_defs'
      AND COLUMN_NAME = 'gc_eligible'
);
PREPARE add_gc_eligible FROM @add_gc_eligible; EXECUTE add_gc_eligible; DEALLOCATE PREPARE add_gc_eligible;

-- EXACT: boolean to boolean, same rows.
SET @backfill_gc_eligible := (
    SELECT IF(COUNT(*) = 1,
        'UPDATE workflow_defs SET gc_eligible = deprecated WHERE gc_eligible <> deprecated',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_defs'
      AND COLUMN_NAME = 'deprecated'
);
PREPARE backfill_gc_eligible FROM @backfill_gc_eligible; EXECUTE backfill_gc_eligible; DEALLOCATE PREPARE backfill_gc_eligible;

-- AN UPPER BOUND: nothing recorded when the version was retired.
SET @backfill_disabled_at := (
    SELECT IF(COUNT(*) = 1,
        'UPDATE workflow_defs SET disabled_at = NOW(6) WHERE deprecated AND disabled_at IS NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_defs'
      AND COLUMN_NAME = 'deprecated'
);
PREPARE backfill_disabled_at FROM @backfill_disabled_at; EXECUTE backfill_disabled_at; DEALLOCATE PREPARE backfill_disabled_at;

SET @drop_deprecated := (
    SELECT IF(COUNT(*) = 1,
        'ALTER TABLE workflow_defs DROP COLUMN deprecated',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_defs'
      AND COLUMN_NAME = 'deprecated'
);
PREPARE drop_deprecated FROM @drop_deprecated; EXECUTE drop_deprecated; DEALLOCATE PREPARE drop_deprecated;
