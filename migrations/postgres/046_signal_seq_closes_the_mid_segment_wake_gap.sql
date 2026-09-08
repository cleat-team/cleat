-- cleat#953: a signal delivered while the workflow is AWAKE scheduled no wake.
--
-- DeliverSignal pulls next_wake_at forward with
--   UPDATE workflow_instances SET next_wake_at = now()
--   WHERE id = $1 AND status IN ('ready', 'suspended')
-- and a claimed workflow is status = 'running', so that matched zero rows.
-- The workflow then re-suspended and finalize_workflow_status overwrote
-- next_wake_at with its own timeout deadline, so the queued signal waited out
-- the full timeout and AwaitSignals reported a timeout with the signal sitting
-- in workflow_signals.
--
-- signal_seq is the channel that survives that window. DeliverSignal
-- increments it in the same transaction as the insert; the worker captures it
-- at claim time; finalize compares. A value that moved means a delivery
-- arrived while this segment was running, and the workflow wakes immediately
-- instead of sleeping to its deadline.
--
-- A COUNTER RATHER THAN "does workflow_signals have a row", which is the
-- obvious version and spins: a workflow awaiting {a} with an unrelated {z}
-- pending would wake, poll, find nothing it wants, re-suspend, and repeat.
-- The counter only moves on a NEW delivery, so a pending-but-unwanted signal
-- cannot drive the loop.
ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS signal_seq BIGINT NOT NULL DEFAULT 0;

-- signal_seq_at_claim is the value signal_seq held when this claim was taken.
--
-- Captured ON THE ROW rather than carried in the worker, because every claim
-- already runs an UPDATE that writes status, assigned_to, heartbeat_at and
-- generation -- so this is one more clause on a statement already executing,
-- and finalize compares two columns of the row it is already updating. The
-- alternative was a new parameter through FinalizeWorkflowSegment and its nine
-- implementations; the property being bought is that the comparison happens
-- INSIDE the finalize transaction, and the row capture gets that without the
-- interface change (cleat#953).
--
-- A reaper that reassigns a workflow mid-segment re-stamps this on the new
-- claim, so the new owner's window starts at its own claim. That is deliberate:
-- the new owner has seen nothing the old one did, so it should wake for
-- deliveries that arrived before it took over.
ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS signal_seq_at_claim BIGINT NOT NULL DEFAULT 0;
