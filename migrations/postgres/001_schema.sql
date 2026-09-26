-- cleat consolidated schema (001)
--
-- GENERATED from a pg_dump of the database built by the previous
-- migrations/postgres chain, then wrapped by hand. cleat#2059.
--
-- Every CREATE TABLE carries its FINAL column set -- the dump has already
-- applied every ALTER TABLE ADD COLUMN, so there is nothing to fold by hand.
-- Every routine in 003 is likewise the LAST definition of that routine, which
-- is the property the design doc warns is easy to get wrong by reading files.
--
-- The schema cleat builds into is never named here: names are unqualified and
-- resolve through search_path, which the runner sets. `admin.` and `tenant_*`
-- are not the configured schema and stay qualified. See
-- migration/migrations_do_not_hardcode_the_schema_test.go.
-- ── Cluster roles ───────────────────────────────────────────────────────────
-- Roles live in the CLUSTER, not the database, so no pg_dump of a database can
-- carry them -- while every GRANT below names one. Created here, idempotently,
-- from the migrations that owned them: 005_app_role.sql (cleat_app),
-- 023_cross_tenant_claim.sql (cleat_dispatcher) and 077 (cleat_sweep).
--
-- cleat_app is the role the engine is meant to run as. It is created NOLOGIN
-- and without a password on purpose: a credential does not belong in a file
-- that is committed and applied by every worker at boot. The deployment
-- supplies it with ALTER ROLE cleat_app LOGIN PASSWORD '...'.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        CREATE ROLE cleat_app
            NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END $$;

-- Correct the attributes if the role PRE-EXISTED with different ones. From 005,
-- and it is here for the same reason the memberships below are: role attributes
-- live in the CLUSTER, so a fresh database on a cluster that already has a
-- cleat_app does not get them from the CREATE above -- that branch is skipped by
-- IF NOT EXISTS, and nothing else would notice.
--
-- Only the differing case executes, deliberately (005's own note): asserting a
-- value the CREATE branch just set makes this the first file to fail on managed
-- PostgreSQL, where no true superuser exists and the ALTER would be a no-op
-- refusal. A deployment that really did grant SUPERUSER to cleat_app still
-- fails here, loudly, and should.
DO $$
DECLARE
    r RECORD;
BEGIN
    SELECT rolsuper, rolcreatedb, rolcreaterole, rolbypassrls
      INTO r FROM pg_roles WHERE rolname = 'cleat_app';

    IF r.rolsuper     THEN ALTER ROLE cleat_app NOSUPERUSER;  END IF;
    IF r.rolcreatedb  THEN ALTER ROLE cleat_app NOCREATEDB;   END IF;
    IF r.rolcreaterole THEN ALTER ROLE cleat_app NOCREATEROLE; END IF;
    IF r.rolbypassrls THEN ALTER ROLE cleat_app NOBYPASSRLS;  END IF;
END $$;

-- BYPASSRLS is the whole point of this one: the `global` claim strategy reads
-- across tenants and needs the exemption. Only a superuser can grant it, so a
-- deployment applying these files as a non-superuser gets the NOTICE and the
-- cross-tenant path stays unusable -- which is why `rotate` is the default
-- strategy. 023's own branch, kept so the outcome is a notice rather than a
-- failed migration.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        BEGIN
            CREATE ROLE cleat_dispatcher NOLOGIN BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'cleat_dispatcher needs BYPASSRLS, which only a superuser can grant, and this connection is not one. The cross-tenant claim function is still created but will not see across tenants; use --claim-strategy=rotate, which needs no grant. (SQLSTATE %)', SQLSTATE;
        END;
    END IF;
END $$;

-- The retention sweeper. NOBYPASSRLS is the default, stated because it is the
-- point: the sweep must be subject to the policies it runs under.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_sweep') THEN
        CREATE ROLE cleat_sweep NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
    END IF;
END
$$;

-- From 077. A role comment is stored in pg_shdescription, which is CLUSTER-wide
-- like pg_auth_members -- so no pg_dump of a database carries it and no
-- per-database catalog diff can see its absence. Restored here for the same
-- reason the memberships below are.
COMMENT ON ROLE cleat_sweep IS
    'Cross-tenant plugin sweeps (cleat#1490). Entered with SET LOCAL ROLE from '
    'engine/plugindb_tenant.go; never connected to directly. Granted WITH '
    'INHERIT FALSE so membership alone does not apply its policies.';

-- The DEFAULT tenant's login role. Normally a tenant role is provisioned at
-- runtime by admin.create_tenant_role, which derives its password from the
-- worker's key -- but this file's own GRANTs below name it, so it has to exist
-- before they run, and 002 -- which seeds every other row of the default tenant
-- -- runs after this file.
--
-- Creating it here with no password is deliberate and matches how cleat_app is
-- created: the password is not this file's to know. The worker re-ALTERs it to
-- the derived value on its next boot, which is the documented rotation path
-- (see 064). LOGIN is set because that is the attribute a provisioned tenant
-- role carries; a passwordless LOGIN role cannot authenticate in any case.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles
                   WHERE rolname = 'cleat_tenant_00000000_0000_0000_0000_000000000000') THEN
        CREATE ROLE cleat_tenant_00000000_0000_0000_0000_000000000000
            LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END $$;

-- ── Role memberships ────────────────────────────────────────────────────────
-- From 077, the only migration that ever granted these. A role GRANT does not
-- belong in a per-database dump, so the rebaseline had to carry it explicitly
-- -- and dropping it is silent in exactly the way 077's own comment warns
-- about one line further on: nothing in a catalog diff of the DATABASE can see
-- a membership, because pg_auth_members is CLUSTER-wide.
--
-- Membership WITH INHERIT FALSE: enough to SET ROLE, not enough to match the
-- sweep policy passively. This distinction is load-bearing and silent when got
-- wrong -- measured, a plain GRANT lets the application role read all 400000
-- rows with no error and the correct number of policies; WITH INHERIT FALSE
-- returns 1000. PostgreSQL 16+.
--
-- `cleat_app` is the one the worker names: cmd/cleat-worker/setup.go does
-- `SET LOCAL ROLE cleat_sweep` on the retention path, and without this membership
-- that fails with 42501 `permission denied to set role "cleat_sweep"`.
--
-- The loop covers whatever cleat_tenant_% roles exist at this instant. On a
-- fresh bootstrap that is the default tenant role just created above, which is
-- exactly what 077 swept on a fresh database too -- so the rebaseline preserves
-- the behaviour rather than narrowing it. (Roles a worker provisions later via
-- admin.create_tenant_role are not covered, and never were: no migration after
-- 077 granted this.)
DO $$
DECLARE
    r record;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        EXECUTE 'GRANT cleat_sweep TO cleat_app WITH INHERIT FALSE';
    END IF;
    FOR r IN SELECT rolname FROM pg_roles WHERE rolname LIKE 'cleat_tenant\_%' LOOP
        EXECUTE format('GRANT cleat_sweep TO %I WITH INHERIT FALSE', r.rolname);
    END LOOP;
END
$$;


CREATE SCHEMA IF NOT EXISTS admin;

CREATE SCHEMA IF NOT EXISTS cleat;

CREATE SCHEMA IF NOT EXISTS tenant_00000000_0000_0000_0000_000000000000;

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE SEQUENCE IF NOT EXISTS workflow_memory_samples_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE SEQUENCE IF NOT EXISTS workflow_signals_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE IF NOT EXISTS admin.orgs (
    org_id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL
);

CREATE TABLE IF NOT EXISTS admin.plugin_tables (
    plugin_name text NOT NULL,
    table_name text NOT NULL,
    schema_name text DEFAULT "current_schema"() NOT NULL,
    tenant_scoped boolean DEFAULT false NOT NULL
);

CREATE TABLE IF NOT EXISTS admin.tenant_api_keys (
    key_id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    key_hash bytea NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    disabled_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    oauth_identity text,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL
);

CREATE TABLE IF NOT EXISTS admin.tenant_egress_allow (
    tenant_id uuid NOT NULL,
    host text NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    disabled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL
);

CREATE TABLE IF NOT EXISTS admin.tenant_roles (
    tenant_id uuid NOT NULL,
    role_name text NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    disabled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL
);

CREATE TABLE IF NOT EXISTS admin.tenants (
    tenant_id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    suspended boolean DEFAULT false NOT NULL,
    org_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

CREATE TABLE IF NOT EXISTS admin.workers (
    worker_id text NOT NULL,
    hostname text DEFAULT ''::text NOT NULL,
    pid integer DEFAULT 0 NOT NULL,
    concurrency integer DEFAULT 0 NOT NULL,
    connection_budget integer DEFAULT 0 NOT NULL,
    started_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    last_heartbeat_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    secret_key_versions text
);

CREATE TABLE IF NOT EXISTS concurrency_keys (
    key_hash bytea NOT NULL,
    key_text text NOT NULL,
    workflow_id text NOT NULL,
    acquired_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

ALTER TABLE ONLY concurrency_keys FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS deployment_secrets (
    name text NOT NULL,
    ciphertext text NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    disabled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT ck_deployment_secrets_ciphertext_nonempty CHECK ((length(ciphertext) > 0)),
    CONSTRAINT ck_deployment_secrets_name_charset CHECK ((name ~ '^[A-Za-z0-9_.-]{1,128}$'::text))
);

CREATE TABLE IF NOT EXISTS event_history (
    workflow_id text NOT NULL,
    step integer NOT NULL,
    service text,
    operation text,
    request text,
    response text,
    error text,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    event_type text DEFAULT 'call'::text NOT NULL,
    duration_ms bigint,
    signal_names text,
    timeout_ms bigint,
    signal_name text,
    signal_payload text,
    defer_description text,
    defer_id text,
    child_name text,
    child_input text,
    run_id text,
    new_input text,
    plugin_name text,
    plugin_func text,
    plugin_input text,
    plugin_output text,
    plugin_error text,
    promise_name text,
    promise_id text,
    promise_result text,
    promise_error text,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    payload jsonb,
    checksum text,
    thread_id text DEFAULT 'main'::text NOT NULL,
    local_step integer DEFAULT 0 NOT NULL,
    global_seq bigint DEFAULT 0 NOT NULL,
    intent_at TIMESTAMPTZ,
    payload_encoding smallint
)
PARTITION BY HASH (tenant_id);

ALTER TABLE ONLY event_history FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key_hash bytea NOT NULL,
    workflow_id text NOT NULL,
    error_msg text,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    expires_at TIMESTAMPTZ DEFAULT (now() + '7 days'::interval) NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    def_name text,
    input_digest text
);

ALTER TABLE ONLY idempotency_keys FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS plugin_defs (
    name text NOT NULL,
    version text NOT NULL,
    wasm_bytes bytea,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    deprecated boolean DEFAULT false NOT NULL
);

CREATE TABLE IF NOT EXISTS queue_holders (
    tenant_id uuid NOT NULL,
    queue_name text NOT NULL,
    workflow_id text NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    worker_id text
);

CREATE TABLE IF NOT EXISTS queue_rate_tokens (
    tenant_id uuid NOT NULL,
    queue_name text NOT NULL,
    workflow_id text NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS queues (
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    concurrency_limit integer NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    disabled_at TIMESTAMPTZ,
    rate_limit integer,
    rate_period_seconds integer,
    worker_concurrency integer,
    CONSTRAINT ck_queues_concurrency_limit_positive CHECK ((concurrency_limit >= 1)),
    CONSTRAINT ck_queues_name_charset CHECK ((name ~ '^[A-Za-z0-9_.-]{1,128}$'::text)),
    CONSTRAINT ck_queues_rate_limit_paired CHECK (((rate_limit IS NULL) = (rate_period_seconds IS NULL))),
    CONSTRAINT ck_queues_rate_limit_positive CHECK (((rate_limit IS NULL) OR (rate_limit >= 1))),
    CONSTRAINT ck_queues_rate_period_positive CHECK (((rate_period_seconds IS NULL) OR (rate_period_seconds >= 1))),
    CONSTRAINT ck_queues_worker_concurrency_le_concurrency CHECK (((worker_concurrency IS NULL) OR (worker_concurrency <= concurrency_limit))),
    CONSTRAINT ck_queues_worker_concurrency_positive CHECK (((worker_concurrency IS NULL) OR (worker_concurrency >= 1)))
);

ALTER TABLE ONLY queues FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS slack_workspace (
    team_id text NOT NULL,
    tenant_id uuid NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT ck_slack_workspace_team_id_length CHECK ((length(team_id) <= 32)),
    CONSTRAINT ck_slack_workspace_team_id_shape CHECK ((team_id ~ '^[TE][A-Z0-9]+$'::text))
);

CREATE TABLE IF NOT EXISTS tenant_domains (
    hostname text NOT NULL,
    tenant_id uuid NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    disabled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT ck_tenant_domains_hostname_lowercase CHECK ((hostname = lower(hostname))),
    CONSTRAINT ck_tenant_domains_hostname_no_port CHECK ((POSITION((':'::text) IN (hostname)) = 0)),
    CONSTRAINT ck_tenant_domains_hostname_nonempty CHECK ((length(hostname) > 0))
);

ALTER TABLE ONLY tenant_domains FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS tenant_secrets (
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    ciphertext text NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT ck_tenant_secrets_ciphertext_nonempty CHECK ((length(ciphertext) > 0)),
    CONSTRAINT ck_tenant_secrets_name_charset CHECK ((name ~ '^[A-Za-z0-9_.-]{1,128}$'::text))
);

ALTER TABLE ONLY tenant_secrets FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS tenant_settings (
    tenant_id uuid NOT NULL,
    wasm_instance_timeout_ms bigint,
    wasm_wall_clock_ceiling_ms bigint,
    host_retry_budget_ms bigint,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    max_workflow_duration_ms bigint,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT ck_tenant_settings_instance_timeout_positive CHECK (((wasm_instance_timeout_ms IS NULL) OR (wasm_instance_timeout_ms > 0))),
    CONSTRAINT ck_tenant_settings_retry_budget_positive CHECK (((host_retry_budget_ms IS NULL) OR (host_retry_budget_ms > 0))),
    CONSTRAINT ck_tenant_settings_wall_clock_positive CHECK (((wasm_wall_clock_ceiling_ms IS NULL) OR (wasm_wall_clock_ceiling_ms > 0))),
    CONSTRAINT ck_ts_max_workflow_duration_positive CHECK (((max_workflow_duration_ms IS NULL) OR (max_workflow_duration_ms > 0)))
);

ALTER TABLE ONLY tenant_settings FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_defs (
    name text NOT NULL,
    version integer NOT NULL,
    wasm_bytes bytea NOT NULL,
    entry_points text[] DEFAULT '{}'::text[] NOT NULL,
    min_version integer DEFAULT 0 NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    max_history_length integer DEFAULT 0 NOT NULL,
    dag_spec jsonb,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    task_queue text DEFAULT 'default'::text NOT NULL,
    abi_version integer DEFAULT 1 NOT NULL,
    plugin_deps jsonb DEFAULT '{}'::jsonb NOT NULL,
    disabled_at TIMESTAMPTZ,
    gc_eligible boolean DEFAULT false NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL
);

ALTER TABLE ONLY workflow_defs FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_instances (
    id text NOT NULL,
    def_name text NOT NULL,
    def_version integer NOT NULL,
    status text DEFAULT 'ready'::text NOT NULL,
    input jsonb DEFAULT '{}'::jsonb NOT NULL,
    assigned_to text,
    heartbeat_at TIMESTAMPTZ,
    next_wake_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    completed_at TIMESTAMPTZ,
    cancellation_requested boolean DEFAULT false NOT NULL,
    cancellation_reason text,
    result jsonb,
    error_msg text,
    error_code text,
    error_op text,
    parent_workflow_id text,
    parent_close_policy text DEFAULT 'ABANDON'::text,
    query_state jsonb DEFAULT '{}'::jsonb,
    trace_id text,
    sticky_worker_id text,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    task_queue text DEFAULT 'default'::text NOT NULL,
    compaction_state jsonb,
    compacted_at TIMESTAMPTZ,
    compaction_step integer,
    plugin_vers jsonb DEFAULT '{}'::jsonb NOT NULL,
    event_count bigint DEFAULT 0 NOT NULL,
    allowed_signals jsonb,
    priority integer DEFAULT 0 NOT NULL,
    generation bigint DEFAULT 0 NOT NULL,
    pending_terminal_status text,
    defer_phase_deadline TIMESTAMPTZ,
    continued_from text,
    signal_seq bigint DEFAULT 0 NOT NULL,
    signal_seq_at_claim bigint DEFAULT 0 NOT NULL,
    signal_consumed_seq bigint DEFAULT 0 NOT NULL,
    signal_consumed_at_claim bigint DEFAULT 0 NOT NULL,
    reclaim_count bigint DEFAULT 0 NOT NULL,
    started_at TIMESTAMPTZ,
    concurrency_key text,
    concurrency_key_hash bytea,
    run_wasm_instance_timeout_ms bigint,
    run_wasm_wall_clock_ceiling_ms bigint,
    run_host_retry_budget_ms bigint,
    completed_by text,
    run_max_workflow_duration_ms bigint,
    history_swept_at TIMESTAMPTZ,
    CONSTRAINT ck_wi_run_instance_timeout_positive CHECK (((run_wasm_instance_timeout_ms IS NULL) OR (run_wasm_instance_timeout_ms > 0))),
    CONSTRAINT ck_wi_run_max_workflow_duration_positive CHECK (((run_max_workflow_duration_ms IS NULL) OR (run_max_workflow_duration_ms > 0))),
    CONSTRAINT ck_wi_run_retry_budget_positive CHECK (((run_host_retry_budget_ms IS NULL) OR (run_host_retry_budget_ms > 0))),
    CONSTRAINT ck_wi_run_wall_clock_positive CHECK (((run_wasm_wall_clock_ceiling_ms IS NULL) OR (run_wasm_wall_clock_ceiling_ms > 0)))
);

ALTER TABLE ONLY workflow_instances FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_memory_samples (
    id bigint NOT NULL,
    def_name text NOT NULL,
    sample_bytes bigint NOT NULL,
    recorded_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

ALTER TABLE ONLY workflow_memory_samples FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_memory_stats (
    def_name text NOT NULL,
    mean_bytes double precision DEFAULT 0 NOT NULL,
    sample_count integer DEFAULT 0 NOT NULL,
    alpha double precision DEFAULT 0.3 NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

ALTER TABLE ONLY workflow_memory_stats FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_promises (
    workflow_id text NOT NULL,
    promise_id text NOT NULL,
    promise_name text NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    result jsonb,
    error_msg text,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    resolved_at TIMESTAMPTZ,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

ALTER TABLE ONLY workflow_promises FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_routing (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    workflow_name text NOT NULL,
    target_version integer NOT NULL,
    weight real DEFAULT 1.0 NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    disabled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT workflow_routing_weight_check CHECK (((weight >= (0)::double precision) AND (weight <= (1)::double precision)))
);

ALTER TABLE ONLY workflow_routing FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_schedules (
    name text NOT NULL,
    def_name text NOT NULL,
    entry_point text DEFAULT ''::text NOT NULL,
    cron_expression text NOT NULL,
    input jsonb DEFAULT '{}'::jsonb NOT NULL,
    disabled_at TIMESTAMPTZ,
    next_run_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    last_run_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    timezone text DEFAULT 'UTC'::text NOT NULL,
    misfire_policy text DEFAULT 'catch_up'::text NOT NULL,
    catch_up_limit integer DEFAULT 60 NOT NULL,
    overlap_policy text DEFAULT 'allow'::text NOT NULL,
    last_run_id text,
    idempotency_key text,
    request_digest text,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT ck_schedules_catch_up_limit CHECK ((catch_up_limit >= 0)),
    CONSTRAINT ck_schedules_misfire_policy CHECK ((misfire_policy = ANY (ARRAY['catch_up'::text, 'skip'::text]))),
    CONSTRAINT ck_schedules_overlap_policy CHECK ((overlap_policy = ANY (ARRAY['allow'::text, 'skip'::text])))
);

ALTER TABLE ONLY workflow_schedules FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_signals (
    workflow_id text NOT NULL,
    signal_name text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    delivered_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    id bigint NOT NULL
);

ALTER TABLE ONLY workflow_signals FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_tags (
    workflow_name text NOT NULL,
    version integer NOT NULL,
    tag text NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    disabled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL
);

ALTER TABLE ONLY workflow_tags FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS workflow_update_requests (
    workflow_id text NOT NULL,
    update_name text NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    promise_id text,
    status text DEFAULT 'pending'::text NOT NULL,
    result jsonb,
    error_msg text,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    completed_at TIMESTAMPTZ,
    request_id text NOT NULL
);

ALTER TABLE ONLY workflow_update_requests FORCE ROW LEVEL SECURITY;

CREATE TABLE IF NOT EXISTS event_history_p0 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 0);

CREATE TABLE IF NOT EXISTS event_history_p1 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 1);

CREATE TABLE IF NOT EXISTS event_history_p2 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 2);

CREATE TABLE IF NOT EXISTS event_history_p3 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 3);

CREATE TABLE IF NOT EXISTS event_history_p4 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 4);

CREATE TABLE IF NOT EXISTS event_history_p5 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 5);

CREATE TABLE IF NOT EXISTS event_history_p6 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 6);

CREATE TABLE IF NOT EXISTS event_history_p7 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 7);

CREATE TABLE IF NOT EXISTS event_history_p8 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 8);

CREATE TABLE IF NOT EXISTS event_history_p9 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 9);

CREATE TABLE IF NOT EXISTS event_history_p10 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 10);

CREATE TABLE IF NOT EXISTS event_history_p11 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 11);

CREATE TABLE IF NOT EXISTS event_history_p12 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 12);

CREATE TABLE IF NOT EXISTS event_history_p13 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 13);

CREATE TABLE IF NOT EXISTS event_history_p14 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 14);

CREATE TABLE IF NOT EXISTS event_history_p15 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 15);

CREATE TABLE IF NOT EXISTS event_history_p16 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 16);

CREATE TABLE IF NOT EXISTS event_history_p17 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 17);

CREATE TABLE IF NOT EXISTS event_history_p18 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 18);

CREATE TABLE IF NOT EXISTS event_history_p19 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 19);

CREATE TABLE IF NOT EXISTS event_history_p20 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 20);

CREATE TABLE IF NOT EXISTS event_history_p21 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 21);

CREATE TABLE IF NOT EXISTS event_history_p22 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 22);

CREATE TABLE IF NOT EXISTS event_history_p23 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 23);

CREATE TABLE IF NOT EXISTS event_history_p24 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 24);

CREATE TABLE IF NOT EXISTS event_history_p25 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 25);

CREATE TABLE IF NOT EXISTS event_history_p26 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 26);

CREATE TABLE IF NOT EXISTS event_history_p27 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 27);

CREATE TABLE IF NOT EXISTS event_history_p28 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 28);

CREATE TABLE IF NOT EXISTS event_history_p29 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 29);

CREATE TABLE IF NOT EXISTS event_history_p30 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 30);

CREATE TABLE IF NOT EXISTS event_history_p31 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 31);

CREATE TABLE IF NOT EXISTS event_history_p32 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 32);

CREATE TABLE IF NOT EXISTS event_history_p33 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 33);

CREATE TABLE IF NOT EXISTS event_history_p34 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 34);

CREATE TABLE IF NOT EXISTS event_history_p35 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 35);

CREATE TABLE IF NOT EXISTS event_history_p36 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 36);

CREATE TABLE IF NOT EXISTS event_history_p37 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 37);

CREATE TABLE IF NOT EXISTS event_history_p38 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 38);

CREATE TABLE IF NOT EXISTS event_history_p39 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 39);

CREATE TABLE IF NOT EXISTS event_history_p40 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 40);

CREATE TABLE IF NOT EXISTS event_history_p41 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 41);

CREATE TABLE IF NOT EXISTS event_history_p42 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 42);

CREATE TABLE IF NOT EXISTS event_history_p43 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 43);

CREATE TABLE IF NOT EXISTS event_history_p44 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 44);

CREATE TABLE IF NOT EXISTS event_history_p45 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 45);

CREATE TABLE IF NOT EXISTS event_history_p46 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 46);

CREATE TABLE IF NOT EXISTS event_history_p47 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 47);

CREATE TABLE IF NOT EXISTS event_history_p48 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 48);

CREATE TABLE IF NOT EXISTS event_history_p49 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 49);

CREATE TABLE IF NOT EXISTS event_history_p50 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 50);

CREATE TABLE IF NOT EXISTS event_history_p51 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 51);

CREATE TABLE IF NOT EXISTS event_history_p52 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 52);

CREATE TABLE IF NOT EXISTS event_history_p53 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 53);

CREATE TABLE IF NOT EXISTS event_history_p54 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 54);

CREATE TABLE IF NOT EXISTS event_history_p55 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 55);

CREATE TABLE IF NOT EXISTS event_history_p56 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 56);

CREATE TABLE IF NOT EXISTS event_history_p57 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 57);

CREATE TABLE IF NOT EXISTS event_history_p58 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 58);

CREATE TABLE IF NOT EXISTS event_history_p59 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 59);

CREATE TABLE IF NOT EXISTS event_history_p60 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 60);

CREATE TABLE IF NOT EXISTS event_history_p61 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 61);

CREATE TABLE IF NOT EXISTS event_history_p62 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 62);

CREATE TABLE IF NOT EXISTS event_history_p63 PARTITION OF event_history
    FOR VALUES WITH (MODULUS 64, REMAINDER 63);

ALTER SEQUENCE workflow_memory_samples_id_seq OWNED BY workflow_memory_samples.id;

ALTER SEQUENCE workflow_signals_id_seq OWNED BY workflow_signals.id;

ALTER TABLE ONLY workflow_memory_samples ALTER COLUMN id SET DEFAULT nextval('workflow_memory_samples_id_seq'::regclass);

ALTER TABLE ONLY workflow_signals ALTER COLUMN id SET DEFAULT nextval('workflow_signals_id_seq'::regclass);

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'orgs_name_key'
                   AND conrelid = 'admin.orgs'::regclass) THEN
        ALTER TABLE ONLY admin.orgs
            ADD CONSTRAINT orgs_name_key UNIQUE (name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'orgs_pkey'
                   AND conrelid = 'admin.orgs'::regclass) THEN
        ALTER TABLE ONLY admin.orgs
            ADD CONSTRAINT orgs_pkey PRIMARY KEY (org_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugin_tables_pkey'
                   AND conrelid = 'admin.plugin_tables'::regclass) THEN
        ALTER TABLE ONLY admin.plugin_tables
            ADD CONSTRAINT plugin_tables_pkey PRIMARY KEY (plugin_name, schema_name, table_name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_api_keys_pkey'
                   AND conrelid = 'admin.tenant_api_keys'::regclass) THEN
        ALTER TABLE ONLY admin.tenant_api_keys
            ADD CONSTRAINT tenant_api_keys_pkey PRIMARY KEY (key_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_egress_allow_pkey'
                   AND conrelid = 'admin.tenant_egress_allow'::regclass) THEN
        ALTER TABLE ONLY admin.tenant_egress_allow
            ADD CONSTRAINT tenant_egress_allow_pkey PRIMARY KEY (tenant_id, host);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_roles_pkey'
                   AND conrelid = 'admin.tenant_roles'::regclass) THEN
        ALTER TABLE ONLY admin.tenant_roles
            ADD CONSTRAINT tenant_roles_pkey PRIMARY KEY (tenant_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_roles_role_name_key'
                   AND conrelid = 'admin.tenant_roles'::regclass) THEN
        ALTER TABLE ONLY admin.tenant_roles
            ADD CONSTRAINT tenant_roles_role_name_key UNIQUE (role_name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenants_name_key'
                   AND conrelid = 'admin.tenants'::regclass) THEN
        ALTER TABLE ONLY admin.tenants
            ADD CONSTRAINT tenants_name_key UNIQUE (name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenants_pkey'
                   AND conrelid = 'admin.tenants'::regclass) THEN
        ALTER TABLE ONLY admin.tenants
            ADD CONSTRAINT tenants_pkey PRIMARY KEY (tenant_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workers_pkey'
                   AND conrelid = 'admin.workers'::regclass) THEN
        ALTER TABLE ONLY admin.workers
            ADD CONSTRAINT workers_pkey PRIMARY KEY (worker_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'concurrency_keys_pkey'
                   AND conrelid = 'concurrency_keys'::regclass) THEN
        ALTER TABLE ONLY concurrency_keys
            ADD CONSTRAINT concurrency_keys_pkey PRIMARY KEY (key_hash, tenant_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'deployment_secrets_pkey'
                   AND conrelid = 'deployment_secrets'::regclass) THEN
        ALTER TABLE ONLY deployment_secrets
            ADD CONSTRAINT deployment_secrets_pkey PRIMARY KEY (name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'event_history_pkey'
                   AND conrelid = 'event_history'::regclass) THEN
        ALTER TABLE event_history
            ADD CONSTRAINT event_history_pkey PRIMARY KEY (tenant_id, workflow_id, step);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'idempotency_keys_pkey'
                   AND conrelid = 'idempotency_keys'::regclass) THEN
        ALTER TABLE ONLY idempotency_keys
            ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (key_hash, tenant_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugin_defs_pkey'
                   AND conrelid = 'plugin_defs'::regclass) THEN
        ALTER TABLE ONLY plugin_defs
            ADD CONSTRAINT plugin_defs_pkey PRIMARY KEY (name, version);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'queue_holders_pkey'
                   AND conrelid = 'queue_holders'::regclass) THEN
        ALTER TABLE ONLY queue_holders
            ADD CONSTRAINT queue_holders_pkey PRIMARY KEY (tenant_id, queue_name, workflow_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'queues_pkey'
                   AND conrelid = 'queues'::regclass) THEN
        ALTER TABLE ONLY queues
            ADD CONSTRAINT queues_pkey PRIMARY KEY (tenant_id, name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'slack_workspace_pkey'
                   AND conrelid = 'slack_workspace'::regclass) THEN
        ALTER TABLE ONLY slack_workspace
            ADD CONSTRAINT slack_workspace_pkey PRIMARY KEY (team_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_domains_pkey'
                   AND conrelid = 'tenant_domains'::regclass) THEN
        ALTER TABLE ONLY tenant_domains
            ADD CONSTRAINT tenant_domains_pkey PRIMARY KEY (hostname);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_secrets_pkey'
                   AND conrelid = 'tenant_secrets'::regclass) THEN
        ALTER TABLE ONLY tenant_secrets
            ADD CONSTRAINT tenant_secrets_pkey PRIMARY KEY (tenant_id, name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_settings_pkey'
                   AND conrelid = 'tenant_settings'::regclass) THEN
        ALTER TABLE ONLY tenant_settings
            ADD CONSTRAINT tenant_settings_pkey PRIMARY KEY (tenant_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_defs_pkey'
                   AND conrelid = 'workflow_defs'::regclass) THEN
        ALTER TABLE ONLY workflow_defs
            ADD CONSTRAINT workflow_defs_pkey PRIMARY KEY (tenant_id, name, version);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_instances_pkey'
                   AND conrelid = 'workflow_instances'::regclass) THEN
        ALTER TABLE ONLY workflow_instances
            ADD CONSTRAINT workflow_instances_pkey PRIMARY KEY (id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_memory_samples_pkey'
                   AND conrelid = 'workflow_memory_samples'::regclass) THEN
        ALTER TABLE ONLY workflow_memory_samples
            ADD CONSTRAINT workflow_memory_samples_pkey PRIMARY KEY (id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_memory_stats_pkey'
                   AND conrelid = 'workflow_memory_stats'::regclass) THEN
        ALTER TABLE ONLY workflow_memory_stats
            ADD CONSTRAINT workflow_memory_stats_pkey PRIMARY KEY (tenant_id, def_name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_promises_pkey'
                   AND conrelid = 'workflow_promises'::regclass) THEN
        ALTER TABLE ONLY workflow_promises
            ADD CONSTRAINT workflow_promises_pkey PRIMARY KEY (workflow_id, promise_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_routing_pkey'
                   AND conrelid = 'workflow_routing'::regclass) THEN
        ALTER TABLE ONLY workflow_routing
            ADD CONSTRAINT workflow_routing_pkey PRIMARY KEY (id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_schedules_pkey'
                   AND conrelid = 'workflow_schedules'::regclass) THEN
        ALTER TABLE ONLY workflow_schedules
            ADD CONSTRAINT workflow_schedules_pkey PRIMARY KEY (tenant_id, name);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_signals_pkey'
                   AND conrelid = 'workflow_signals'::regclass) THEN
        ALTER TABLE ONLY workflow_signals
            ADD CONSTRAINT workflow_signals_pkey PRIMARY KEY (id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_tags_pkey'
                   AND conrelid = 'workflow_tags'::regclass) THEN
        ALTER TABLE ONLY workflow_tags
            ADD CONSTRAINT workflow_tags_pkey PRIMARY KEY (tenant_id, workflow_name, tag);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_update_requests_pkey'
                   AND conrelid = 'workflow_update_requests'::regclass) THEN
        ALTER TABLE ONLY workflow_update_requests
            ADD CONSTRAINT workflow_update_requests_pkey PRIMARY KEY (workflow_id, request_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_api_keys_tenant_id_fkey'
                   AND conrelid = 'admin.tenant_api_keys'::regclass) THEN
        ALTER TABLE ONLY admin.tenant_api_keys
            ADD CONSTRAINT tenant_api_keys_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_egress_allow_tenant_id_fkey'
                   AND conrelid = 'admin.tenant_egress_allow'::regclass) THEN
        ALTER TABLE ONLY admin.tenant_egress_allow
            ADD CONSTRAINT tenant_egress_allow_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_roles_tenant_id_fkey'
                   AND conrelid = 'admin.tenant_roles'::regclass) THEN
        ALTER TABLE ONLY admin.tenant_roles
            ADD CONSTRAINT tenant_roles_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenants_org_id_fkey'
                   AND conrelid = 'admin.tenants'::regclass) THEN
        ALTER TABLE ONLY admin.tenants
            ADD CONSTRAINT tenants_org_id_fkey FOREIGN KEY (org_id) REFERENCES admin.orgs(org_id);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'concurrency_keys_workflow_id_fkey'
                   AND conrelid = 'concurrency_keys'::regclass) THEN
        ALTER TABLE ONLY concurrency_keys
            ADD CONSTRAINT concurrency_keys_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'queue_holders_workflow_id_fkey'
                   AND conrelid = 'queue_holders'::regclass) THEN
        ALTER TABLE ONLY queue_holders
            ADD CONSTRAINT queue_holders_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'queue_rate_tokens_tenant_id_fkey'
                   AND conrelid = 'queue_rate_tokens'::regclass) THEN
        ALTER TABLE ONLY queue_rate_tokens
            ADD CONSTRAINT queue_rate_tokens_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'queues_tenant_id_fkey'
                   AND conrelid = 'queues'::regclass) THEN
        ALTER TABLE ONLY queues
            ADD CONSTRAINT queues_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'slack_workspace_tenant_id_fkey'
                   AND conrelid = 'slack_workspace'::regclass) THEN
        ALTER TABLE ONLY slack_workspace
            ADD CONSTRAINT slack_workspace_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_domains_tenant_id_fkey'
                   AND conrelid = 'tenant_domains'::regclass) THEN
        ALTER TABLE ONLY tenant_domains
            ADD CONSTRAINT tenant_domains_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_secrets_tenant_id_fkey'
                   AND conrelid = 'tenant_secrets'::regclass) THEN
        ALTER TABLE ONLY tenant_secrets
            ADD CONSTRAINT tenant_secrets_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenant_settings_tenant_id_fkey'
                   AND conrelid = 'tenant_settings'::regclass) THEN
        ALTER TABLE ONLY tenant_settings
            ADD CONSTRAINT tenant_settings_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_instances_def_fkey'
                   AND conrelid = 'workflow_instances'::regclass) THEN
        ALTER TABLE ONLY workflow_instances
            ADD CONSTRAINT workflow_instances_def_fkey FOREIGN KEY (tenant_id, def_name, def_version) REFERENCES workflow_defs(tenant_id, name, version);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_promises_workflow_id_fkey'
                   AND conrelid = 'workflow_promises'::regclass) THEN
        ALTER TABLE ONLY workflow_promises
            ADD CONSTRAINT workflow_promises_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_routing_def_fkey'
                   AND conrelid = 'workflow_routing'::regclass) THEN
        ALTER TABLE ONLY workflow_routing
            ADD CONSTRAINT workflow_routing_def_fkey FOREIGN KEY (tenant_id, workflow_name, target_version) REFERENCES workflow_defs(tenant_id, name, version);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_signals_workflow_id_fkey'
                   AND conrelid = 'workflow_signals'::regclass) THEN
        ALTER TABLE ONLY workflow_signals
            ADD CONSTRAINT workflow_signals_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_tags_def_fkey'
                   AND conrelid = 'workflow_tags'::regclass) THEN
        ALTER TABLE ONLY workflow_tags
            ADD CONSTRAINT workflow_tags_def_fkey FOREIGN KEY (tenant_id, workflow_name, version) REFERENCES workflow_defs(tenant_id, name, version);
    END IF;
END $do$;

DO $do$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_update_requests_workflow_id_fkey'
                   AND conrelid = 'workflow_update_requests'::regclass) THEN
        ALTER TABLE ONLY workflow_update_requests
            ADD CONSTRAINT workflow_update_requests_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE;
    END IF;
END $do$;

CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON admin.tenant_api_keys USING btree (key_hash) WHERE (disabled_at IS NULL);

CREATE INDEX IF NOT EXISTS idx_workers_last_heartbeat ON admin.workers USING btree (last_heartbeat_at);

CREATE INDEX IF NOT EXISTS idx_concurrency_keys_expires ON concurrency_keys USING btree (expires_at);

CREATE INDEX IF NOT EXISTS idx_concurrency_keys_workflow ON concurrency_keys USING btree (workflow_id);

CREATE INDEX IF NOT EXISTS idx_defs_active ON workflow_defs USING btree (name, version DESC);

CREATE INDEX IF NOT EXISTS idx_defs_tenant_name_version ON workflow_defs USING btree (tenant_id, name, version DESC);

CREATE INDEX IF NOT EXISTS idx_event_history_pending ON event_history USING btree (workflow_id, step) WHERE ((intent_at IS NOT NULL) AND (checksum IS NULL));

CREATE INDEX IF NOT EXISTS idx_event_history_tenant_wf ON event_history USING btree (tenant_id, workflow_id, step);

CREATE INDEX IF NOT EXISTS idx_idempotency_expires ON idempotency_keys USING btree (expires_at);

CREATE INDEX IF NOT EXISTS idx_idempotency_workflow_id ON idempotency_keys USING btree (workflow_id);

CREATE INDEX IF NOT EXISTS idx_instances_claim_order ON workflow_instances USING btree (tenant_id, task_queue, priority, created_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));

CREATE INDEX IF NOT EXISTS idx_instances_claimable ON workflow_instances USING btree (status, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));

CREATE INDEX IF NOT EXISTS idx_instances_concurrency_key ON workflow_instances USING btree (tenant_id, concurrency_key_hash) WHERE (concurrency_key_hash IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_instances_created_at ON workflow_instances USING btree (tenant_id, created_at DESC);

DO $trgm$
DECLARE
    ext_schema text;
BEGIN
    SELECT n.nspname INTO ext_schema
      FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
     WHERE e.extname = 'pg_trgm';
    IF ext_schema IS NULL THEN
        RAISE EXCEPTION 'pg_trgm is not installed and CREATE EXTENSION IF NOT EXISTS did not create it';
    END IF;
    EXECUTE format('CREATE INDEX IF NOT EXISTS idx_instances_def_name_trgm ON workflow_instances USING GIN (def_name %I.gin_trgm_ops)',
                   ext_schema);
END
$trgm$;

DO $trgm$
DECLARE
    ext_schema text;
BEGIN
    SELECT n.nspname INTO ext_schema
      FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
     WHERE e.extname = 'pg_trgm';
    IF ext_schema IS NULL THEN
        RAISE EXCEPTION 'pg_trgm is not installed and CREATE EXTENSION IF NOT EXISTS did not create it';
    END IF;
    EXECUTE format('CREATE INDEX IF NOT EXISTS idx_instances_error_msg_trgm ON workflow_instances USING GIN (error_msg %I.gin_trgm_ops) WHERE (error_msg IS NOT NULL)',
                   ext_schema);
END
$trgm$;

CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances USING btree (assigned_to, heartbeat_at) WHERE (status = 'running'::text);

CREATE INDEX IF NOT EXISTS idx_instances_parent_policy ON workflow_instances USING btree (parent_workflow_id, parent_close_policy, status);

CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances USING btree (status, heartbeat_at) WHERE (status = 'running'::text);

CREATE INDEX IF NOT EXISTS idx_instances_sticky ON workflow_instances USING btree (sticky_worker_id) WHERE (sticky_worker_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_instances_tenant_claimable ON workflow_instances USING btree (tenant_id, status, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));

CREATE INDEX IF NOT EXISTS idx_instances_tenant_queue_claimable ON workflow_instances USING btree (tenant_id, task_queue, status, priority, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));

CREATE INDEX IF NOT EXISTS idx_instances_tenant_status_created ON workflow_instances USING btree (tenant_id, status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_instances_terminal_completed ON workflow_instances USING btree (tenant_id, status, completed_at) WHERE (status = ANY (ARRAY['done'::text, 'failed'::text, 'terminated'::text]));

CREATE INDEX IF NOT EXISTS idx_mem_samples_def ON workflow_memory_samples USING btree (def_name, recorded_at DESC);

CREATE INDEX IF NOT EXISTS idx_memory_samples_tenant_def ON workflow_memory_samples USING btree (tenant_id, def_name, recorded_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_promises_id_unique ON workflow_promises USING btree (tenant_id, promise_id);

CREATE INDEX IF NOT EXISTS idx_promises_status ON workflow_promises USING btree (workflow_id, status);

CREATE INDEX IF NOT EXISTS idx_queue_holders_expires ON queue_holders USING btree (expires_at);

CREATE INDEX IF NOT EXISTS idx_queue_holders_worker ON queue_holders USING btree (tenant_id, queue_name, worker_id, expires_at);

CREATE INDEX IF NOT EXISTS idx_queue_holders_workflow ON queue_holders USING btree (workflow_id);

CREATE INDEX IF NOT EXISTS idx_queue_rate_tokens_expires ON queue_rate_tokens USING btree (expires_at);

CREATE INDEX IF NOT EXISTS idx_queue_rate_tokens_window ON queue_rate_tokens USING btree (tenant_id, queue_name, expires_at);

CREATE INDEX IF NOT EXISTS idx_schedules_tenant_due ON workflow_schedules USING btree (tenant_id, next_run_at) WHERE (disabled_at IS NULL);

CREATE INDEX IF NOT EXISTS idx_signals_tenant_wf ON workflow_signals USING btree (tenant_id, workflow_id, signal_name);

CREATE INDEX IF NOT EXISTS idx_slack_workspace_tenant ON slack_workspace USING btree (tenant_id);

CREATE INDEX IF NOT EXISTS idx_tenant_domains_tenant ON tenant_domains USING btree (tenant_id);

CREATE INDEX IF NOT EXISTS idx_update_requests_pending ON workflow_update_requests USING btree (workflow_id, status);

CREATE INDEX IF NOT EXISTS idx_update_requests_pending_name ON workflow_update_requests USING btree (workflow_id, update_name, status);

CREATE INDEX IF NOT EXISTS idx_workflow_instances_continued_from ON workflow_instances USING btree (continued_from) WHERE (continued_from IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_workflow_instances_defer_phase_deadline ON workflow_instances USING btree (defer_phase_deadline) WHERE (pending_terminal_status IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_workflow_signals_queue ON workflow_signals USING btree (workflow_id, signal_name);

CREATE UNIQUE INDEX IF NOT EXISTS uq_workflow_schedules_idempotency_key ON workflow_schedules USING btree (tenant_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);

CREATE OR REPLACE FUNCTION admin.claim_workflows(p_worker_id text, p_task_queues text[], p_limit integer) RETURNS TABLE(id text, def_name text, def_version integer, status text, input jsonb, assigned_to text, next_wake_at TIMESTAMPTZ, tenant_id uuid, created_at TIMESTAMPTZ, error_code text, error_op text, generation bigint, priority integer, trace_id text, pending_terminal_status text)
    LANGUAGE sql SECURITY DEFINER
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

CREATE OR REPLACE FUNCTION admin.create_tenant_role(p_tenant_id uuid, p_password text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path FROM CURRENT
    AS $$
DECLARE
    v_role_name TEXT;
BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

            EXECUTE format(
                'CREATE ROLE %I WITH LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT CONNECTION LIMIT 10',
                v_role_name, p_password
            );
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot create role % (SQLSTATE: %) -- skipping (single-tenant mode)', v_role_name, SQLSTATE;
            RETURN NULL;
        END;
    ELSE
        BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

            EXECUTE format('ALTER ROLE %I WITH PASSWORD %L', v_role_name, p_password);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot alter password for role % (SQLSTATE: %)', v_role_name, SQLSTATE;
        END;
    END IF;

    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I AUTHORIZATION %I',
        'tenant_' || replace(p_tenant_id::text, '-', '_'), v_role_name);

    -- current_schema() rather than a literal: the schema cleat's tables live
    -- in is --schema's to choose (cleat#1287). Asking rather than stating is
    -- also what makes this work under every applier of these files, none of
    -- which substitutes anything.
    EXECUTE format('ALTER ROLE %I SET search_path = %L, %I', v_role_name,
        'tenant_' || replace(p_tenant_id::text, '-', '_'), current_schema());
    EXECUTE format('ALTER ROLE %I SET cleat.tenant_id = %L', v_role_name, p_tenant_id);

    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', current_schema(), v_role_name);
    EXECUTE format('GRANT USAGE ON SCHEMA admin TO %I', v_role_name);

    PERFORM admin.grant_core_tables_to_tenant_role(v_role_name);

    -- role_name only; the password column is dropped at the end of this file.
    INSERT INTO admin.tenant_roles (tenant_id, role_name)
    VALUES (p_tenant_id, v_role_name)
    ON CONFLICT (tenant_id) DO UPDATE SET role_name = EXCLUDED.role_name;

    RETURN v_role_name;
END;
$$;

CREATE OR REPLACE FUNCTION admin.drop_tenant(p_tenant_id uuid, p_schema text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
    AS $_$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_plugin_table RECORD;
    v_deleted BIGINT;
    v_core_table TEXT;
BEGIN
    IF p_schema IS NULL OR p_schema = '' THEN
        RAISE EXCEPTION 'admin.drop_tenant: p_schema is required -- it names the schema holding this deployment''s cleat tables, and must match the --schema cleat-worker was started with. Passing it is what stops the DELETEs resolving through the caller''s search_path (cleat#1363)';
    END IF;

    IF p_tenant_id = '00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'admin.drop_tenant: refusing to delete the default tenant (00000000-0000-0000-0000-000000000000) -- it is shared by every single-tenant deployment and by workflow_defs/plugin_defs, which are not tenant-owned data';
    END IF;

    IF to_regnamespace(p_schema) IS NULL THEN
        RAISE EXCEPTION 'admin.drop_tenant: schema % does not exist. Deleting nothing and reporting success is the failure this function was fixed to stop, so it refuses instead', p_schema;
    END IF;

    -- Makes every DELETE below correct regardless of whether the owning or
    -- executing role is a superuser.
    PERFORM set_config('cleat.tenant_id', p_tenant_id::text, true);

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    -- The seven core tables, in dependency order, each schema-qualified from
    -- p_schema. The order is 066's and is load bearing:
    --
    --   event_history       no FK/CASCADE back to workflow_instances (dropped
    --                       deliberately by 003_procedures.sql), so it must go
    --                       first or it is orphaned.
    --   workflow_instances  cascades workflow_signals, workflow_promises,
    --                       concurrency_keys and workflow_update_requests via
    --                       real ON DELETE CASCADE FKs.
    --   workflow_schedules  no FK to workflow_instances; own tenant_id.
    --   workflow_tags       FK to workflow_defs (NO ACTION), own tenant_id.
    --   workflow_routing    as workflow_tags.
    --   idempotency_keys    own tenant_id (010); result/error_msg hold a
    --                       workflow's actual output.
    --   workflow_defs       LAST of the seven: workflow_instances carries an FK
    --                       to it (NO ACTION), so it cannot be deleted while an
    --                       instance references it. By here there are none.
    --
    -- plugin_defs is deliberately absent: it has no tenant_id column at all
    -- (PRIMARY KEY (name, version)) and is genuinely shared.
    FOREACH v_core_table IN ARRAY ARRAY[
        'event_history',
        'workflow_instances',
        'workflow_schedules',
        'workflow_tags',
        'workflow_routing',
        'idempotency_keys',
        -- cleat#1644. Both carry tenant_id since 056 and neither has a foreign
        -- key to anything, so nothing cascaded them either: a dropped tenant's
        -- memory profile -- the names of the workflows it ran and how much
        -- memory each used -- survived indefinitely. Position in this array is
        -- free for exactly that reason; they are here rather than at the end so
        -- that workflow_defs stays visibly last.
        'workflow_memory_samples',
        'workflow_memory_stats',
        'workflow_defs'
    ] LOOP
        IF to_regclass(format('%I.%I', p_schema, v_core_table)) IS NULL THEN
            RAISE EXCEPTION 'admin.drop_tenant: %.% does not exist. Either p_schema is wrong or this deployment is not fully migrated; either way, continuing would delete some of this tenant''s data and report success', p_schema, v_core_table;
        END IF;
        EXECUTE format('DELETE FROM %I.%I WHERE tenant_id = $1', p_schema, v_core_table)
            USING p_tenant_id;
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        RAISE DEBUG 'admin.drop_tenant: deleted % row(s) from %.%',
            v_deleted, p_schema, v_core_table;
    END LOOP;

    -- cleat#1289. Every table a plugin declared TenantScoped, which is the same
    -- declaration that gave it a row-level security policy. Schema-qualified
    -- from the registry, which is where a plugin table's schema is recorded --
    -- NOT from p_schema, because a plugin table need not live in the same
    -- schema as the core tables.
    FOR v_plugin_table IN
        SELECT schema_name, table_name
        FROM admin.plugin_tables
        WHERE tenant_scoped
        ORDER BY schema_name, table_name
    LOOP
        -- A registry row can outlive its table: a plugin is removed from the
        -- build, or its migration is reversed, and nothing deletes the row.
        -- Without this guard the first such row makes admin.drop_tenant raise
        -- and TENANT DELETION STOPS WORKING ENTIRELY, for every tenant.
        --
        -- A warning rather than silence, because the other reading of a missing
        -- table is a registration that recorded the wrong schema -- in which
        -- case rows DO survive, and the skip is the only evidence.
        IF to_regclass(format('%I.%I', v_plugin_table.schema_name,
                              v_plugin_table.table_name)) IS NULL THEN
            RAISE WARNING 'admin.drop_tenant: admin.plugin_tables names %.%, which does not exist -- skipping. If that table does exist under another schema, this tenant''s rows in it are NOT deleted.',
                v_plugin_table.schema_name, v_plugin_table.table_name;
            CONTINUE;
        END IF;

        EXECUTE format('DELETE FROM %I.%I WHERE tenant_id = $1',
                       v_plugin_table.schema_name, v_plugin_table.table_name)
            USING p_tenant_id;
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        RAISE DEBUG 'admin.drop_tenant: deleted % row(s) from %.%',
            v_deleted, v_plugin_table.schema_name, v_plugin_table.table_name;
    END LOOP;

    -- Must precede the admin.tenants delete: admin.tenant_api_keys' FK to
    -- admin.tenants has no ON DELETE clause (NO ACTION), so deleting
    -- admin.tenants first fails for any tenant that ever had a key issued.
    DELETE FROM admin.tenant_api_keys WHERE tenant_id = p_tenant_id;

    -- DROP ROLE refuses to drop a role that still holds privileges anywhere,
    -- and that error aborts the whole function -- rolling back every DELETE
    -- above with it, since a single CALL is one transaction. DROP OWNED BY
    -- strips the grants first. Guarded on existence because DROP OWNED BY has
    -- no IF EXISTS form and the role may legitimately not exist (single-tenant
    -- mode warns and skips role creation).
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        EXECUTE format('DROP OWNED BY %I', v_role_name);
    END IF;
    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_schema_name);
    EXECUTE format('DROP ROLE IF EXISTS %I', v_role_name);

    DELETE FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    DELETE FROM admin.tenants WHERE tenant_id = p_tenant_id;
END;
$_$;

CREATE OR REPLACE FUNCTION admin.get_due_schedules() RETURNS TABLE(name text, def_name text, entry_point text, cron_expression text, input jsonb, disabled_at TIMESTAMPTZ, next_run_at TIMESTAMPTZ, last_run_at TIMESTAMPTZ, timezone text, tenant_id uuid, misfire_policy text, catch_up_limit integer, overlap_policy text, last_run_id text)
    LANGUAGE sql SECURITY DEFINER
    SET search_path FROM CURRENT
    AS $$
    SELECT s.name, s.def_name, s.entry_point, s.cron_expression, s.input,
           s.disabled_at, s.next_run_at, s.last_run_at, s.timezone, s.tenant_id,
           s.misfire_policy, s.catch_up_limit, s.overlap_policy,
           COALESCE(s.last_run_id, '')
    FROM workflow_schedules s
    WHERE s.disabled_at IS NULL
      AND s.next_run_at <= now()
    ORDER BY s.next_run_at;
$$;

CREATE OR REPLACE FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path FROM CURRENT
    AS $$
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
$$;

CREATE OR REPLACE FUNCTION admin.grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
    AS $$
DECLARE
    v_role_name TEXT;
    v_table RECORD;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'grant_plugin_to_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    -- schema_name from the registry, not 'tenant_' || uuid. See the header.
    FOR v_table IN
        SELECT t.schema_name, t.table_name
        FROM admin.plugin_tables t
        WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO %I',
            v_table.schema_name, v_table.table_name, v_role_name);
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION admin.in_flight_workflow_ids() RETURNS TABLE(id text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path FROM CURRENT
    AS $$
    SELECT w.id FROM workflow_instances w WHERE w.status IN ('ready', 'running');
$$;

CREATE OR REPLACE FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
    AS $$
DECLARE
    v_role_name TEXT;
    v_table RECORD;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'revoke_plugin_from_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    -- schema_name from the registry, not 'tenant_' || uuid. See the header.
    FOR v_table IN
        SELECT t.schema_name, t.table_name
        FROM admin.plugin_tables t
        WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('REVOKE ALL ON %I.%I FROM %I',
            v_table.schema_name, v_table.table_name, v_role_name);
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION admin.tenants_org_id_is_immutable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.org_id IS DISTINCT FROM OLD.org_id THEN
        RAISE EXCEPTION 'admin.tenants.org_id is immutable and cannot be changed (tenant_id=%, from org %  to org %)',
            OLD.tenant_id, OLD.org_id, NEW.org_id
            USING ERRCODE = '23514'; -- check_violation, the same code a CHECK constraint would raise
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION cleat.assert_tenant_set() RETURNS uuid
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    tid text;
BEGIN
    tid := current_setting('cleat.tenant_id', true);
    IF tid IS NULL OR tid = '' THEN
        RAISE EXCEPTION 'cleat.tenant_id is not set -- tenant context required for RLS-scoped query';
    END IF;
    RETURN tid::uuid;
END;
$$;

CREATE OR REPLACE FUNCTION cleat.tenant_row_is_visible(row_tenant uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE
        WHEN coalesce(current_setting('cleat.cross_tenant', true), '') <> ''
            THEN true
        ELSE row_tenant = cleat.assert_tenant_set()
    END
$$;


-- ── Function ownership ──────────────────────────────────────────────────────
-- `pg_dump --no-owner` was used to generate this file, so that the baseline does
-- not pin every object to the role that happened to run the dump. These three
-- are the exception, and they carry their ownership because it is not cosmetic:
-- a SECURITY DEFINER function executes with the OWNER's privileges, so the owner
-- IS the policy. cleat_dispatcher is the only role holding BYPASSRLS, which is
-- what these three need to read across tenants.
--
-- Guarded on the role existing, as 023/024/040/073 each are: BYPASSRLS needs a
-- superuser to grant, so a non-superuser deployment has no cleat_dispatcher and
-- the ALTER would fail the whole migration rather than skipping.
DO $do$ BEGIN
    -- ATTEMPTED, NOT GUARDED ON THE ROLE'S EXISTENCE, and that is 023's own
    -- finding rather than a preference. "Does cleat_dispatcher exist" is the
    -- wrong question: ALTER ... OWNER TO also requires the CURRENT ROLE to be a
    -- member of the target, so a role that exists but was created by somebody
    -- else still fails --
    --
    --   ERROR:  must be able to SET ROLE "cleat_dispatcher"   (SQLSTATE 42501)
    --
    -- and when the role does not exist at all the refusal is a DIFFERENT
    -- SQLSTATE -- undefined_object, 42704, not 42501 -- so both are caught.
    -- Catching only the first passes every run against a cluster where an
    -- earlier superuser run left the role behind, and fails the first genuinely
    -- clean one. Measured here: the guard-on-existence version failed
    -- TestTheMigrationSetAppliesWithoutASuperuser with exactly 42501.
    BEGIN
        EXECUTE 'ALTER FUNCTION admin.claim_workflows(text, text[], integer) OWNER TO cleat_dispatcher';
        EXECUTE 'ALTER FUNCTION admin.get_due_schedules() OWNER TO cleat_dispatcher';
        EXECUTE 'ALTER FUNCTION admin.in_flight_workflow_ids() OWNER TO cleat_dispatcher';
    EXCEPTION WHEN insufficient_privilege OR undefined_object THEN
        RAISE NOTICE 'cannot give the cross-tenant functions to cleat_dispatcher (SQLSTATE %); they keep the migrating role as their owner and will not see across tenants. Use --claim-strategy=rotate, which needs no exemption.', SQLSTATE;
    END;
END $do$;


ALTER TABLE concurrency_keys ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history ENABLE ROW LEVEL SECURITY;

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;

ALTER TABLE queues ENABLE ROW LEVEL SECURITY;

ALTER TABLE tenant_domains ENABLE ROW LEVEL SECURITY;

ALTER TABLE tenant_secrets ENABLE ROW LEVEL SECURITY;

ALTER TABLE tenant_settings ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_defs ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_instances ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_memory_samples ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_memory_stats ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_promises ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_routing ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_schedules ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_signals ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_tags ENABLE ROW LEVEL SECURITY;

ALTER TABLE workflow_update_requests ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p0 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p0 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p1 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p1 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p2 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p2 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p3 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p3 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p4 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p4 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p5 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p5 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p6 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p6 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p7 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p7 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p8 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p8 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p9 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p9 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p10 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p10 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p11 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p11 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p12 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p12 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p13 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p13 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p14 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p14 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p15 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p15 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p16 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p16 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p17 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p17 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p18 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p18 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p19 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p19 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p20 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p20 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p21 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p21 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p22 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p22 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p23 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p23 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p24 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p24 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p25 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p25 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p26 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p26 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p27 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p27 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p28 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p28 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p29 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p29 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p30 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p30 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p31 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p31 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p32 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p32 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p33 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p33 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p34 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p34 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p35 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p35 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p36 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p36 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p37 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p37 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p38 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p38 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p39 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p39 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p40 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p40 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p41 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p41 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p42 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p42 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p43 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p43 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p44 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p44 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p45 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p45 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p46 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p46 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p47 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p47 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p48 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p48 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p49 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p49 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p50 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p50 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p51 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p51 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p52 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p52 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p53 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p53 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p54 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p54 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p55 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p55 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p56 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p56 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p57 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p57 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p58 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p58 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p59 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p59 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p60 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p60 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p61 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p61 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p62 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p62 FORCE ROW LEVEL SECURITY;

ALTER TABLE event_history_p63 ENABLE ROW LEVEL SECURITY;

ALTER TABLE event_history_p63 FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS idempotency_keys_cross_tenant ON idempotency_keys;
CREATE POLICY idempotency_keys_cross_tenant ON idempotency_keys TO cleat_sweep USING (true);

DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_concurrency_keys ON concurrency_keys;
CREATE POLICY tenant_isolation_concurrency_keys ON concurrency_keys USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_defs ON workflow_defs;
CREATE POLICY tenant_isolation_defs ON workflow_defs USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_domains ON tenant_domains;
CREATE POLICY tenant_isolation_domains ON tenant_domains USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history;
CREATE POLICY tenant_isolation_events ON event_history USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_instances ON workflow_instances;
CREATE POLICY tenant_isolation_instances ON workflow_instances USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_memory_samples ON workflow_memory_samples;
CREATE POLICY tenant_isolation_memory_samples ON workflow_memory_samples USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_memory_stats ON workflow_memory_stats;
CREATE POLICY tenant_isolation_memory_stats ON workflow_memory_stats USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_promises ON workflow_promises;
CREATE POLICY tenant_isolation_promises ON workflow_promises USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_queues ON queues;
CREATE POLICY tenant_isolation_queues ON queues USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_routing ON workflow_routing;
CREATE POLICY tenant_isolation_routing ON workflow_routing USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_schedules ON workflow_schedules;
CREATE POLICY tenant_isolation_schedules ON workflow_schedules USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_secrets ON tenant_secrets;
CREATE POLICY tenant_isolation_secrets ON tenant_secrets USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_settings ON tenant_settings;
CREATE POLICY tenant_isolation_settings ON tenant_settings USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_signals ON workflow_signals;
CREATE POLICY tenant_isolation_signals ON workflow_signals USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_tags ON workflow_tags;
CREATE POLICY tenant_isolation_tags ON workflow_tags USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_update_requests ON workflow_update_requests;
CREATE POLICY tenant_isolation_update_requests ON workflow_update_requests USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p0;
CREATE POLICY tenant_isolation_events ON event_history_p0 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p1;
CREATE POLICY tenant_isolation_events ON event_history_p1 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p2;
CREATE POLICY tenant_isolation_events ON event_history_p2 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p3;
CREATE POLICY tenant_isolation_events ON event_history_p3 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p4;
CREATE POLICY tenant_isolation_events ON event_history_p4 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p5;
CREATE POLICY tenant_isolation_events ON event_history_p5 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p6;
CREATE POLICY tenant_isolation_events ON event_history_p6 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p7;
CREATE POLICY tenant_isolation_events ON event_history_p7 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p8;
CREATE POLICY tenant_isolation_events ON event_history_p8 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p9;
CREATE POLICY tenant_isolation_events ON event_history_p9 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p10;
CREATE POLICY tenant_isolation_events ON event_history_p10 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p11;
CREATE POLICY tenant_isolation_events ON event_history_p11 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p12;
CREATE POLICY tenant_isolation_events ON event_history_p12 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p13;
CREATE POLICY tenant_isolation_events ON event_history_p13 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p14;
CREATE POLICY tenant_isolation_events ON event_history_p14 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p15;
CREATE POLICY tenant_isolation_events ON event_history_p15 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p16;
CREATE POLICY tenant_isolation_events ON event_history_p16 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p17;
CREATE POLICY tenant_isolation_events ON event_history_p17 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p18;
CREATE POLICY tenant_isolation_events ON event_history_p18 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p19;
CREATE POLICY tenant_isolation_events ON event_history_p19 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p20;
CREATE POLICY tenant_isolation_events ON event_history_p20 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p21;
CREATE POLICY tenant_isolation_events ON event_history_p21 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p22;
CREATE POLICY tenant_isolation_events ON event_history_p22 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p23;
CREATE POLICY tenant_isolation_events ON event_history_p23 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p24;
CREATE POLICY tenant_isolation_events ON event_history_p24 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p25;
CREATE POLICY tenant_isolation_events ON event_history_p25 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p26;
CREATE POLICY tenant_isolation_events ON event_history_p26 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p27;
CREATE POLICY tenant_isolation_events ON event_history_p27 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p28;
CREATE POLICY tenant_isolation_events ON event_history_p28 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p29;
CREATE POLICY tenant_isolation_events ON event_history_p29 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p30;
CREATE POLICY tenant_isolation_events ON event_history_p30 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p31;
CREATE POLICY tenant_isolation_events ON event_history_p31 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p32;
CREATE POLICY tenant_isolation_events ON event_history_p32 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p33;
CREATE POLICY tenant_isolation_events ON event_history_p33 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p34;
CREATE POLICY tenant_isolation_events ON event_history_p34 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p35;
CREATE POLICY tenant_isolation_events ON event_history_p35 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p36;
CREATE POLICY tenant_isolation_events ON event_history_p36 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p37;
CREATE POLICY tenant_isolation_events ON event_history_p37 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p38;
CREATE POLICY tenant_isolation_events ON event_history_p38 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p39;
CREATE POLICY tenant_isolation_events ON event_history_p39 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p40;
CREATE POLICY tenant_isolation_events ON event_history_p40 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p41;
CREATE POLICY tenant_isolation_events ON event_history_p41 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p42;
CREATE POLICY tenant_isolation_events ON event_history_p42 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p43;
CREATE POLICY tenant_isolation_events ON event_history_p43 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p44;
CREATE POLICY tenant_isolation_events ON event_history_p44 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p45;
CREATE POLICY tenant_isolation_events ON event_history_p45 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p46;
CREATE POLICY tenant_isolation_events ON event_history_p46 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p47;
CREATE POLICY tenant_isolation_events ON event_history_p47 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p48;
CREATE POLICY tenant_isolation_events ON event_history_p48 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p49;
CREATE POLICY tenant_isolation_events ON event_history_p49 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p50;
CREATE POLICY tenant_isolation_events ON event_history_p50 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p51;
CREATE POLICY tenant_isolation_events ON event_history_p51 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p52;
CREATE POLICY tenant_isolation_events ON event_history_p52 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p53;
CREATE POLICY tenant_isolation_events ON event_history_p53 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p54;
CREATE POLICY tenant_isolation_events ON event_history_p54 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p55;
CREATE POLICY tenant_isolation_events ON event_history_p55 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p56;
CREATE POLICY tenant_isolation_events ON event_history_p56 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p57;
CREATE POLICY tenant_isolation_events ON event_history_p57 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p58;
CREATE POLICY tenant_isolation_events ON event_history_p58 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p59;
CREATE POLICY tenant_isolation_events ON event_history_p59 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p60;
CREATE POLICY tenant_isolation_events ON event_history_p60 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p61;
CREATE POLICY tenant_isolation_events ON event_history_p61 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p62;
CREATE POLICY tenant_isolation_events ON event_history_p62 USING ((tenant_id = cleat.assert_tenant_set()));

DROP POLICY IF EXISTS tenant_isolation_events ON event_history_p63;
CREATE POLICY tenant_isolation_events ON event_history_p63 USING ((tenant_id = cleat.assert_tenant_set()));

CREATE OR REPLACE TRIGGER tenants_org_id_immutable BEFORE UPDATE ON admin.tenants FOR EACH ROW EXECUTE FUNCTION admin.tenants_org_id_is_immutable();

GRANT USAGE ON SCHEMA admin TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT USAGE ON SCHEMA admin TO cleat_app;
GRANT USAGE ON SCHEMA admin TO cleat_sweep;

GRANT USAGE ON SCHEMA cleat TO cleat_app;
GRANT USAGE ON SCHEMA cleat TO cleat_sweep;

DO $do$ BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', current_schema(), 'cleat_tenant_00000000_0000_0000_0000_000000000000');
END $do$;
DO $do$ BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', current_schema(), 'cleat_app');
END $do$;
DO $do$ BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', current_schema(), 'cleat_dispatcher');
END $do$;
DO $do$ BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', current_schema(), 'cleat_sweep');
END $do$;

REVOKE ALL ON FUNCTION admin.claim_workflows(p_worker_id text, p_task_queues text[], p_limit integer) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.claim_workflows(p_worker_id text, p_task_queues text[], p_limit integer) TO cleat_app;

REVOKE ALL ON FUNCTION admin.create_tenant_role(p_tenant_id uuid, p_password text) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.create_tenant_role(p_tenant_id uuid, p_password text) TO cleat_app;

REVOKE ALL ON FUNCTION admin.drop_tenant(p_tenant_id uuid, p_schema text) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.drop_tenant(p_tenant_id uuid, p_schema text) TO cleat_app;

REVOKE ALL ON FUNCTION admin.get_due_schedules() FROM PUBLIC;
GRANT ALL ON FUNCTION admin.get_due_schedules() TO cleat_app;

REVOKE ALL ON FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) TO cleat_app;

REVOKE ALL ON FUNCTION admin.grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid) TO cleat_app;

REVOKE ALL ON FUNCTION admin.in_flight_workflow_ids() FROM PUBLIC;
GRANT ALL ON FUNCTION admin.in_flight_workflow_ids() TO cleat_app;
GRANT ALL ON FUNCTION admin.in_flight_workflow_ids() TO cleat_sweep;

REVOKE ALL ON FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid) TO cleat_app;

REVOKE ALL ON FUNCTION admin.tenants_org_id_is_immutable() FROM PUBLIC;
GRANT ALL ON FUNCTION admin.tenants_org_id_is_immutable() TO cleat_app;

GRANT ALL ON FUNCTION cleat.assert_tenant_set() TO cleat_app;
GRANT ALL ON FUNCTION cleat.assert_tenant_set() TO cleat_sweep;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.orgs TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.plugin_tables TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.tenant_api_keys TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.tenant_egress_allow TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.tenant_roles TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.tenants TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.workers TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE concurrency_keys TO cleat_app;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE concurrency_keys TO cleat_tenant_00000000_0000_0000_0000_000000000000;

GRANT SELECT ON TABLE deployment_secrets TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE event_history TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE event_history TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE idempotency_keys TO cleat_app;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE idempotency_keys TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT,DELETE ON TABLE idempotency_keys TO cleat_sweep;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE plugin_defs TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE queue_holders TO cleat_app;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE queue_holders TO cleat_tenant_00000000_0000_0000_0000_000000000000;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE queue_rate_tokens TO cleat_app;
GRANT SELECT,INSERT,DELETE ON TABLE queue_rate_tokens TO cleat_tenant_00000000_0000_0000_0000_000000000000;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE queues TO cleat_app;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE queues TO cleat_tenant_00000000_0000_0000_0000_000000000000;

GRANT SELECT ON TABLE slack_workspace TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE tenant_domains TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE tenant_secrets TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE tenant_settings TO cleat_app;
GRANT SELECT ON TABLE tenant_settings TO cleat_tenant_00000000_0000_0000_0000_000000000000;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_defs TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_defs TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_instances TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_instances TO cleat_app;
GRANT SELECT,UPDATE ON TABLE workflow_instances TO cleat_dispatcher;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_memory_samples TO cleat_app;

GRANT SELECT,USAGE ON SEQUENCE workflow_memory_samples_id_seq TO cleat_app;
GRANT USAGE ON SEQUENCE workflow_memory_samples_id_seq TO cleat_tenant_00000000_0000_0000_0000_000000000000;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_memory_stats TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_promises TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_promises TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_routing TO cleat_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_schedules TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_schedules TO cleat_app;
GRANT SELECT ON TABLE workflow_schedules TO cleat_dispatcher;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_signals TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_signals TO cleat_app;

GRANT SELECT,USAGE ON SEQUENCE workflow_signals_id_seq TO cleat_app;
GRANT USAGE ON SEQUENCE workflow_signals_id_seq TO cleat_tenant_00000000_0000_0000_0000_000000000000;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_tags TO cleat_app;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_tags TO cleat_tenant_00000000_0000_0000_0000_000000000000;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_update_requests TO cleat_app;
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE workflow_update_requests TO cleat_tenant_00000000_0000_0000_0000_000000000000;

DO $do$ BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relname = 'schema_migrations' AND n.nspname = current_schema()
    ) THEN
        EXECUTE format('GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE %I.schema_migrations TO cleat_app', current_schema());
    END IF;
END $do$;

ALTER DEFAULT PRIVILEGES IN SCHEMA admin GRANT ALL ON FUNCTIONS TO cleat_app;

ALTER DEFAULT PRIVILEGES IN SCHEMA admin GRANT SELECT,INSERT,DELETE,UPDATE ON TABLES TO cleat_app;

DO $do$ BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT SELECT,USAGE ON SEQUENCES TO cleat_app', current_schema());
END $do$;

DO $do$ BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT ALL ON FUNCTIONS TO cleat_app', current_schema());
END $do$;

DO $do$ BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT SELECT,INSERT,DELETE,UPDATE ON TABLES TO cleat_app', current_schema());
END $do$;


--
-- PostgreSQL database dump complete
--
