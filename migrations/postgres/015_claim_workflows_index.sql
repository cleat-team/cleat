-- Migration 015: Add index to support ClaimWorkflows ORDER BY for
-- efficient polling even with many ready rows.
--
-- The ClaimWorkflows query orders by (sticky_worker_id match, priority, created_at).
-- Without an index covering these columns, PostgreSQL sorts all ready rows on disk
-- when the ready queue is large (29MB disk sort for ~422K rows).
--
-- This index covers the WHERE filter (tenant_id, task_queue, status='ready')
-- and includes priority+created_at so the ORDER BY can be satisfied from
-- the index without a sort. The first ORDER BY key (sticky_worker_id CASE)
-- prevents a pure index scan for ordering, but having the data pre-sorted
-- by the secondary keys still eliminates the disk sort and makes an
-- in-memory top-N heapsort very cheap.

DROP INDEX IF EXISTS idx_instances_ready_claim;
CREATE INDEX idx_instances_ready_claim
    ON workflow_instances (tenant_id, task_queue, priority, created_at)
    WHERE status = 'ready';
