-- cleat migration 094 (postgres): a queue holder occupies a declared slot
--
-- cleat#1116, second piece. `queues` (093) declares a concurrency limit; this
-- table records a claimed workflow's occupancy of one of those slots. The claim
-- path (separate work) inserts a row per admitted holder and deletes it on
-- release/terminal commit; a row left behind by a crash expires via
-- `expires_at` and is reaped.
--
-- Deliberately NOT #1702-conforming. Unlike `queues`, a holder is transient
-- state, not an operator-created entity that outlives every run. It mirrors
-- `concurrency_keys` -- the mutex holder table this semaphore generalises --
-- which has no created_at/updated_at/disabled_at either.
--
-- SHAPED LIKE concurrency_keys (001) rather than queues (093), on purpose:
-- transient, FOREIGN KEY to workflow_instances ON DELETE CASCADE, and NO
-- row-level security policy of its own. Tenant scoping is the primary key plus
-- the explicit tenant_id every write and read carries, exactly as
-- concurrency_keys relies on it -- and for the same reason: the claim path's
-- candidates are RLS-scoped, so a holder's tenant_id is always the claim's own
-- tenant, and a bare predicate would only ever re-state it.
--
-- No FOREIGN KEY to queues. concurrency_keys has none to any entity table, and
-- adding one here would make every holder INSERT take a FOR KEY SHARE lock on
-- the queues row -- the same row the claim holds FOR UPDATE as the
-- serialisation point. Referential integrity to queues is enforced by the claim
-- path itself, which only inserts a holder for a name the LEFT JOIN proved is a
-- registered, non-disabled queue.

CREATE TABLE IF NOT EXISTS queue_holders (
    tenant_id    UUID NOT NULL,
    queue_name   TEXT NOT NULL,
    workflow_id  TEXT NOT NULL REFERENCES workflow_instances(id) ON DELETE CASCADE,
    expires_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, queue_name, workflow_id)
);

CREATE INDEX IF NOT EXISTS idx_queue_holders_workflow ON queue_holders(workflow_id);
CREATE INDEX IF NOT EXISTS idx_queue_holders_expires ON queue_holders(expires_at);

-- Grant to tenant login roles, following 093's pattern for queues: add the
-- table to the shared function (create_tenant_role calls it for new roles),
-- then re-grant for roles that already exist. Same SELECT/INSERT/UPDATE/DELETE
-- shape as concurrency_keys: a tenant connection both occupies a slot at claim
-- time and releases/reaps it.
CREATE OR REPLACE FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name TEXT)
RETURNS void AS $$
BEGIN
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_defs TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_instances TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.event_history TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_signals TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_schedules TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_promises TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.idempotency_keys TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.concurrency_keys TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_update_requests TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_tags TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT ON %I.tenant_settings TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.queues TO %I', current_schema(), p_role_name);

    -- cleat#1116. The holder table for declared queues; granted beside the
    -- queue itself, not beside concurrency_keys, so the pairing is visible.
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.queue_holders TO %I', current_schema(), p_role_name);

    EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA %I TO %I', current_schema(), p_role_name);
END;
$$ LANGUAGE plpgsql SECURITY DEFINER
SET search_path FROM CURRENT;

DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN SELECT role_name FROM admin.tenant_roles LOOP
        BEGIN
            PERFORM admin.grant_core_tables_to_tenant_role(r.role_name);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'could not re-grant core tables to %: % (SQLSTATE %)',
                r.role_name, SQLERRM, SQLSTATE;
        END;
    END LOOP;
END;
$$;
