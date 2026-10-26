-- cleat#3245 B3a: readers of the lease columns read workflow_leases.
--
-- The engine's remaining PostgreSQL readers of status, continued_from and the
-- run_* limits moved from workflow_instances to workflow_leases in the same
-- change. Two of them filter on a column workflow_leases had no index for, so
-- moving them as-is would have traded an index lookup for a sequential scan:
--
--   * successorOfRun looks up a run by continued_from. workflow_instances has
--     idx_workflow_instances_continued_from; this is its lease twin.
--   * the retention sweeps select terminal runs by status. workflow_instances
--     has idx_instances_terminal_completed (tenant_id, status, completed_at);
--     completed_at lives on workflow_payloads since B1a, so the lease twin
--     carries (tenant_id, status) over every terminal status the sweeps name.
--
-- admin.in_flight_workflow_ids() reads status, so it is redefined here to read
-- workflow_leases. CREATE OR REPLACE keeps its owner (cleat_dispatcher, which
-- has BYPASSRLS and SELECT on workflow_leases exactly as on workflow_instances)
-- and its grants, so it still answers across every tenant. Its attributes are
-- restated because CREATE OR REPLACE replaces them all.
--
-- The workflow_instances indexes these duplicate are dropped in the baseline
-- regeneration (cleat#3362), not here. Unqualified, like 003 and 005: the
-- migration runner sets search_path to the target schema.

CREATE INDEX IF NOT EXISTS idx_leases_continued_from
    ON workflow_leases (continued_from)
    WHERE continued_from IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_leases_terminal
    ON workflow_leases (tenant_id, status)
    WHERE status IN ('done', 'failed', 'terminated', 'cancelled', 'dead_lettered');

CREATE OR REPLACE FUNCTION admin.in_flight_workflow_ids() RETURNS TABLE(id text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path FROM CURRENT
    AS $$
    SELECT l.id
    FROM workflow_leases l
    WHERE l.status IN ('ready', 'running');
$$;
