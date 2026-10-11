-- cleat#3245 Phase 3 step 3, piece B's blocker: ClaimWorkflows/
-- ClaimStickyWorkflows/CountRunnableWorkflows (engine/store_lifecycle.go)
-- read concurrency_key/concurrency_key_hash from workflow_instances as part
-- of their claim-candidate predicate, and workflow_leases (migration 016)
-- never carried them -- 016 only duplicated the three columns (task_queue,
-- priority, created_at) that a measured 6-17x join loss in that same
-- predicate justified. Blocked piece B's FROM-clause cutover until this
-- landed; see cleat#3245's tracking issue for the full survey, including
-- the three options weighed and the owner's choice (add the columns here,
-- scoped to exactly what step 3's remaining pieces B/C/D/E/F read).
--
-- That survey, run across every remaining `FROM workflow_instances` read
-- site outside pieces A/B, found two more genuine gaps and ruled out a
-- third:
--
--   - piece E (engine/store_children.go's ChildWorkflowCount and
--     OriginalChildRunIDs) reads parent_workflow_id and continued_from --
--     both ADDED to workflow_leases below, no index: these are cold admin
--     counts, not the hot claim path concurrency_key serves, so an index
--     is piece E's own call to make if its own cutover needs one.
--   - found by a blanket grep rather than by name, outside the piece
--     breakdown the survey was asked to check: engine/run_limits.go's
--     GetRunLimits reads four run-config override columns, written in the
--     SAME StartNewRun INSERT as concurrency_key -- ADDED below for the
--     same reason.
--   - piece D's apparent allowed_signals gap (engine/store_signals.go's
--     GetAllowedSignalCallers) is NOT a workflow_leases gap at all:
--     allowed_signals is a workflow_payloads column already (migration
--     016), already mirrored there by SetAllowedSignalCallers since step
--     2 piece 5a. Not touched here.
--   - piece F (engine/workflow_list_query.go's ListWorkflows/
--     CountWorkflows filters) needs def_name -- owner's call: this one
--     goes on workflow_payloads, NOT workflow_leases. Per 016's own split
--     rule, workflow_leases carries only columns written by claim,
--     heartbeat, reclaim, fence, signal delivery or promise delivery;
--     def_name/def_version are written by none of those and are read
--     only by this cold external listing, which already has to join
--     workflow_payloads for error_code/input/result -- def_name/
--     def_version ride that join at no extra cost rather than sitting on
--     the hot heartbeat row.
--
-- No backfill, per 016's own note: a fresh database is required for 0.5.0,
-- so no supported upgrade path has a pre-existing workflow_leases row to
-- catch up.

ALTER TABLE workflow_leases
    ADD COLUMN IF NOT EXISTS concurrency_key text,
    ADD COLUMN IF NOT EXISTS concurrency_key_hash bytea,
    ADD COLUMN IF NOT EXISTS parent_workflow_id text,
    ADD COLUMN IF NOT EXISTS continued_from text,
    ADD COLUMN IF NOT EXISTS run_wasm_instance_timeout_ms bigint,
    ADD COLUMN IF NOT EXISTS run_wasm_wall_clock_ceiling_ms bigint,
    ADD COLUMN IF NOT EXISTS run_host_retry_budget_ms bigint,
    ADD COLUMN IF NOT EXISTS run_max_workflow_duration_ms bigint;

-- Mirrors idx_instances_concurrency_key (migrations/postgres/001_schema.sql)
-- exactly, so piece B's cutover of the concurrency-key mutex check
-- (acquireCandidateConcurrencyKey) gets the same index shape on its new
-- table that it relied on against workflow_instances.
CREATE INDEX IF NOT EXISTS idx_leases_concurrency_key
    ON workflow_leases USING btree (tenant_id, concurrency_key_hash)
    WHERE (concurrency_key_hash IS NOT NULL);

ALTER TABLE workflow_payloads
    ADD COLUMN IF NOT EXISTS def_name text,
    ADD COLUMN IF NOT EXISTS def_version integer;
