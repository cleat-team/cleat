-- workflow_instances.started_at: when a worker first began executing this run.
--
-- cleat-team/cleat#1090. The row recorded created_at and completed_at and
-- nothing in between, so `completed_at - created_at` measured WAIT PLUS
-- EXECUTION -- a run that sat in a backlog for a minute and a run that executed
-- for a minute produced the same number. That is exactly the distinction an
-- operator needs and the one the pair could not make.
--
-- It was not recoverable from anywhere else. The first event in history is the
-- first STEP, so a workflow whose body opens with a durable sleep records
-- nothing at claim time -- and those are the runs whose latency is interesting.
-- assigned_to does not help either: finalize_workflow_status clears it on every
-- terminal branch, so a completed run names no worker.
--
-- COST: none beyond the column. The claim is a single UPDATE on all three
-- dialects and it already stamps heartbeat_at from the database clock, so this
-- is a sixth entry in a SET list that had five. The claim instant was ALREADY
-- being computed; it was written to heartbeat_at and then destroyed by the
-- first heartbeat that followed.
--
-- COALESCE, so the FIRST claim wins and a reclaim does not move it. Two of the
-- three questions #1090 names (queue latency, backlog drain rate) need the
-- first start; only execution-time-of-the-successful-attempt wants the latest,
-- and for a reclaimed run the longer figure is the honest one -- the work did
-- occupy that much wall clock, with reclaim_count available to say why. The
-- deciding argument is #1090's own: a column that gets rewritten is how
-- heartbeat_at came to lose this information, and stamping every claim would
-- reproduce that shape in a new column.
--
-- NULLABLE with no backfill. A run claimed before this migration has no
-- knowable start, and inventing one -- created_at, or completed_at -- would be
-- indistinguishable from a measured value to every reader. NULL says "not
-- recorded", which is true.
--
-- Idempotent: IF NOT EXISTS, and CREATE OR REPLACE below.
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS started_at timestamptz;

-- The THIRD postgres claim site, and the one a Go-only scan cannot see.
--
-- ClaimWorkflow and ClaimWorkflows issue their UPDATE from Go
-- (engine/store_lifecycle.go); ClaimWorkflowsAcrossTenants calls THIS function,
-- so a change made only in Go would leave cross-tenant dispatch -- the
-- multi-tenant worker's entire claim path -- silently unstamped.
--
-- CREATE OR REPLACE rather than 040's DROP and CREATE: this changes the SET
-- list only, not the RETURNS TABLE, so the return type is unchanged and the
-- owner, revoke and grant survive. 040 had to drop because it added a column to
-- the return type, which PostgreSQL refuses to replace.
CREATE OR REPLACE FUNCTION admin.claim_workflows(
    p_worker_id   text,
    p_task_queues text[],
    p_limit       integer
)
RETURNS TABLE (
    id                      text,
    def_name                text,
    def_version             integer,
    status                  text,
    input                   jsonb,
    assigned_to             text,
    next_wake_at            timestamptz,
    tenant_id               uuid,
    created_at              timestamptz,
    error_code              text,
    error_op                text,
    generation              bigint,
    priority                integer,
    trace_id                text,
    pending_terminal_status text
)
LANGUAGE sql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
    WITH candidates AS (
        SELECT w.id FROM workflow_instances w
        WHERE w.status IN ('ready', 'terminating')
          AND w.next_wake_at <= now()
          AND w.task_queue = ANY(p_task_queues)
        ORDER BY w.priority ASC, w.created_at
        LIMIT p_limit
        FOR UPDATE SKIP LOCKED
    )
    UPDATE workflow_instances w
    SET status = 'running',
        assigned_to = p_worker_id,
        heartbeat_at = now(),
        started_at = COALESCE(w.started_at, now()),
        generation = w.generation + 1
    FROM candidates c
    WHERE w.id = c.id
    RETURNING w.id, w.def_name, w.def_version, w.status, w.input, w.assigned_to,
              w.next_wake_at, w.tenant_id, w.created_at, w.error_code, w.error_op,
              w.generation, COALESCE(w.priority, 0), COALESCE(w.trace_id, ''),
              COALESCE(w.pending_terminal_status, '');
$$;
