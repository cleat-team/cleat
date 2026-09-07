-- cleat migration 045 (postgres): a continuation records the run it came from
--
-- cleat#826. ContinueAsNew inserts a NEW instance row with a fresh
-- gen_random_uuid() and marks the old one 'done', and nothing connected the
-- two. A caller holding the id it started saw that workflow complete with an
-- empty result and had no way to name the run carrying the real one.
--
-- Direction: the SUCCESSOR records its PREDECESSOR. continued_from on the new
-- row holds the id of the run that continued into it, so the chain is a linked
-- list walkable in either direction -- backwards by reading the column,
-- forwards by the index below.
--
-- Why a new column rather than parent_workflow_id, which already exists and is
-- already nullable. A continuation is not a child, and every consumer of that
-- column would have treated it as one:
--
--   * GetChildCount (engine/store_children.go) counts non-terminal rows with
--     parent_workflow_id = $1. A continuation would have counted as a live
--     child of the run it replaced.
--   * enforceParentClosePolicy sweeps a workflow's children AT THE MOMENT IT
--     COMPLETES -- which is exactly when ContinueAsNew completes the
--     predecessor; it calls that function directly. The column defaults to
--     'ABANDON' so the naive form survives, but #826 proposed reusing the
--     column "with a distinct close policy", and anything other than ABANDON
--     terminates the continuation the instant its predecessor finishes.
--   * TERMINATE would have been the worst available choice: that arm records
--     no completed_at (fixed in #864), and every retention sweep gates on
--     completed_at IS NOT NULL, so each killed continuation would also have
--     become permanently uncollectable.
--
-- Nothing is backfilled. Rows written before this migration keep a NULL
-- continued_from and their chains stay unwalkable; that population is finite
-- and stops growing here. cleat#867 covers what to do about it, deliberately
-- separately, because inventing a link for a chain nobody recorded is a
-- different decision from recording the ones we do have.

ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS continued_from TEXT;

-- The forward walk -- "which run did THIS one continue into?" -- is the new
-- access pattern, and it is the one a caller polling its original id needs.
-- Partial: the overwhelming majority of rows are not continuations and should
-- not be in this index at all.
CREATE INDEX IF NOT EXISTS idx_workflow_instances_continued_from
    ON workflow_instances (continued_from)
    WHERE continued_from IS NOT NULL;
