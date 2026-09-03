-- cleat migration 038 (postgres): a workflow can owe a defer phase
--
-- D6, decided 2026-09-02 (tiers.yaml): terminate is ASYNCHRONOUS, and the
-- window between "terminate was asked for" and "the workflow is terminal" gets
-- its own status rather than reusing 'ready'. IMPROVEMENT-PLAN 3.75 step 1.
--
-- Why a marker on workflow_instances rather than a row somewhere. A defer body
-- needs a live instance WITH a session -- 3.35 phase 2 measured that a defer
-- without one panics inside any host call, and a defer that cannot call the
-- host cannot release the lock it took. A live instance means replay, which
-- means dispatch, which means the workflow must be claimable and NON-TERMINAL.
-- So the only genuinely new durable fact is workflow-level: "this workflow owes
-- a defer phase, and here is the outcome to apply once it is done."
--
-- What is owed is NOT recorded here. DeferralsFromHistory (engine/helpers.go)
-- reconstructs the registered set from the EventTypeDefer rows, which carry
-- defer_id and defer_description and are written on the normal path. And each
-- defer body's own host calls are durable calls with their own event rows and
-- their own intent handling, so a crash INSIDE the defer phase is already
-- covered at the granularity that works. 3.75 rejected both a per-defer table
-- (a second scheduler, not a second table) and a pending event_history row
-- (the fence is defined on a live claim, and TerminateWorkflow bumps
-- generation).
--
-- Two columns:
--
--   pending_terminal_status  the outcome to finalize with when the defer phase
--                            completes. NULL means no defer phase is owed,
--                            which is every row today.
--   defer_phase_deadline     when the reaper may conclude the defer phase died
--                            and re-queue the workflow. Deliberately separate
--                            from heartbeat staleness: a defer phase that is
--                            claimed and heartbeating is not stale, and one
--                            whose worker vanished is caught by the existing
--                            heartbeat sweep. This bounds the PHASE, so a
--                            workflow cannot sit in 'terminating' forever
--                            because its defers trap every time.
--
-- error_msg and error_code already exist and carry the outcome's detail, so
-- there is nothing new to add for those.
--
-- No CHECK constraint on status to update: workflow_instances.status is plain
-- TEXT (001_schema.sql:227), so 'terminating' needs no schema change to be
-- storable. Verified before writing this file rather than assumed -- a
-- CHECK-constrained status column would have made this migration bigger on the
-- two dialects that spell constraints differently.

ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS pending_terminal_status TEXT;

ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS defer_phase_deadline TIMESTAMPTZ;

-- The dispatch loop's claim query filters on status and next_wake_at, and a
-- workflow in 'terminating' is claimed through that same path -- so no new
-- index is needed for dispatch.
--
-- The reaper's sweep is the new access pattern: find rows whose defer phase has
-- outrun its deadline. Partial, because the overwhelming majority of rows have
-- no defer phase owed and should not be in this index at all.
CREATE INDEX IF NOT EXISTS idx_workflow_instances_defer_phase_deadline
    ON workflow_instances (defer_phase_deadline)
    WHERE pending_terminal_status IS NOT NULL;
