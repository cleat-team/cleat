-- cleat migration 093 (postgres): a queue declares its own concurrency limit
--
-- cleat#1116. Owner decision: follow DBOS's functionality -- concurrency=N for
-- N > 1, deferral not rejection. `concurrency_keys` cannot express N > 1: its
-- primary key is `key_hash`, one row per key, so it is a mutex rather than a
-- semaphore. This migration adds the missing piece -- a declared capacity --
-- without touching the claim path or existing keys at all. Generalising the
-- claim predicate from `NOT EXISTS` to `COUNT(*) < N` is separate work.
--
-- EXPLICIT REGISTRATION, NOT IMPLICIT-ON-FIRST-USE. `concurrency_key` today is
-- a bare string a caller supplies inline; DBOS's `Queue(name, concurrency=N)`
-- is a named, first-class object declared once. The thread on cleat#1116 flagged
-- the alternative -- capacity conjured by whichever workflow starts first --
-- as a race with no good answer ("who declares N, and when?"). Explicit
-- registration answers it outright: a queue exists before anything starts
-- against it, or starting against an unregistered name is just today's bare
-- concurrency_key (N=1, unaffected by anything here).
--
-- #1702-CONFORMING. cleat#1702 settled the shared design for exactly this
-- class of entity -- operator-created, outlives every run that uses it, idle
-- is its normal state -- and a queue is the twelfth instance the sequencing
-- decision on cleat#1116 named. created_at / updated_at / disabled_at, no new
-- retirement spelling, `disabled_at` following the same soft-flag pattern
-- workflow_schedules.enabled and workflow_defs.deprecated were both converted
-- to.
--
-- SHAPED LIKE tenant_secrets (081) / tenant_domains (080), the two most recent
-- tenant-scoped tables added after 001: PRIMARY KEY (tenant_id, name), FK to
-- admin.tenants ON DELETE CASCADE, RLS via ENABLE+FORCE+policy, deletion by
-- cascade rather than a DELETE inside admin.drop_tenant.

CREATE TABLE IF NOT EXISTS queues (
    tenant_id          UUID NOT NULL
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,

    -- Same charset constraint as tenant_secrets.name, for the same reason: a
    -- row written by any other means must not carry a name the Go-side
    -- validator would refuse to match.
    name                TEXT NOT NULL,

    concurrency_limit   INTEGER NOT NULL,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at         TIMESTAMPTZ,

    PRIMARY KEY (tenant_id, name),

    CONSTRAINT ck_queues_name_charset
        CHECK (name ~ '^[A-Za-z0-9_.-]{1,128}$'),
    CONSTRAINT ck_queues_concurrency_limit_positive
        CHECK (concurrency_limit >= 1)
);

-- Row-level security, in the shape 039/079/081 established. A queue without it
-- would let one tenant see, or worse disable, another tenant's admission
-- control.
ALTER TABLE queues ENABLE ROW LEVEL SECURITY;
ALTER TABLE queues FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_queues ON queues;
CREATE POLICY tenant_isolation_queues ON queues
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

-- Grant queues to every tenant login role, following 064's pattern exactly:
-- CREATE OR REPLACE the shared function (create_tenant_role calls it for new
-- roles), then re-grant for roles that already exist.
--
-- SELECT, INSERT, UPDATE, DELETE: a tenant connection both registers its own
-- queues and, once the claim path is generalised, reads concurrency_limit at
-- claim time -- the same per-workflow-path reasoning 064 gave for
-- concurrency_keys.
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

    -- cleat#1116. The twelfth #1702-conforming table, and the first one this
    -- function grants.
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.queues TO %I', current_schema(), p_role_name);

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
