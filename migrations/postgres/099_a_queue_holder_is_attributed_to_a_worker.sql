-- cleat migration 099 (postgres): a queue holder is attributed to a worker
--
-- cleat#1917, second piece. 098 declares a queue's optional worker_concurrency;
-- this column is what the claim path reads to enforce it. Decision 3 on the
-- issue: "what counts: slot-holders attributed to a worker, INCLUDING parked
-- runs" -- a worker's count is its unexpired queue_holders (094) for a
-- (tenant_id, queue_name), by worker_id, whether the holder's workflow is
-- currently executing or parked (`assigned_to = NULL, status = 'ready'`,
-- ReleaseWorkflow). A parked run still holds its slot; see the issue for the
-- DBOS-parity argument and the in-tree fixture it rests on.
--
-- NULLABLE, WITH NO BACKFILL. A holder row inserted before this migration
-- carries no worker_id and reads NULL, which the claim path's worker-cap
-- count (`WHERE worker_id = <claiming worker>`) never matches -- such a row
-- counts toward no worker's cap until it expires or is released, which is at
-- most claimedKeyTTL (30 minutes) after this migration runs. Backfilling is
-- not possible: nothing recorded which worker held a pre-migration row.
--
-- A woken run's holder MOVES rather than duplicates: decision 4 on the issue
-- says a parked run's holder, once claimed by a (possibly different) worker,
-- has its worker_id updated to that worker and that worker's cap applies from
-- then on. The claim path does this with an UPDATE, not a new INSERT, so the
-- row's identity (tenant_id, queue_name, workflow_id) is unchanged and no new
-- migration is needed to express it.
-- TEXT, matching workflow_instances.assigned_to's value (a generateWorkerID
-- hex string) -- postgres has no column-width reason to bound it further.
ALTER TABLE queue_holders
    ADD COLUMN IF NOT EXISTS worker_id TEXT;

-- The worker-cap count's own index: `WHERE tenant_id = ? AND queue_name = ?
-- AND worker_id = ? AND expires_at > now()`, mirroring idx_queue_rate_tokens_window
-- (097) for the same query shape one level down.
CREATE INDEX IF NOT EXISTS idx_queue_holders_worker
    ON queue_holders(tenant_id, queue_name, worker_id, expires_at);
