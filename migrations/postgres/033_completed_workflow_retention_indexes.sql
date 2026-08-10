-- cleat migration 033 (postgres): indexes for completed-workflow retention
-- and the ListWorkflows admin/dashboard query path.
--
-- Finding S2 (see the sibling stream that added DeleteCompletedWorkflows and
-- --completed-workflow-retention-days) also called out that ListWorkflows
-- (engine/db.go) builds ILIKE filters against input/result/error_msg/def_name
-- with no supporting index -- the one GIN index on `input`
-- (idx_instances_input_gin, 001_schema.sql) supports JSONB containment (@>),
-- not LIKE. Dashboard list latency degrades with lifetime workflow count as
-- a result. This migration adds what the real query shapes actually need,
-- and says explicitly what it does NOT add and why.
--
-- The real query shapes, checked against engine/db.go's ListWorkflows and
-- engine/query_builder.go rather than assumed:
--
--   SELECT ... FROM workflow_instances WHERE 1=1
--     [AND status = $1]                                  -- Status filter
--     [AND input::text ILIKE $n]                          -- InputContains
--     [AND error_msg ILIKE $n]                             -- ErrorContains
--     [AND (input::text ILIKE $n OR result::text ILIKE $n+1
--           OR error_msg ILIKE $n+2 OR def_name ILIKE $n+3)]  -- Search
--   ORDER BY created_at DESC
--   LIMIT/OFFSET
--
-- All three dialects filter by tenant first -- PostgreSQL via the
-- tenant_isolation_instances RLS policy (`tenant_id = cleat.assert_tenant_set()`),
-- MySQL/MSSQL via an explicit `WHERE tenant_id = ?` -- so an index leading
-- with tenant_id is usable on every dialect even though only PostgreSQL's
-- copy of this predicate is implicit.
--
-- ===========================================================================
-- 1. Widen the terminal-status partial index to also serve retention
-- ===========================================================================
-- idx_instances_terminal_completed (001_schema.sql) is
-- `(tenant_id, status, completed_at) WHERE status IN ('done','failed')`,
-- built for DeleteExpiredEvents' subquery. DeleteCompletedWorkflows (the
-- sibling stream) queries the same shape but for
-- `status IN ('done', 'failed', 'terminated')` -- 'terminated' is a real,
-- checked terminal status (TerminateWorkflow sets completed_at and never
-- reactivates the row) that nothing before this stream ever purged. A
-- partial index only serves a query whose WHERE clause is provably a subset
-- of the index's own predicate, so the existing 2-status index cannot serve
-- a 3-status query at all -- Postgres falls back to a full scan the moment
-- 'terminated' is added to the IN-list, which would make
-- DeleteCompletedWorkflows' batch-selection query scan the whole table on
-- every tick. Replacing it with a 3-status predicate keeps it usable by
-- DeleteExpiredEvents' narrower 2-status query too (every row matching the
-- narrower predicate also matches the wider one), so this is a strict
-- widening, not two indexes doing overlapping work.
DROP INDEX IF EXISTS idx_instances_terminal_completed;
CREATE INDEX IF NOT EXISTS idx_instances_terminal_completed
    ON workflow_instances(tenant_id, status, completed_at)
    WHERE status IN ('done', 'failed', 'terminated');

-- ===========================================================================
-- 2. tenant + status + created_at, for ListWorkflows' Status filter
-- ===========================================================================
-- ListWorkflows always orders by created_at DESC regardless of which filters
-- are set, and completed_at (index 1 above) is not created_at -- a status
-- filter using that index still needs a separate sort step and cannot use
-- the index to satisfy LIMIT via early termination. This index lets
-- `WHERE tenant_id = ? AND status = ?  ORDER BY created_at DESC LIMIT ?` --
-- the shape a "show me failed workflows" dashboard filter produces -- run as
-- a single ordered index scan that stops at LIMIT, independent of how many
-- terminal workflows exist in total. Deliberately not partial: unlike index
-- 1, this needs to serve every status value ListWorkflows can be asked to
-- filter on ('ready', 'running', 'dead_lettered', ...), not only the
-- terminal ones.
CREATE INDEX IF NOT EXISTS idx_instances_tenant_status_created
    ON workflow_instances(tenant_id, status, created_at DESC);

-- ===========================================================================
-- 3. Trigram index on error_msg, for the ErrorContains filter
-- ===========================================================================
-- error_msg is a plain TEXT column (not JSONB), bounded in practice to one
-- error message, and only ever set on a minority of rows (failed workflows).
-- ILIKE '%text%' has a leading wildcard, so no plain B-tree can support it;
-- pg_trgm's GIN opclass can. Write cost: one GIN update per workflow that
-- transitions into a status with a non-null error_msg (FailWorkflow,
-- MoveToDeadLetterQueue) -- a small fraction of total write volume, since
-- most workflows complete via CompleteWorkflow with error_msg staying NULL,
-- and the partial WHERE clause below means rows that stay NULL are never
-- indexed at all. Judged worth it: this is the one column in the filter set
-- where indexing is both cheap and actually usable standalone (the
-- dedicated ErrorContains filter queries this column alone, not ORed against
-- input/result -- see the note on Search below for why the same index does
-- NOT help there).
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX IF NOT EXISTS idx_instances_error_msg_trgm
    ON workflow_instances USING GIN (error_msg gin_trgm_ops)
    WHERE error_msg IS NOT NULL;

-- ===========================================================================
-- 4. What this migration deliberately does NOT index, and why
-- ===========================================================================
-- InputContains and the general Search filter's input/result branches:
-- `input` and `result` are JSONB, and the query casts them to text
-- (`input::text ILIKE $n`) before matching -- a substring match over the
-- serialized form of an arbitrary user payload. A trigram GIN index on
-- `input::text` is technically possible (pg_trgm supports expression
-- indexes) but was rejected: it would index the full serialized text of
-- every workflow's input, sized by payload rather than by anything bounded,
-- write-amplifying every single workflow start and (for `result`) every
-- single completion -- the majority of this table's write volume, unlike
-- error_msg's minority. The existing idx_instances_input_gin
-- (jsonb_path_ops) already answers the query this data shape is actually
-- suited to -- "does this JSON document contain this key/value" via `@>` --
-- and that is the narrower query operators should be pointed at instead of
-- a free-text ILIKE scan over serialized JSON.
--
-- The general Search filter overall: even granting error_msg and def_name
-- their own indexes, Search ORs all four conditions together in one WHERE
-- clause. PostgreSQL can only satisfy an OR of multiple conditions via
-- BitmapOr-combined index scans if EVERY disjunct is index-satisfiable --
-- one un-indexable disjunct (input::text ILIKE, per the paragraph above)
-- forces a sequential scan for the entire OR, regardless of how well the
-- other three branches are indexed. Concretely: no index this migration
-- could add makes the Search filter fast while it still ORs against
-- input/result. That is a mechanism problem, not a missing-index problem --
-- the fix is to narrow the query, e.g. splitting Search into separately
-- indexed sub-queries unioned in application code, or dropping input/result
-- from the general search box in favour of the already-separate
-- InputContains filter and JSONB containment. Neither is done here; this
-- migration adds only what makes the already-separate, already-indexable
-- filters (Status, ErrorContains) fast, and documents the rest as a known,
-- unindexed gap rather than a false claim of having fixed it.
