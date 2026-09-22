-- cleat migration 097 (postgres): a queue rate token expires with its window
--
-- cleat#1918, second piece. 096 declares a queue's rate_limit/rate_period_seconds;
-- this table is the counter the claim path reads to enforce it. One row per
-- admission through a rate-limited queue, holding EXPIRES_AT rather than
-- admitted_at, deliberately: "how many admissions are inside the trailing
-- window right now" and "which rows have aged out and can be reaped" are the
-- same question asked twice, and expires_at answers both with one comparison
-- against now() -- exactly the shape queue_holders (094) and concurrency_keys
-- (001) already use for the same reason. admitted_at is not stored; it is
-- recoverable as expires_at - rate_period_seconds for anyone who needs it,
-- and every write already knows its own period.
--
-- SHAPED LIKE queue_holders, NOT queues. Transient, derived, no
-- created_at/updated_at/disabled_at, no #1702 lifecycle -- a token's entire
-- lifecycle is "exists until its window closes", same as a holder's is
-- "exists until the slot is released or its lease expires".
--
-- NO FOREIGN KEY ON workflow_id, UNLIKE queue_holders. This is the one place
-- this table deliberately does NOT copy 094's shape, and the reason is a
-- lifetime mismatch rather than an oversight: queue_holders' CASCADE is
-- correct because a holder's relevance ends exactly when its workflow row
-- does. A rate token's relevance ends rate_period_seconds after admission,
-- which is unrelated to -completed-workflow-retention-days -- a short
-- retention window combined with a longer rate period would let workflow
-- completion silently delete rate-limiting evidence out from under an
-- in-progress window, undercounting admissions and letting a burst through
-- that the limiter should have caught. Keeping this table independent of
-- workflow_instances' lifecycle removes that coupling entirely; workflow_id
-- is carried for diagnostics only, the same role concurrency_key plays in
-- logClaimKeyDecision (store_lifecycle.go), not as a referential constraint.
--
-- NO ROW-LEVEL SECURITY, matching queue_holders' own reasoning exactly: this
-- table is written and read by the same claim path, tenant-scoped by an
-- explicit tenant_id predicate everywhere it is touched (094's header gives
-- the fuller version of this argument). A policy here would only re-state a
-- predicate the claim already carries.

CREATE TABLE IF NOT EXISTS queue_rate_tokens (
    tenant_id    UUID NOT NULL
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,
    queue_name   TEXT NOT NULL,
    workflow_id  TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);

-- The count-in-window query's own index: every admission check does
-- `WHERE tenant_id = ? AND queue_name = ? AND expires_at > now()`.
CREATE INDEX IF NOT EXISTS idx_queue_rate_tokens_window
    ON queue_rate_tokens(tenant_id, queue_name, expires_at);

-- The reaper's index, mirroring idx_queue_holders_expires (094) exactly --
-- ReapExpiredConcurrencyKeys sweeps `WHERE expires_at < now()` with no other
-- predicate once tenant_id is bound.
CREATE INDEX IF NOT EXISTS idx_queue_rate_tokens_expires
    ON queue_rate_tokens(expires_at);

-- Grant to every tenant login role, following 093/094's pattern: CREATE OR
-- REPLACE the shared function, then re-grant for roles that already exist.
-- SELECT, INSERT, DELETE: the claim path counts, inserts on admission, and
-- the reaper deletes expired rows. No UPDATE -- a token is never modified
-- after it is written, only counted or removed.
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
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.queue_holders TO %I', current_schema(), p_role_name);

    -- cleat#1918. The rate-limit counter table; granted beside queue_holders,
    -- its nearest sibling in shape.
    EXECUTE format('GRANT SELECT, INSERT, DELETE ON %I.queue_rate_tokens TO %I', current_schema(), p_role_name);

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
