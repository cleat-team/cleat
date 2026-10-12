-- cleat#3245 Phase 3 step 3 piece F's blocker, owner-approved 2026-10-12:
-- trace_id and completed_at are on NEITHER workflow_leases nor
-- workflow_payloads. Both are needed by GetWorkflowByID (folded into piece
-- F's scope by the owner) and trace_id alone is also needed by
-- ListWorkflows/CountWorkflows (engine/query_builder.go's
-- workflowInstanceColumns()).
--
-- Both go on workflow_payloads, not workflow_leases, by migration 016's own
-- classification rule: workflow_leases carries only columns written by
-- claim, heartbeat, reclaim, fence, signal delivery or promise delivery.
-- trace_id is written once by TraceWorkflow, none of those. completed_at is
-- written only by terminal transitions -- the same category as
-- result/error_msg, already on workflow_payloads.
--
-- completed_at's absence was not a decision anyone made for a reason: migration
-- 016 never discusses it, and migration 017's later comment
-- ("completed_at stays workflow_instances-only (absent from both new tables'
-- column lists, migration 016)") only observes that absence -- it does not
-- cite one. Verified before writing this: completed_at is 0 hits in
-- migrations 016 and 018; its hits in 017 and 019 are all inside
-- finalize_workflow_status's `UPDATE workflow_instances SET ...` -- writes to
-- the OLD table, not column grants on either new one.
--
-- No backfill, per 016's own note: a fresh database is required for 0.5.0,
-- so no supported upgrade path has a pre-existing workflow_leases/
-- workflow_payloads row to catch up.

ALTER TABLE workflow_payloads
    ADD COLUMN IF NOT EXISTS trace_id text,
    ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;
