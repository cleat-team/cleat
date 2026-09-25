--
-- Name: admin; Type: SCHEMA; Schema: -; Owner: postgres
--

CREATE SCHEMA admin;


ALTER SCHEMA admin OWNER TO postgres;

--
-- Name: cleat; Type: SCHEMA; Schema: -; Owner: postgres
--

CREATE SCHEMA cleat;


ALTER SCHEMA cleat OWNER TO postgres;

--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;


--
-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: 
--

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';


--
-- Name: pgcrypto; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;


--
-- Name: EXTENSION pgcrypto; Type: COMMENT; Schema: -; Owner: 
--

COMMENT ON EXTENSION pgcrypto IS 'cryptographic functions';


-- ── Static roles ──────────────────────────────────────────────────────────
-- cleat_app, cleat_dispatcher and cleat_sweep: fixed roles every deployment
-- creates once, as opposed to admin.create_tenant_role's per-tenant roles
-- (003_procedures.sql), which are created and dropped as tenants come and
-- go. Guarded on existence: re-running this file against an already-built
-- database (a no-op migration re-apply) must not fail on a role that is
-- already there.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        -- NOBYPASSRLS is the default, but stating it makes the intent
        -- reviewable: this role must never be exempt from a policy.
        CREATE ROLE cleat_app
            NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END $$;

-- Creating a role with BYPASSRLS requires superuser. On managed PostgreSQL
-- (RDS, Cloud SQL, Azure) no true superuser connection exists, so this
-- degrades: cross-tenant dispatch (--claim-strategy=all-tenants) is
-- unavailable there, but --claim-strategy=rotate needs no such role and the
-- rest of the schema still applies cleanly. See
-- PostgresStore.CheckCrossTenantCapability, which already probes for and
-- words this exact condition.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        BEGIN
            CREATE ROLE cleat_dispatcher NOLOGIN BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'cleat_dispatcher needs BYPASSRLS, which only a superuser can grant, and this connection is not one. Cross-tenant claiming and cross-tenant schedule dispatch will not work on this deployment. (SQLSTATE %)', SQLSTATE;
        END;
    END IF;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_sweep') THEN
        CREATE ROLE cleat_sweep NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
    END IF;
END $$;

--
-- Name: assert_tenant_set(); Type: FUNCTION; Schema: cleat; Owner: postgres
--

CREATE FUNCTION cleat.assert_tenant_set() RETURNS uuid
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


ALTER FUNCTION cleat.assert_tenant_set() OWNER TO postgres;

--
-- Name: orgs; Type: TABLE; Schema: admin; Owner: postgres
--

CREATE TABLE admin.orgs (
    org_id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE admin.orgs OWNER TO postgres;

--
-- Name: TABLE orgs; Type: COMMENT; Schema: admin; Owner: postgres
--

COMMENT ON TABLE admin.orgs IS 'cleat#1898: identity only, by design -- no plan, limit or usage column. Billing systems key on org_id from outside; cleat knows WHO, not what they bought.';


--
-- Name: plugin_tables; Type: TABLE; Schema: admin; Owner: postgres
--

CREATE TABLE admin.plugin_tables (
    plugin_name text NOT NULL,
    table_name text NOT NULL,
    schema_name text DEFAULT "current_schema"() NOT NULL,
    tenant_scoped boolean DEFAULT false NOT NULL
);


ALTER TABLE admin.plugin_tables OWNER TO postgres;

--
-- Name: tenant_api_keys; Type: TABLE; Schema: admin; Owner: postgres
--

CREATE TABLE admin.tenant_api_keys (
    key_id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    key_hash bytea NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE admin.tenant_api_keys OWNER TO postgres;

--
-- Name: COLUMN tenant_api_keys.disabled_at; Type: COMMENT; Schema: admin; Owner: postgres
--

COMMENT ON COLUMN admin.tenant_api_keys.disabled_at IS 'cleat#1702: when this API key was retired. NULL = live. The single retirement spelling; revoked_at was dropped by migration 087 and this column is now the authority. Reversible: setting it back to NULL restores the key, which is what revoked_at always meant here.';


--
-- Name: COLUMN tenant_api_keys.updated_at; Type: COMMENT; Schema: admin; Owner: postgres
--

COMMENT ON COLUMN admin.tenant_api_keys.updated_at IS 'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';


--
-- Name: tenant_egress_allow; Type: TABLE; Schema: admin; Owner: postgres
--

CREATE TABLE admin.tenant_egress_allow (
    tenant_id uuid NOT NULL,
    host text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE admin.tenant_egress_allow OWNER TO postgres;

--
-- Name: COLUMN tenant_egress_allow.disabled_at; Type: COMMENT; Schema: admin; Owner: postgres
--

COMMENT ON COLUMN admin.tenant_egress_allow.disabled_at IS 'cleat#1702: when this entity was retired. NULL = live.';


--
-- Name: COLUMN tenant_egress_allow.updated_at; Type: COMMENT; Schema: admin; Owner: postgres
--

COMMENT ON COLUMN admin.tenant_egress_allow.updated_at IS 'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';


--
-- Name: tenant_roles; Type: TABLE; Schema: admin; Owner: postgres
--

CREATE TABLE admin.tenant_roles (
    tenant_id uuid NOT NULL,
    role_name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE admin.tenant_roles OWNER TO postgres;

--
-- Name: COLUMN tenant_roles.disabled_at; Type: COMMENT; Schema: admin; Owner: postgres
--

COMMENT ON COLUMN admin.tenant_roles.disabled_at IS 'cleat#1702: when this entity was retired. NULL = live.';


--
-- Name: COLUMN tenant_roles.updated_at; Type: COMMENT; Schema: admin; Owner: postgres
--

COMMENT ON COLUMN admin.tenant_roles.updated_at IS 'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';


--
-- Name: tenants; Type: TABLE; Schema: admin; Owner: postgres
--

CREATE TABLE admin.tenants (
    tenant_id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    suspended boolean DEFAULT false NOT NULL,
    org_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);


ALTER TABLE admin.tenants OWNER TO postgres;

--
-- Name: COLUMN tenants.org_id; Type: COMMENT; Schema: admin; Owner: postgres
--

COMMENT ON COLUMN admin.tenants.org_id IS 'cleat#1898: the org this tenant belongs to. IMMUTABLE -- enforced by the tenants_org_id_immutable trigger, not just this comment. Changing which org a tenant belongs to is a trust-boundary change (it decides who may reach the tenant''s org-scoped endpoints), not an attribute edit. An explicit, audited operator move operation is the answer when one is needed.';


--
-- Name: workers; Type: TABLE; Schema: admin; Owner: postgres
--

CREATE TABLE admin.workers (
    worker_id text NOT NULL,
    hostname text DEFAULT ''::text NOT NULL,
    pid integer DEFAULT 0 NOT NULL,
    concurrency integer DEFAULT 0 NOT NULL,
    connection_budget integer DEFAULT 0 NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    last_heartbeat_at timestamp with time zone DEFAULT now() NOT NULL,
    secret_key_versions text
);


ALTER TABLE admin.workers OWNER TO postgres;

--
-- Name: concurrency_keys; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.concurrency_keys (
    key_hash bytea NOT NULL,
    key_text text NOT NULL,
    workflow_id text NOT NULL,
    acquired_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

ALTER TABLE ONLY public.concurrency_keys FORCE ROW LEVEL SECURITY;


ALTER TABLE public.concurrency_keys OWNER TO postgres;

--
-- Name: deployment_secrets; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.deployment_secrets (
    name text NOT NULL,
    ciphertext text NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    disabled_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ck_deployment_secrets_ciphertext_nonempty CHECK ((length(ciphertext) > 0)),
    CONSTRAINT ck_deployment_secrets_name_charset CHECK ((name ~ '^[A-Za-z0-9_.-]{1,128}$'::text))
);


ALTER TABLE public.deployment_secrets OWNER TO postgres;

-- Name: event_history; Type: TABLE; Schema: public; Owner: postgres
--
-- cleat#2059. event_history is the one table Decision 2 partitions: unbounded
-- growth in workflows x steps, never read cross-tenant, and the table that
-- dominates size. HASH on tenant_id, 64 buckets (Decision 1) -- the primary
-- key moves from (workflow_id, step) to (tenant_id, workflow_id, step),
-- which is what makes HASH(tenant_id) a valid partition key at all (every
-- partitioning column must be part of any unique constraint on the table).
--
-- RLS on a partitioned PARENT does not reach its children -- measured in
-- docs/schema-partitioning-design.md, "RLS does not propagate to
-- partitions": ENABLE+FORCE+POLICY on event_history alone left every
-- eh_pN readable and writable direct, unfiltered, as the table owner. The
-- DO block below applies ENABLE, FORCE and the policy to each of the 64
-- partitions individually, which is the only thing that measurement found
-- to close the gap. Grants have the same non-propagation problem, and are
-- handled the same way inside admin.grant_core_tables_to_tenant_role
-- (003_procedures.sql) rather than here, because the role a partition is
-- granted to does not exist until a tenant is created.
--
-- Ordinary CREATE INDEX (no ONLY) on the parent DOES propagate to every
-- partition automatically, which is why idx_event_history_pending below
-- needs no such loop. The ONLY + CONCURRENTLY + ATTACH dance in the design
-- doc is for adding an index to an ALREADY POPULATED table without
-- blocking; it does not apply to a table being created empty.

CREATE TABLE public.event_history (
    workflow_id text NOT NULL,
    step integer NOT NULL,
    service text,
    operation text,
    request text,
    response text,
    error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
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
    intent_at timestamp with time zone,
    payload_encoding smallint,
    PRIMARY KEY (tenant_id, workflow_id, step)
) PARTITION BY HASH (tenant_id);

ALTER TABLE public.event_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.event_history FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_events ON public.event_history USING ((tenant_id = cleat.assert_tenant_set()));

-- 64 hash partitions, each with its own ENABLE + FORCE + policy -- see the
-- header comment above for why the parent-level statements just above are
-- not enough on their own.
DO $$
DECLARE
    i int;
BEGIN
    FOR i IN 0..63 LOOP
        EXECUTE format(
            'CREATE TABLE public.event_history_p%1$s PARTITION OF public.event_history FOR VALUES WITH (MODULUS 64, REMAINDER %1$s)',
            i);
        EXECUTE format('ALTER TABLE public.event_history_p%1$s ENABLE ROW LEVEL SECURITY', i);
        EXECUTE format('ALTER TABLE public.event_history_p%1$s FORCE ROW LEVEL SECURITY', i);
        EXECUTE format(
            'CREATE POLICY tenant_isolation_events ON public.event_history_p%1$s USING ((tenant_id = cleat.assert_tenant_set()))',
            i);
    END LOOP;
END $$;

COMMENT ON COLUMN public.event_history.payload_encoding IS 'How request/response are encoded: NULL = unknown (pre-cleat#1319 row, decode is a guess), 1 = base64, 0 = plaintext.';

--
-- Name: COLUMN event_history.payload_encoding; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.event_history.payload_encoding IS 'How request/response are encoded: NULL = unknown (pre-cleat#1319 row, decode is a guess), 1 = base64, 0 = plaintext.';


--
-- Name: idempotency_keys; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.idempotency_keys (
    key_hash bytea NOT NULL,
    workflow_id text NOT NULL,
    error_msg text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone DEFAULT (now() + '7 days'::interval) NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    def_name text,
    input_digest text
);

ALTER TABLE ONLY public.idempotency_keys FORCE ROW LEVEL SECURITY;


ALTER TABLE public.idempotency_keys OWNER TO postgres;

--
-- Name: plugin_defs; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.plugin_defs (
    name text NOT NULL,
    version text NOT NULL,
    wasm_bytes bytea,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deprecated boolean DEFAULT false NOT NULL
);


ALTER TABLE public.plugin_defs OWNER TO postgres;

--
-- Name: queue_holders; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.queue_holders (
    tenant_id uuid NOT NULL,
    queue_name text NOT NULL,
    workflow_id text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    worker_id text
);


ALTER TABLE public.queue_holders OWNER TO postgres;

--
-- Name: queue_rate_tokens; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.queue_rate_tokens (
    tenant_id uuid NOT NULL,
    queue_name text NOT NULL,
    workflow_id text NOT NULL,
    expires_at timestamp with time zone NOT NULL
);


ALTER TABLE public.queue_rate_tokens OWNER TO postgres;

--
-- Name: queues; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.queues (
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    concurrency_limit integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone,
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

ALTER TABLE ONLY public.queues FORCE ROW LEVEL SECURITY;


ALTER TABLE public.queues OWNER TO postgres;

--
-- Name: slack_workspace; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.slack_workspace (
    team_id text NOT NULL,
    tenant_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ck_slack_workspace_team_id_length CHECK ((length(team_id) <= 32)),
    CONSTRAINT ck_slack_workspace_team_id_shape CHECK ((team_id ~ '^[TE][A-Z0-9]+$'::text))
);


ALTER TABLE public.slack_workspace OWNER TO postgres;

--
-- Name: tenant_domains; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.tenant_domains (
    hostname text NOT NULL,
    tenant_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ck_tenant_domains_hostname_lowercase CHECK ((hostname = lower(hostname))),
    CONSTRAINT ck_tenant_domains_hostname_no_port CHECK ((POSITION((':'::text) IN (hostname)) = 0)),
    CONSTRAINT ck_tenant_domains_hostname_nonempty CHECK ((length(hostname) > 0))
);

ALTER TABLE ONLY public.tenant_domains FORCE ROW LEVEL SECURITY;


ALTER TABLE public.tenant_domains OWNER TO postgres;

--
-- Name: COLUMN tenant_domains.disabled_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.tenant_domains.disabled_at IS 'cleat#1702: when this entity was retired. NULL = live.';


--
-- Name: COLUMN tenant_domains.updated_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.tenant_domains.updated_at IS 'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';


--
-- Name: tenant_secrets; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.tenant_secrets (
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    ciphertext text NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ck_tenant_secrets_ciphertext_nonempty CHECK ((length(ciphertext) > 0)),
    CONSTRAINT ck_tenant_secrets_name_charset CHECK ((name ~ '^[A-Za-z0-9_.-]{1,128}$'::text))
);

ALTER TABLE ONLY public.tenant_secrets FORCE ROW LEVEL SECURITY;


ALTER TABLE public.tenant_secrets OWNER TO postgres;

--
-- Name: COLUMN tenant_secrets.disabled_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.tenant_secrets.disabled_at IS 'cleat#1702: when this entity was retired. NULL = live.';


--
-- Name: tenant_settings; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.tenant_settings (
    tenant_id uuid NOT NULL,
    wasm_instance_timeout_ms bigint,
    wasm_wall_clock_ceiling_ms bigint,
    host_retry_budget_ms bigint,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    max_workflow_duration_ms bigint,
    disabled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ck_tenant_settings_instance_timeout_positive CHECK (((wasm_instance_timeout_ms IS NULL) OR (wasm_instance_timeout_ms > 0))),
    CONSTRAINT ck_tenant_settings_retry_budget_positive CHECK (((host_retry_budget_ms IS NULL) OR (host_retry_budget_ms > 0))),
    CONSTRAINT ck_tenant_settings_wall_clock_positive CHECK (((wasm_wall_clock_ceiling_ms IS NULL) OR (wasm_wall_clock_ceiling_ms > 0))),
    CONSTRAINT ck_ts_max_workflow_duration_positive CHECK (((max_workflow_duration_ms IS NULL) OR (max_workflow_duration_ms > 0)))
);

ALTER TABLE ONLY public.tenant_settings FORCE ROW LEVEL SECURITY;


ALTER TABLE public.tenant_settings OWNER TO postgres;

--
-- Name: COLUMN tenant_settings.max_workflow_duration_ms; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.tenant_settings.max_workflow_duration_ms IS 'cleat#1117: this tenant''s bound on WHOLE-workflow wall clock, clamped to --max-workflow-duration. NULL = no override.';


--
-- Name: COLUMN tenant_settings.disabled_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.tenant_settings.disabled_at IS 'cleat#1702: when this entity was retired. NULL = live.';


--
-- Name: workflow_defs; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_defs (
    name text NOT NULL,
    version integer NOT NULL,
    wasm_bytes bytea NOT NULL,
    entry_points text[] DEFAULT '{}'::text[] NOT NULL,
    min_version integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    max_history_length integer DEFAULT 0 NOT NULL,
    dag_spec jsonb,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    task_queue text DEFAULT 'default'::text NOT NULL,
    abi_version integer DEFAULT 1 NOT NULL,
    plugin_deps jsonb DEFAULT '{}'::jsonb NOT NULL,
    disabled_at timestamp with time zone,
    gc_eligible boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.workflow_defs FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_defs OWNER TO postgres;

--
-- Name: COLUMN workflow_defs.disabled_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_defs.disabled_at IS 'cleat#1702: ADMISSION CONTROL. NULL = live. A disabled version cannot be started, routed to, tagged, or chosen for a child. It does NOT make the version collectable -- that is gc_eligible, deliberately separate, because a generic entity helper writing this column must not arm a permanent deletion. Backfilled from the former `deprecated` boolean at migration 088, where the value is an upper bound: nothing recorded when.';


--
-- Name: COLUMN workflow_defs.gc_eligible; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_defs.gc_eligible IS 'cleat#1702: COLLECTION ELIGIBILITY, and nothing else. true = version_gc.go may permanently delete this version once it is older than --version-gc-max-age. Named for the mechanism rather than the lifecycle so that nothing reaches for it by analogy with the other entity members. Equality with disabled_at is INCIDENTAL -- cleatctl versions deprecate writes both, and is the only shipped writer of either -- not an invariant, and not a reason to merge them.';


--
-- Name: COLUMN workflow_defs.updated_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_defs.updated_at IS 'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';


--
-- Name: workflow_instances; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_instances (
    id text NOT NULL,
    def_name text NOT NULL,
    def_version integer NOT NULL,
    status text DEFAULT 'ready'::text NOT NULL,
    input jsonb DEFAULT '{}'::jsonb NOT NULL,
    assigned_to text,
    heartbeat_at timestamp with time zone,
    next_wake_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
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
    compacted_at timestamp with time zone,
    compaction_step integer,
    plugin_vers jsonb DEFAULT '{}'::jsonb NOT NULL,
    event_count bigint DEFAULT 0 NOT NULL,
    allowed_signals jsonb,
    priority integer DEFAULT 0 NOT NULL,
    generation bigint DEFAULT 0 NOT NULL,
    pending_terminal_status text,
    defer_phase_deadline timestamp with time zone,
    continued_from text,
    signal_seq bigint DEFAULT 0 NOT NULL,
    signal_seq_at_claim bigint DEFAULT 0 NOT NULL,
    signal_consumed_seq bigint DEFAULT 0 NOT NULL,
    signal_consumed_at_claim bigint DEFAULT 0 NOT NULL,
    reclaim_count bigint DEFAULT 0 NOT NULL,
    started_at timestamp with time zone,
    concurrency_key text,
    concurrency_key_hash bytea,
    run_wasm_instance_timeout_ms bigint,
    run_wasm_wall_clock_ceiling_ms bigint,
    run_host_retry_budget_ms bigint,
    completed_by text,
    run_max_workflow_duration_ms bigint,
    history_swept_at timestamp with time zone,
    CONSTRAINT ck_wi_run_instance_timeout_positive CHECK (((run_wasm_instance_timeout_ms IS NULL) OR (run_wasm_instance_timeout_ms > 0))),
    CONSTRAINT ck_wi_run_max_workflow_duration_positive CHECK (((run_max_workflow_duration_ms IS NULL) OR (run_max_workflow_duration_ms > 0))),
    CONSTRAINT ck_wi_run_retry_budget_positive CHECK (((run_host_retry_budget_ms IS NULL) OR (run_host_retry_budget_ms > 0))),
    CONSTRAINT ck_wi_run_wall_clock_positive CHECK (((run_wasm_wall_clock_ceiling_ms IS NULL) OR (run_wasm_wall_clock_ceiling_ms > 0)))
);

ALTER TABLE ONLY public.workflow_instances FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_instances OWNER TO postgres;

--
-- Name: COLUMN workflow_instances.concurrency_key; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_instances.concurrency_key IS 'The Cleat-Concurrency-Key this run was started with, or NULL. Recorded so the dispatcher can test it at claim time: a run whose key is held by another live run is not claimable and waits, rather than being rejected at the request boundary. See cleat#1186.';


--
-- Name: COLUMN workflow_instances.run_wasm_instance_timeout_ms; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_instances.run_wasm_instance_timeout_ms IS 'cleat#1187: this run''s own bound on guest execution, clamped to the tenant setting and then to the operator flag. NULL = no override.';


--
-- Name: COLUMN workflow_instances.run_wasm_wall_clock_ceiling_ms; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_instances.run_wasm_wall_clock_ceiling_ms IS 'cleat#1187: this run''s own wall-clock ceiling. NULL = no override.';


--
-- Name: COLUMN workflow_instances.run_host_retry_budget_ms; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_instances.run_host_retry_budget_ms IS 'cleat#1187: this run''s own host-retry budget. NULL = no override.';


--
-- Name: COLUMN workflow_instances.run_max_workflow_duration_ms; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_instances.run_max_workflow_duration_ms IS 'cleat#1117: this run''s own bound on WHOLE-workflow wall clock, clamped to the tenant setting and then the operator flag. NULL = no override.';


--
-- Name: workflow_memory_samples; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_memory_samples (
    id bigint NOT NULL,
    def_name text NOT NULL,
    sample_bytes bigint NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

ALTER TABLE ONLY public.workflow_memory_samples FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_memory_samples OWNER TO postgres;

--
-- Name: workflow_memory_samples_id_seq; Type: SEQUENCE; Schema: public; Owner: postgres
--

CREATE SEQUENCE public.workflow_memory_samples_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


ALTER SEQUENCE public.workflow_memory_samples_id_seq OWNER TO postgres;

--
-- Name: workflow_memory_samples_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: postgres
--

ALTER SEQUENCE public.workflow_memory_samples_id_seq OWNED BY public.workflow_memory_samples.id;


--
-- Name: workflow_memory_stats; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_memory_stats (
    def_name text NOT NULL,
    mean_bytes double precision DEFAULT 0 NOT NULL,
    sample_count integer DEFAULT 0 NOT NULL,
    alpha double precision DEFAULT 0.3 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

ALTER TABLE ONLY public.workflow_memory_stats FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_memory_stats OWNER TO postgres;

--
-- Name: workflow_promises; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_promises (
    workflow_id text NOT NULL,
    promise_id text NOT NULL,
    promise_name text NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    result jsonb,
    error_msg text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL
);

ALTER TABLE ONLY public.workflow_promises FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_promises OWNER TO postgres;

--
-- Name: workflow_routing; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_routing (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    workflow_name text NOT NULL,
    target_version integer NOT NULL,
    weight real DEFAULT 1.0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    disabled_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workflow_routing_weight_check CHECK (((weight >= (0)::double precision) AND (weight <= (1)::double precision)))
);

ALTER TABLE ONLY public.workflow_routing FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_routing OWNER TO postgres;

--
-- Name: COLUMN workflow_routing.disabled_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_routing.disabled_at IS 'cleat#1702: when this entity was retired. NULL = live.';


--
-- Name: COLUMN workflow_routing.updated_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_routing.updated_at IS 'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';


--
-- Name: workflow_schedules; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_schedules (
    name text NOT NULL,
    def_name text NOT NULL,
    entry_point text DEFAULT ''::text NOT NULL,
    cron_expression text NOT NULL,
    input jsonb DEFAULT '{}'::jsonb NOT NULL,
    disabled_at timestamp with time zone,
    next_run_at timestamp with time zone DEFAULT now() NOT NULL,
    last_run_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    timezone text DEFAULT 'UTC'::text NOT NULL,
    misfire_policy text DEFAULT 'catch_up'::text NOT NULL,
    catch_up_limit integer DEFAULT 60 NOT NULL,
    overlap_policy text DEFAULT 'allow'::text NOT NULL,
    last_run_id text,
    idempotency_key text,
    request_digest text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ck_schedules_catch_up_limit CHECK ((catch_up_limit >= 0)),
    CONSTRAINT ck_schedules_misfire_policy CHECK ((misfire_policy = ANY (ARRAY['catch_up'::text, 'skip'::text]))),
    CONSTRAINT ck_schedules_overlap_policy CHECK ((overlap_policy = ANY (ARRAY['allow'::text, 'skip'::text])))
);

ALTER TABLE ONLY public.workflow_schedules FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_schedules OWNER TO postgres;

--
-- Name: COLUMN workflow_schedules.disabled_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_schedules.disabled_at IS 'cleat#1702: when this schedule was retired. NULL = live, and a live schedule is the only kind admin.get_due_schedules() and GetDueSchedules return. Replaced the `enabled` BOOLEAN at migration 089, the one conversion in the class that inverted polarity -- enabled=true meant LIVE. Backfilled values are an UPPER BOUND: a boolean recorded that a schedule was disabled and never when.';


--
-- Name: COLUMN workflow_schedules.updated_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_schedules.updated_at IS 'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';


--
-- Name: workflow_signals; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_signals (
    workflow_id text NOT NULL,
    signal_name text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    delivered_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    id bigint NOT NULL
);

ALTER TABLE ONLY public.workflow_signals FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_signals OWNER TO postgres;

--
-- Name: workflow_signals_id_seq; Type: SEQUENCE; Schema: public; Owner: postgres
--

CREATE SEQUENCE public.workflow_signals_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


ALTER SEQUENCE public.workflow_signals_id_seq OWNER TO postgres;

--
-- Name: workflow_signals_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: postgres
--

ALTER SEQUENCE public.workflow_signals_id_seq OWNED BY public.workflow_signals.id;


--
-- Name: workflow_tags; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_tags (
    workflow_name text NOT NULL,
    version integer NOT NULL,
    tag text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    disabled_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.workflow_tags FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_tags OWNER TO postgres;

--
-- Name: COLUMN workflow_tags.disabled_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_tags.disabled_at IS 'cleat#1702: when this entity was retired. NULL = live.';


--
-- Name: COLUMN workflow_tags.updated_at; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON COLUMN public.workflow_tags.updated_at IS 'cleat#1702: when this row last changed. Backfilled from created_at; no writer maintains it yet.';


--
-- Name: workflow_update_requests; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.workflow_update_requests (
    workflow_id text NOT NULL,
    update_name text NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    promise_id text,
    status text DEFAULT 'pending'::text NOT NULL,
    result jsonb,
    error_msg text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    request_id text NOT NULL
);

ALTER TABLE ONLY public.workflow_update_requests FORCE ROW LEVEL SECURITY;


ALTER TABLE public.workflow_update_requests OWNER TO postgres;

--
-- Name: workflow_memory_samples id; Type: DEFAULT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_memory_samples ALTER COLUMN id SET DEFAULT nextval('public.workflow_memory_samples_id_seq'::regclass);


--
-- Name: workflow_signals id; Type: DEFAULT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_signals ALTER COLUMN id SET DEFAULT nextval('public.workflow_signals_id_seq'::regclass);


--
-- Name: orgs orgs_name_key; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.orgs
    ADD CONSTRAINT orgs_name_key UNIQUE (name);


--
-- Name: orgs orgs_pkey; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.orgs
    ADD CONSTRAINT orgs_pkey PRIMARY KEY (org_id);


--
-- Name: plugin_tables plugin_tables_pkey; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.plugin_tables
    ADD CONSTRAINT plugin_tables_pkey PRIMARY KEY (plugin_name, schema_name, table_name);


--
-- Name: tenant_api_keys tenant_api_keys_pkey; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenant_api_keys
    ADD CONSTRAINT tenant_api_keys_pkey PRIMARY KEY (key_id);


--
-- Name: tenant_egress_allow tenant_egress_allow_pkey; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenant_egress_allow
    ADD CONSTRAINT tenant_egress_allow_pkey PRIMARY KEY (tenant_id, host);


--
-- Name: tenant_roles tenant_roles_pkey; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenant_roles
    ADD CONSTRAINT tenant_roles_pkey PRIMARY KEY (tenant_id);


--
-- Name: tenant_roles tenant_roles_role_name_key; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenant_roles
    ADD CONSTRAINT tenant_roles_role_name_key UNIQUE (role_name);


--
-- Name: tenants tenants_name_key; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenants
    ADD CONSTRAINT tenants_name_key UNIQUE (name);


--
-- Name: tenants tenants_pkey; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenants
    ADD CONSTRAINT tenants_pkey PRIMARY KEY (tenant_id);


--
-- Name: workers workers_pkey; Type: CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.workers
    ADD CONSTRAINT workers_pkey PRIMARY KEY (worker_id);


--
-- Name: concurrency_keys concurrency_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.concurrency_keys
    ADD CONSTRAINT concurrency_keys_pkey PRIMARY KEY (key_hash, tenant_id);


--
-- Name: deployment_secrets deployment_secrets_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.deployment_secrets
    ADD CONSTRAINT deployment_secrets_pkey PRIMARY KEY (name);


--
-- Name: idempotency_keys idempotency_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (key_hash, tenant_id);


--
-- Name: plugin_defs plugin_defs_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.plugin_defs
    ADD CONSTRAINT plugin_defs_pkey PRIMARY KEY (name, version);


--
-- Name: queue_holders queue_holders_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.queue_holders
    ADD CONSTRAINT queue_holders_pkey PRIMARY KEY (tenant_id, queue_name, workflow_id);


--
-- Name: queues queues_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.queues
    ADD CONSTRAINT queues_pkey PRIMARY KEY (tenant_id, name);


--
-- Name: slack_workspace slack_workspace_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.slack_workspace
    ADD CONSTRAINT slack_workspace_pkey PRIMARY KEY (team_id);


--
-- Name: tenant_domains tenant_domains_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.tenant_domains
    ADD CONSTRAINT tenant_domains_pkey PRIMARY KEY (hostname);


--
-- Name: tenant_secrets tenant_secrets_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.tenant_secrets
    ADD CONSTRAINT tenant_secrets_pkey PRIMARY KEY (tenant_id, name);


--
-- Name: tenant_settings tenant_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.tenant_settings
    ADD CONSTRAINT tenant_settings_pkey PRIMARY KEY (tenant_id);


--
-- Name: workflow_defs workflow_defs_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_defs
    ADD CONSTRAINT workflow_defs_pkey PRIMARY KEY (tenant_id, name, version);


--
-- Name: workflow_instances workflow_instances_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_instances
    ADD CONSTRAINT workflow_instances_pkey PRIMARY KEY (id);


--
-- Name: workflow_memory_samples workflow_memory_samples_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_memory_samples
    ADD CONSTRAINT workflow_memory_samples_pkey PRIMARY KEY (id);


--
-- Name: workflow_memory_stats workflow_memory_stats_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_memory_stats
    ADD CONSTRAINT workflow_memory_stats_pkey PRIMARY KEY (tenant_id, def_name);


--
-- Name: workflow_promises workflow_promises_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_promises
    ADD CONSTRAINT workflow_promises_pkey PRIMARY KEY (workflow_id, promise_id);


--
-- Name: workflow_routing workflow_routing_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_routing
    ADD CONSTRAINT workflow_routing_pkey PRIMARY KEY (id);


--
-- Name: workflow_schedules workflow_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_schedules
    ADD CONSTRAINT workflow_schedules_pkey PRIMARY KEY (tenant_id, name);


--
-- Name: workflow_signals workflow_signals_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_signals
    ADD CONSTRAINT workflow_signals_pkey PRIMARY KEY (id);


--
-- Name: workflow_tags workflow_tags_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_tags
    ADD CONSTRAINT workflow_tags_pkey PRIMARY KEY (tenant_id, workflow_name, tag);


--
-- Name: workflow_update_requests workflow_update_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_update_requests
    ADD CONSTRAINT workflow_update_requests_pkey PRIMARY KEY (workflow_id, request_id);


--
-- Name: idx_api_keys_hash; Type: INDEX; Schema: admin; Owner: postgres
--

CREATE INDEX idx_api_keys_hash ON admin.tenant_api_keys USING btree (key_hash) WHERE (disabled_at IS NULL);


--
-- Name: idx_workers_last_heartbeat; Type: INDEX; Schema: admin; Owner: postgres
--

CREATE INDEX idx_workers_last_heartbeat ON admin.workers USING btree (last_heartbeat_at);


--
-- Name: idx_concurrency_keys_expires; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_concurrency_keys_expires ON public.concurrency_keys USING btree (expires_at);


--
-- Name: idx_concurrency_keys_workflow; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_concurrency_keys_workflow ON public.concurrency_keys USING btree (workflow_id);


--
-- Name: idx_defs_active; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_defs_active ON public.workflow_defs USING btree (name, version DESC);


--
-- Name: idx_defs_tenant_name_version; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_defs_tenant_name_version ON public.workflow_defs USING btree (tenant_id, name, version DESC);


--
-- Name: idx_event_history_pending; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_event_history_pending ON public.event_history USING btree (workflow_id, step) WHERE ((intent_at IS NOT NULL) AND (checksum IS NULL));


--
-- Name: idx_idempotency_expires; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_idempotency_expires ON public.idempotency_keys USING btree (expires_at);


--
-- Name: idx_idempotency_workflow_id; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_idempotency_workflow_id ON public.idempotency_keys USING btree (workflow_id);


--
-- Name: idx_instances_claim_order; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_claim_order ON public.workflow_instances USING btree (tenant_id, task_queue, priority, created_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));


--
-- Name: idx_instances_claimable; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_claimable ON public.workflow_instances USING btree (status, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));


--
-- Name: idx_instances_concurrency_key; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_concurrency_key ON public.workflow_instances USING btree (tenant_id, concurrency_key_hash) WHERE (concurrency_key_hash IS NOT NULL);


--
-- Name: idx_instances_created_at; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_created_at ON public.workflow_instances USING btree (tenant_id, created_at DESC);


--
-- Name: idx_instances_def_name_trgm; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_def_name_trgm ON public.workflow_instances USING gin (def_name public.gin_trgm_ops);


--
-- Name: idx_instances_error_msg_trgm; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_error_msg_trgm ON public.workflow_instances USING gin (error_msg public.gin_trgm_ops) WHERE (error_msg IS NOT NULL);


--
-- Name: idx_instances_heartbeat; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_heartbeat ON public.workflow_instances USING btree (assigned_to, heartbeat_at) WHERE (status = 'running'::text);


--
-- Name: idx_instances_parent_policy; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_parent_policy ON public.workflow_instances USING btree (parent_workflow_id, parent_close_policy, status);


--
-- Name: idx_instances_stale; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_stale ON public.workflow_instances USING btree (status, heartbeat_at) WHERE (status = 'running'::text);


--
-- Name: idx_instances_sticky; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_sticky ON public.workflow_instances USING btree (sticky_worker_id) WHERE (sticky_worker_id IS NOT NULL);


--
-- Name: idx_instances_tenant_claimable; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_tenant_claimable ON public.workflow_instances USING btree (tenant_id, status, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));


--
-- Name: idx_instances_tenant_queue_claimable; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_tenant_queue_claimable ON public.workflow_instances USING btree (tenant_id, task_queue, status, priority, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));


--
-- Name: idx_instances_tenant_status_created; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_tenant_status_created ON public.workflow_instances USING btree (tenant_id, status, created_at DESC);


--
-- Name: idx_instances_terminal_completed; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_instances_terminal_completed ON public.workflow_instances USING btree (tenant_id, status, completed_at) WHERE (status = ANY (ARRAY['done'::text, 'failed'::text, 'terminated'::text]));


--
-- Name: idx_mem_samples_def; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_mem_samples_def ON public.workflow_memory_samples USING btree (def_name, recorded_at DESC);


--
-- Name: idx_memory_samples_tenant_def; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_memory_samples_tenant_def ON public.workflow_memory_samples USING btree (tenant_id, def_name, recorded_at DESC);


--
-- Name: idx_promises_id_unique; Type: INDEX; Schema: public; Owner: postgres
--

CREATE UNIQUE INDEX idx_promises_id_unique ON public.workflow_promises USING btree (tenant_id, promise_id);


--
-- Name: idx_promises_status; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_promises_status ON public.workflow_promises USING btree (workflow_id, status);


--
-- Name: idx_queue_holders_expires; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_queue_holders_expires ON public.queue_holders USING btree (expires_at);


--
-- Name: idx_queue_holders_worker; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_queue_holders_worker ON public.queue_holders USING btree (tenant_id, queue_name, worker_id, expires_at);


--
-- Name: idx_queue_holders_workflow; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_queue_holders_workflow ON public.queue_holders USING btree (workflow_id);


--
-- Name: idx_queue_rate_tokens_expires; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_queue_rate_tokens_expires ON public.queue_rate_tokens USING btree (expires_at);


--
-- Name: idx_queue_rate_tokens_window; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_queue_rate_tokens_window ON public.queue_rate_tokens USING btree (tenant_id, queue_name, expires_at);


--
-- Name: idx_schedules_tenant_due; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_schedules_tenant_due ON public.workflow_schedules USING btree (tenant_id, next_run_at) WHERE (disabled_at IS NULL);


--
-- Name: idx_signals_tenant_wf; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_signals_tenant_wf ON public.workflow_signals USING btree (tenant_id, workflow_id, signal_name);


--
-- Name: idx_slack_workspace_tenant; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_slack_workspace_tenant ON public.slack_workspace USING btree (tenant_id);


--
-- Name: idx_tenant_domains_tenant; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_tenant_domains_tenant ON public.tenant_domains USING btree (tenant_id);


--
-- Name: idx_update_requests_pending; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_update_requests_pending ON public.workflow_update_requests USING btree (workflow_id, status);


--
-- Name: idx_update_requests_pending_name; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_update_requests_pending_name ON public.workflow_update_requests USING btree (workflow_id, update_name, status);


--
-- Name: idx_workflow_instances_continued_from; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_workflow_instances_continued_from ON public.workflow_instances USING btree (continued_from) WHERE (continued_from IS NOT NULL);


--
-- Name: idx_workflow_instances_defer_phase_deadline; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_workflow_instances_defer_phase_deadline ON public.workflow_instances USING btree (defer_phase_deadline) WHERE (pending_terminal_status IS NOT NULL);


--
-- Name: idx_workflow_signals_queue; Type: INDEX; Schema: public; Owner: postgres
--

CREATE INDEX idx_workflow_signals_queue ON public.workflow_signals USING btree (workflow_id, signal_name);


--
-- Name: uq_workflow_schedules_idempotency_key; Type: INDEX; Schema: public; Owner: postgres
--

CREATE UNIQUE INDEX uq_workflow_schedules_idempotency_key ON public.workflow_schedules USING btree (tenant_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);


--
-- Name: tenant_api_keys tenant_api_keys_tenant_id_fkey; Type: FK CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenant_api_keys
    ADD CONSTRAINT tenant_api_keys_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id);


--
-- Name: tenant_egress_allow tenant_egress_allow_tenant_id_fkey; Type: FK CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenant_egress_allow
    ADD CONSTRAINT tenant_egress_allow_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;


--
-- Name: tenant_roles tenant_roles_tenant_id_fkey; Type: FK CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenant_roles
    ADD CONSTRAINT tenant_roles_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id);


--
-- Name: tenants tenants_org_id_fkey; Type: FK CONSTRAINT; Schema: admin; Owner: postgres
--

ALTER TABLE ONLY admin.tenants
    ADD CONSTRAINT tenants_org_id_fkey FOREIGN KEY (org_id) REFERENCES admin.orgs(org_id);


--
-- Name: concurrency_keys concurrency_keys_workflow_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.concurrency_keys
    ADD CONSTRAINT concurrency_keys_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflow_instances(id) ON DELETE CASCADE;


--
-- Name: queue_holders queue_holders_workflow_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.queue_holders
    ADD CONSTRAINT queue_holders_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflow_instances(id) ON DELETE CASCADE;


--
-- Name: queue_rate_tokens queue_rate_tokens_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.queue_rate_tokens
    ADD CONSTRAINT queue_rate_tokens_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;


--
-- Name: queues queues_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.queues
    ADD CONSTRAINT queues_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;


--
-- Name: slack_workspace slack_workspace_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.slack_workspace
    ADD CONSTRAINT slack_workspace_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;


--
-- Name: tenant_domains tenant_domains_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.tenant_domains
    ADD CONSTRAINT tenant_domains_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;


--
-- Name: tenant_secrets tenant_secrets_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.tenant_secrets
    ADD CONSTRAINT tenant_secrets_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;


--
-- Name: tenant_settings tenant_settings_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.tenant_settings
    ADD CONSTRAINT tenant_settings_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE;


--
-- Name: workflow_instances workflow_instances_def_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_instances
    ADD CONSTRAINT workflow_instances_def_fkey FOREIGN KEY (tenant_id, def_name, def_version) REFERENCES public.workflow_defs(tenant_id, name, version);


--
-- Name: workflow_promises workflow_promises_workflow_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_promises
    ADD CONSTRAINT workflow_promises_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflow_instances(id) ON DELETE CASCADE;


--
-- Name: workflow_routing workflow_routing_def_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_routing
    ADD CONSTRAINT workflow_routing_def_fkey FOREIGN KEY (tenant_id, workflow_name, target_version) REFERENCES public.workflow_defs(tenant_id, name, version);


--
-- Name: workflow_signals workflow_signals_workflow_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_signals
    ADD CONSTRAINT workflow_signals_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflow_instances(id) ON DELETE CASCADE;


--
-- Name: workflow_tags workflow_tags_def_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_tags
    ADD CONSTRAINT workflow_tags_def_fkey FOREIGN KEY (tenant_id, workflow_name, version) REFERENCES public.workflow_defs(tenant_id, name, version);


--
-- Name: workflow_update_requests workflow_update_requests_workflow_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.workflow_update_requests
    ADD CONSTRAINT workflow_update_requests_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflow_instances(id) ON DELETE CASCADE;


--
-- Name: concurrency_keys; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.concurrency_keys ENABLE ROW LEVEL SECURITY;

--
-- Name: idempotency_keys; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.idempotency_keys ENABLE ROW LEVEL SECURITY;

--
-- Name: idempotency_keys idempotency_keys_cross_tenant; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY idempotency_keys_cross_tenant ON public.idempotency_keys TO cleat_sweep USING (true);


--
-- Name: idempotency_keys idempotency_keys_tenant_isolation; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY idempotency_keys_tenant_isolation ON public.idempotency_keys USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: queues; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.queues ENABLE ROW LEVEL SECURITY;

--
-- Name: tenant_domains; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.tenant_domains ENABLE ROW LEVEL SECURITY;

--
-- Name: concurrency_keys tenant_isolation_concurrency_keys; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_concurrency_keys ON public.concurrency_keys USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_defs tenant_isolation_defs; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_defs ON public.workflow_defs USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: tenant_domains tenant_isolation_domains; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_domains ON public.tenant_domains USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_instances tenant_isolation_instances; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_instances ON public.workflow_instances USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_memory_samples tenant_isolation_memory_samples; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_memory_samples ON public.workflow_memory_samples USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_memory_stats tenant_isolation_memory_stats; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_memory_stats ON public.workflow_memory_stats USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_promises tenant_isolation_promises; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_promises ON public.workflow_promises USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: queues tenant_isolation_queues; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_queues ON public.queues USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_routing tenant_isolation_routing; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_routing ON public.workflow_routing USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_schedules tenant_isolation_schedules; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_schedules ON public.workflow_schedules USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: tenant_secrets tenant_isolation_secrets; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_secrets ON public.tenant_secrets USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: tenant_settings tenant_isolation_settings; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_settings ON public.tenant_settings USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_signals tenant_isolation_signals; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_signals ON public.workflow_signals USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_tags tenant_isolation_tags; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_tags ON public.workflow_tags USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: workflow_update_requests tenant_isolation_update_requests; Type: POLICY; Schema: public; Owner: postgres
--

CREATE POLICY tenant_isolation_update_requests ON public.workflow_update_requests USING ((tenant_id = cleat.assert_tenant_set()));


--
-- Name: tenant_secrets; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.tenant_secrets ENABLE ROW LEVEL SECURITY;

--
-- Name: tenant_settings; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.tenant_settings ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_defs; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_defs ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_instances; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_instances ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_memory_samples; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_memory_samples ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_memory_stats; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_memory_stats ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_promises; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_promises ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_routing; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_routing ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_schedules; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_schedules ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_signals; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_signals ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_tags; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_tags ENABLE ROW LEVEL SECURITY;

--
-- Name: workflow_update_requests; Type: ROW SECURITY; Schema: public; Owner: postgres
--

ALTER TABLE public.workflow_update_requests ENABLE ROW LEVEL SECURITY;

--
-- Name: SCHEMA admin; Type: ACL; Schema: -; Owner: postgres
--

GRANT USAGE ON SCHEMA admin TO cleat_app;
GRANT USAGE ON SCHEMA admin TO cleat_sweep;


--
-- Name: SCHEMA cleat; Type: ACL; Schema: -; Owner: postgres
--

GRANT USAGE ON SCHEMA cleat TO cleat_app;
GRANT USAGE ON SCHEMA cleat TO cleat_sweep;


--
-- Name: SCHEMA public; Type: ACL; Schema: -; Owner: pg_database_owner
--

GRANT USAGE ON SCHEMA public TO cleat_app;
GRANT USAGE ON SCHEMA public TO cleat_dispatcher;
GRANT USAGE ON SCHEMA public TO cleat_sweep;

-- schema_migrations itself is created by migration.Runner, not by any
-- migration file (see the header note above) -- so it never appears in the
-- pg_dump-derived table/ACL blocks below, and gets no grant from them.
-- Historically it picked one up as a side effect of 005_app_role.sql's
-- "GRANT ... ON ALL TABLES IN SCHEMA public TO cleat_app", run back when
-- schema_migrations was the only table that existed yet. That statement
-- isn't reproduced here (every other table it would have swept up already
-- has its own explicit grant below), so it's spelled out for the one table
-- it actually mattered for.
GRANT SELECT, INSERT, UPDATE, DELETE ON public.schema_migrations TO cleat_app;


--
-- Name: FUNCTION gtrgm_in(cstring); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_in(cstring) TO cleat_app;


--
-- Name: FUNCTION gtrgm_out(public.gtrgm); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_out(public.gtrgm) TO cleat_app;


--
-- Name: FUNCTION assert_tenant_set(); Type: ACL; Schema: cleat; Owner: postgres
--

GRANT ALL ON FUNCTION cleat.assert_tenant_set() TO cleat_app;
GRANT ALL ON FUNCTION cleat.assert_tenant_set() TO cleat_sweep;


--
-- Name: FUNCTION armor(bytea); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.armor(bytea) TO cleat_app;


--
-- Name: FUNCTION armor(bytea, text[], text[]); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.armor(bytea, text[], text[]) TO cleat_app;


--
-- Name: FUNCTION crypt(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.crypt(text, text) TO cleat_app;


--
-- Name: FUNCTION dearmor(text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.dearmor(text) TO cleat_app;


--
-- Name: FUNCTION decrypt(bytea, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.decrypt(bytea, bytea, text) TO cleat_app;


--
-- Name: FUNCTION decrypt_iv(bytea, bytea, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.decrypt_iv(bytea, bytea, bytea, text) TO cleat_app;


--
-- Name: FUNCTION digest(bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.digest(bytea, text) TO cleat_app;


--
-- Name: FUNCTION digest(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.digest(text, text) TO cleat_app;


--
-- Name: FUNCTION encrypt(bytea, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.encrypt(bytea, bytea, text) TO cleat_app;


--
-- Name: FUNCTION encrypt_iv(bytea, bytea, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.encrypt_iv(bytea, bytea, bytea, text) TO cleat_app;


--
-- Name: FUNCTION gen_random_bytes(integer); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gen_random_bytes(integer) TO cleat_app;


--
-- Name: FUNCTION gen_random_uuid(); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gen_random_uuid() TO cleat_app;


--
-- Name: FUNCTION gen_salt(text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gen_salt(text) TO cleat_app;


--
-- Name: FUNCTION gen_salt(text, integer); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gen_salt(text, integer) TO cleat_app;


--
-- Name: FUNCTION gin_extract_query_trgm(text, internal, smallint, internal, internal, internal, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gin_extract_query_trgm(text, internal, smallint, internal, internal, internal, internal) TO cleat_app;


--
-- Name: FUNCTION gin_extract_value_trgm(text, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gin_extract_value_trgm(text, internal) TO cleat_app;


--
-- Name: FUNCTION gin_trgm_consistent(internal, smallint, text, integer, internal, internal, internal, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gin_trgm_consistent(internal, smallint, text, integer, internal, internal, internal, internal) TO cleat_app;


--
-- Name: FUNCTION gin_trgm_triconsistent(internal, smallint, text, integer, internal, internal, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gin_trgm_triconsistent(internal, smallint, text, integer, internal, internal, internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_compress(internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_compress(internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_consistent(internal, text, smallint, oid, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_consistent(internal, text, smallint, oid, internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_decompress(internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_decompress(internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_distance(internal, text, smallint, oid, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_distance(internal, text, smallint, oid, internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_options(internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_options(internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_penalty(internal, internal, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_penalty(internal, internal, internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_picksplit(internal, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_picksplit(internal, internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_same(public.gtrgm, public.gtrgm, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_same(public.gtrgm, public.gtrgm, internal) TO cleat_app;


--
-- Name: FUNCTION gtrgm_union(internal, internal); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.gtrgm_union(internal, internal) TO cleat_app;


--
-- Name: FUNCTION hmac(bytea, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.hmac(bytea, bytea, text) TO cleat_app;


--
-- Name: FUNCTION hmac(text, text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.hmac(text, text, text) TO cleat_app;


--
-- Name: FUNCTION pgp_armor_headers(text, OUT key text, OUT value text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_armor_headers(text, OUT key text, OUT value text) TO cleat_app;


--
-- Name: FUNCTION pgp_key_id(bytea); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_key_id(bytea) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_decrypt(bytea, bytea); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_decrypt(bytea, bytea) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_decrypt(bytea, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_decrypt(bytea, bytea, text) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_decrypt(bytea, bytea, text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_decrypt(bytea, bytea, text, text) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_decrypt_bytea(bytea, bytea); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_decrypt_bytea(bytea, bytea) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_decrypt_bytea(bytea, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_decrypt_bytea(bytea, bytea, text) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_decrypt_bytea(bytea, bytea, text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_decrypt_bytea(bytea, bytea, text, text) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_encrypt(text, bytea); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_encrypt(text, bytea) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_encrypt(text, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_encrypt(text, bytea, text) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_encrypt_bytea(bytea, bytea); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_encrypt_bytea(bytea, bytea) TO cleat_app;


--
-- Name: FUNCTION pgp_pub_encrypt_bytea(bytea, bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_pub_encrypt_bytea(bytea, bytea, text) TO cleat_app;


--
-- Name: FUNCTION pgp_sym_decrypt(bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_sym_decrypt(bytea, text) TO cleat_app;


--
-- Name: FUNCTION pgp_sym_decrypt(bytea, text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_sym_decrypt(bytea, text, text) TO cleat_app;


--
-- Name: FUNCTION pgp_sym_decrypt_bytea(bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_sym_decrypt_bytea(bytea, text) TO cleat_app;


--
-- Name: FUNCTION pgp_sym_decrypt_bytea(bytea, text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_sym_decrypt_bytea(bytea, text, text) TO cleat_app;


--
-- Name: FUNCTION pgp_sym_encrypt(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_sym_encrypt(text, text) TO cleat_app;


--
-- Name: FUNCTION pgp_sym_encrypt(text, text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_sym_encrypt(text, text, text) TO cleat_app;


--
-- Name: FUNCTION pgp_sym_encrypt_bytea(bytea, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_sym_encrypt_bytea(bytea, text) TO cleat_app;


--
-- Name: FUNCTION pgp_sym_encrypt_bytea(bytea, text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.pgp_sym_encrypt_bytea(bytea, text, text) TO cleat_app;


--
-- Name: FUNCTION set_limit(real); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.set_limit(real) TO cleat_app;


--
-- Name: FUNCTION show_limit(); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.show_limit() TO cleat_app;


--
-- Name: FUNCTION show_trgm(text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.show_trgm(text) TO cleat_app;


--
-- Name: FUNCTION similarity(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.similarity(text, text) TO cleat_app;


--
-- Name: FUNCTION similarity_dist(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.similarity_dist(text, text) TO cleat_app;


--
-- Name: FUNCTION similarity_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.similarity_op(text, text) TO cleat_app;


--
-- Name: FUNCTION strict_word_similarity(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.strict_word_similarity(text, text) TO cleat_app;


--
-- Name: FUNCTION strict_word_similarity_commutator_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.strict_word_similarity_commutator_op(text, text) TO cleat_app;


--
-- Name: FUNCTION strict_word_similarity_dist_commutator_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.strict_word_similarity_dist_commutator_op(text, text) TO cleat_app;


--
-- Name: FUNCTION strict_word_similarity_dist_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.strict_word_similarity_dist_op(text, text) TO cleat_app;


--
-- Name: FUNCTION strict_word_similarity_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.strict_word_similarity_op(text, text) TO cleat_app;


--
-- Name: FUNCTION word_similarity(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.word_similarity(text, text) TO cleat_app;


--
-- Name: FUNCTION word_similarity_commutator_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.word_similarity_commutator_op(text, text) TO cleat_app;


--
-- Name: FUNCTION word_similarity_dist_commutator_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.word_similarity_dist_commutator_op(text, text) TO cleat_app;


--
-- Name: FUNCTION word_similarity_dist_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.word_similarity_dist_op(text, text) TO cleat_app;


--
-- Name: FUNCTION word_similarity_op(text, text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.word_similarity_op(text, text) TO cleat_app;


--
-- Name: TABLE orgs; Type: ACL; Schema: admin; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.orgs TO cleat_app;


--
-- Name: TABLE plugin_tables; Type: ACL; Schema: admin; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.plugin_tables TO cleat_app;


--
-- Name: TABLE tenant_api_keys; Type: ACL; Schema: admin; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.tenant_api_keys TO cleat_app;


--
-- Name: TABLE tenant_egress_allow; Type: ACL; Schema: admin; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.tenant_egress_allow TO cleat_app;


--
-- Name: TABLE tenant_roles; Type: ACL; Schema: admin; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.tenant_roles TO cleat_app;


--
-- Name: TABLE tenants; Type: ACL; Schema: admin; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.tenants TO cleat_app;


--
-- Name: TABLE workers; Type: ACL; Schema: admin; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE admin.workers TO cleat_app;


--
-- Name: TABLE concurrency_keys; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.concurrency_keys TO cleat_app;


--
-- Name: TABLE deployment_secrets; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT ON TABLE public.deployment_secrets TO cleat_app;


-- Name: TABLE event_history; Type: ACL; Schema: public; Owner: postgres
--
-- Grants on the 64 partitions themselves are per-tenant-role, not static --
-- see admin.grant_core_tables_to_tenant_role (003_procedures.sql), which
-- loops over event_history_p0..p63 for the same non-propagation reason as
-- the RLS DO block above. cleat_app is a static role and is granted here,
-- on the parent AND (cleat#2059) on every partition, since a static role's
-- grant needs no per-tenant timing.

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.event_history TO cleat_app;

DO $$
DECLARE
    i int;
BEGIN
    FOR i IN 0..63 LOOP
        EXECUTE format('GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.event_history_p%1$s TO cleat_app', i);
    END LOOP;
END $$;

--
-- Name: TABLE idempotency_keys; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.idempotency_keys TO cleat_app;
GRANT SELECT,DELETE ON TABLE public.idempotency_keys TO cleat_sweep;


--
-- Name: TABLE plugin_defs; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.plugin_defs TO cleat_app;


--
-- Name: TABLE queue_holders; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.queue_holders TO cleat_app;


--
-- Name: TABLE queue_rate_tokens; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.queue_rate_tokens TO cleat_app;


--
-- Name: TABLE queues; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.queues TO cleat_app;


--
-- Name: TABLE slack_workspace; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT ON TABLE public.slack_workspace TO cleat_app;


--
-- Name: TABLE tenant_domains; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.tenant_domains TO cleat_app;


--
-- Name: TABLE tenant_secrets; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.tenant_secrets TO cleat_app;


--
-- Name: TABLE tenant_settings; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.tenant_settings TO cleat_app;


--
-- Name: TABLE workflow_defs; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_defs TO cleat_app;


--
-- Name: TABLE workflow_instances; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_instances TO cleat_app;
GRANT SELECT,UPDATE ON TABLE public.workflow_instances TO cleat_dispatcher;


--
-- Name: TABLE workflow_memory_samples; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_memory_samples TO cleat_app;


--
-- Name: SEQUENCE workflow_memory_samples_id_seq; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,USAGE ON SEQUENCE public.workflow_memory_samples_id_seq TO cleat_app;


--
-- Name: TABLE workflow_memory_stats; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_memory_stats TO cleat_app;


--
-- Name: TABLE workflow_promises; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_promises TO cleat_app;


--
-- Name: TABLE workflow_routing; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_routing TO cleat_app;


--
-- Name: TABLE workflow_schedules; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_schedules TO cleat_app;
GRANT SELECT ON TABLE public.workflow_schedules TO cleat_dispatcher;


--
-- Name: TABLE workflow_signals; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_signals TO cleat_app;


--
-- Name: SEQUENCE workflow_signals_id_seq; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,USAGE ON SEQUENCE public.workflow_signals_id_seq TO cleat_app;


--
-- Name: TABLE workflow_tags; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_tags TO cleat_app;


--
-- Name: TABLE workflow_update_requests; Type: ACL; Schema: public; Owner: postgres
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.workflow_update_requests TO cleat_app;


