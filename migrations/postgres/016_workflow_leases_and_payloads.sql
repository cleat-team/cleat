-- cleat#3245 Phase 3, step 1 of 4 (see the design note on the issue for the
-- full sequencing): ADDITIVE SCHEMA ONLY. This migration creates
-- workflow_leases and workflow_payloads and nothing else -- no column is
-- dropped from workflow_instances, no row is moved or backfilled, and no Go
-- code in this tree yet reads or writes either new table. That is
-- deliberate: owner decision recorded on the issue requires a fresh database
-- for 0.5.0 (no deployed databases exist yet), so there is no existing
-- workflow_instances population to backfill on any supported upgrade path --
-- these tables start empty and are populated going forward by the dual-write
-- code landing in a later PR (step 2).
--
-- THE SPLIT, and why these columns and not others. Phase 1 (this issue's own
-- measurement) found two costs Phase 2 (#3272/#3274, migration 015) could not
-- touch: H2, every heartbeat physically copies whatever payload sits on the
-- same row regardless of indexing (a HOT update still writes a complete new
-- tuple -- HOT only skips index maintenance), and H3, repeated heartbeats
-- scatter a row's TOAST chunks because the heap tuple moves every time,
-- independent of whether that move was HOT. Moving the columns a heartbeat
-- never touches off the row a heartbeat DOES touch is the only remaining
-- lever for either.
--
-- workflow_leases: every column written by claim, heartbeat, reclaim, fence,
-- signal delivery or promise delivery -- derived from the actual
-- `UPDATE workflow_instances` statements across all 16 PostgresStore-receiver
-- files that write to it (re-derive: see the design note's appendix command),
-- not from which columns happen to be indexed. That caught two things a
-- cruder "is it indexed" rule would have missed: the claim-time snapshot
-- columns (signal_seq_at_claim, signal_consumed_at_claim, promise_seq_at_claim,
-- started_at) and the LIVE counters they are compared against
-- (signal_seq, signal_consumed_seq, promise_seq) have zero index coupling but
-- are written in the SAME statement as a genuine lease column (next_wake_at)
-- and compared against each other inside finalize_workflow_status's 'ready'
-- branch (migrations/postgres/014) -- splitting them apart would buy nothing
-- (they are a few bytes each) and cost a second statement on every signal and
-- promise delivery.
--
-- task_queue, priority and created_at are DUPLICATED here rather than left on
-- workflow_instances, decided by measurement rather than guessing: a
-- disposable-container benchmark at Phase 1's own H4 crossover population
-- (294,000 rows, 99,300 -- 34% -- in the ready backlog) compared a
-- join-based claim query (these three columns stay on workflow_instances)
-- against a duplicate-based one (copied here, single-table query identical
-- in shape to today's idx_instances_tenant_queue_claimable) and the join lost
-- by 6-17x (11.8ms/4.2ms cold/warm vs 88.0ms/69.9ms), because no single index
-- can jointly serve a filter on one table and an ORDER BY on another --
-- Postgres falls back to a hash join driven by the LARGER filtered set (the
-- ready backlog) regardless of how selective task_queue is. These three are
-- written once at StartNewRun and never updated again, same as created_at's
-- own character on workflow_instances today, so duplicating them carries no
-- ongoing sync-drift risk.
--
-- cancellation_requested/cancellation_reason are a judgment call, stated as
-- one: unindexed, mutated outside creation (a cancellation can be requested
-- on a running workflow at any time), but checked on the same hot
-- resume/heartbeat path as the other lease columns rather than being "what
-- this run's definition says", so they are grouped with leases.
--
-- workflow_payloads: input, result, query_state, compaction_state (plus
-- compacted_at/compaction_step, written in the same statements), plugin_vers,
-- allowed_signals, error_msg/error_code/error_op. Every one of these is
-- confirmed to carry ZERO index coupling on workflow_instances today (none
-- appear in any `CREATE INDEX ... ON workflow_instances` in 001_schema.sql),
-- and every one is written only at creation, completion, compaction or
-- failure -- never on the heartbeat path. error_msg/error_code/error_op ride
-- along because they are written in the SAME statements as result/query_state
-- (CompleteWorkflow, FailWorkflow, MoveToDeadLetterQueue,
-- finalizeWorkflowSegmentInner via finalize_workflow_status), not because
-- they are individually large.
--
-- A REAL COST THIS SURFACES, stated here so step 2 does not have to discover
-- it: CompleteWorkflow, FailWorkflow, MoveToDeadLetterQueue and
-- finalize_workflow_status (the stored procedure, migrations/postgres/014 is
-- its current highest-numbered definition -- re-derive, do not trust this
-- number: `git grep -ln finalize_workflow_status -- migrations/postgres`)
-- today write a lease column and a payload column in ONE statement. After the
-- split each becomes two statements across two tables. Every call site
-- checked so far already wraps its writes in one *sql.Tx via
-- s.beginTxWithRLS (or s.db.BeginTx + s.setRLSOnTx) with a single tx.Commit()
-- at the end -- ClaimWorkflow, ClaimWorkflows, CompleteWorkflow, FailWorkflow
-- and MoveToDeadLetterQueue all confirmed -- so the two statements commit
-- together or not at all; no new transaction machinery is needed, only care
-- that step 2 keeps both statements on the same tx handle. This is the mirror
-- image of the heartbeat win: finalize gets more expensive in statement
-- count to buy the heartbeat and retention benefits, and finalize is far
-- less frequent than heartbeat, so the trade should favour the split -- but
-- it is a trade, not only a win.
--
-- THE CORRECTNESS INVARIANT THIS STEP DOES NOT NEED YET, BUT STEP 3 MUST
-- CARRY: because there is no backfill, every workflow_leases/workflow_payloads
-- row is created in the SAME transaction as its workflow_instances row from
-- the moment step 2 ships -- there is no transition window where one exists
-- without the other. The read path must still never treat "no
-- workflow_leases row for this id" as "safe to claim": it is defensive
-- insurance against an operator upgrading in place despite the fresh-database
-- requirement, not a mechanism this step needs to build, and it should cost
-- nothing to satisfy once every row genuinely has both from creation.
--
-- NO CONCURRENTLY, same reason as migration 015: the runner wraps every file
-- in one transaction and CREATE/DROP INDEX CONCURRENTLY cannot run inside
-- one. Both new tables are EMPTY at this migration's commit (no backfill, see
-- above), so every index built here costs nothing regardless.

CREATE TABLE IF NOT EXISTS workflow_leases (
    id text PRIMARY KEY REFERENCES workflow_instances(id) ON DELETE CASCADE,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    task_queue text DEFAULT 'default'::text NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    status text DEFAULT 'ready'::text NOT NULL,
    assigned_to text,
    heartbeat_at TIMESTAMPTZ,
    next_wake_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    generation bigint DEFAULT 0 NOT NULL,
    pending_terminal_status text,
    defer_phase_deadline TIMESTAMPTZ,
    sticky_worker_id text,
    reclaim_count bigint DEFAULT 0 NOT NULL,
    started_at TIMESTAMPTZ,
    signal_seq bigint DEFAULT 0 NOT NULL,
    signal_seq_at_claim bigint DEFAULT 0 NOT NULL,
    signal_consumed_seq bigint DEFAULT 0 NOT NULL,
    signal_consumed_at_claim bigint DEFAULT 0 NOT NULL,
    promise_seq bigint DEFAULT 0 NOT NULL,
    promise_seq_at_claim bigint DEFAULT 0 NOT NULL,
    cancellation_requested boolean DEFAULT false NOT NULL,
    cancellation_reason text
);

ALTER TABLE workflow_leases ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY workflow_leases FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_leases ON workflow_leases;
CREATE POLICY tenant_isolation_leases ON workflow_leases USING ((tenant_id = cleat.assert_tenant_set()));

-- Mirrors idx_instances_heartbeat's post-Phase-2 shape exactly (migration
-- 015): HeartbeatBatchFenced's SELECT filters on assigned_to and status
-- alone, never heartbeat_at.
CREATE INDEX IF NOT EXISTS idx_leases_heartbeat ON workflow_leases USING btree (assigned_to) WHERE (status = 'running'::text);

-- RESTORES heartbeat_at, which migration 015 had to drop from
-- idx_instances_stale because re-indexing it on the fat workflow_instances
-- row meant re-paying H1's cost on every heartbeat. On this narrow table
-- (no payload columns at all) that trade no longer applies: ReapStaleInstances
-- and ListStaleHolders (engine/store_lifecycle.go) get back an efficient
-- range+order scan without reintroducing the index-churn cost Phase 2 fixed.
-- Not yet measured against this specific table -- step 3 (cutting reads over)
-- should re-run Phase 1's H4-style measurement against the real narrow table
-- rather than trust this comment's reasoning alone.
CREATE INDEX IF NOT EXISTS idx_leases_stale ON workflow_leases USING btree (status, heartbeat_at) WHERE (status = 'running'::text);

CREATE INDEX IF NOT EXISTS idx_leases_sticky ON workflow_leases USING btree (sticky_worker_id) WHERE (sticky_worker_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_leases_defer_phase_deadline ON workflow_leases USING btree (defer_phase_deadline) WHERE (pending_terminal_status IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_leases_claimable ON workflow_leases USING btree (status, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));

CREATE INDEX IF NOT EXISTS idx_leases_tenant_claimable ON workflow_leases USING btree (tenant_id, status, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));

-- Mirrors idx_instances_claim_order, on the duplicated ordering columns --
-- the benchmark-decided shape above.
CREATE INDEX IF NOT EXISTS idx_leases_claim_order ON workflow_leases USING btree (tenant_id, task_queue, priority, created_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));

-- Mirrors idx_instances_tenant_queue_claimable, same reason.
CREATE INDEX IF NOT EXISTS idx_leases_tenant_queue_claimable ON workflow_leases USING btree (tenant_id, task_queue, status, priority, next_wake_at) WHERE (status = ANY (ARRAY['ready'::text, 'terminating'::text]));

CREATE TABLE IF NOT EXISTS workflow_payloads (
    id text PRIMARY KEY REFERENCES workflow_instances(id) ON DELETE CASCADE,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    input jsonb DEFAULT '{}'::jsonb NOT NULL,
    result jsonb,
    query_state jsonb DEFAULT '{}'::jsonb,
    compaction_state jsonb,
    compacted_at TIMESTAMPTZ,
    compaction_step integer,
    plugin_vers jsonb DEFAULT '{}'::jsonb NOT NULL,
    allowed_signals jsonb,
    error_msg text,
    error_code text,
    error_op text
);

ALTER TABLE workflow_payloads ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY workflow_payloads FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_payloads ON workflow_payloads;
CREATE POLICY tenant_isolation_payloads ON workflow_payloads USING ((tenant_id = cleat.assert_tenant_set()));

-- Mirrors idx_instances_error_msg_trgm. Same pg_trgm-schema-resolution
-- dance as 001_schema.sql's original, since this runs on any deployment
-- regardless of which schema pg_trgm's extension landed in.
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
    EXECUTE format('CREATE INDEX IF NOT EXISTS idx_payloads_error_msg_trgm ON workflow_payloads USING GIN (error_msg %I.gin_trgm_ops) WHERE (error_msg IS NOT NULL)',
                   ext_schema);
END
$trgm$;

-- EXPLICIT grants, not left to ALTER DEFAULT PRIVILEGES -- same reasoning as
-- 008_payload_encryption_ever_enabled.sql and 010_operator_api_keys.sql: these
-- are new, load-bearing tables, and there is no precedent in this tree for
-- trusting the default-privilege path for one. cleat_dispatcher (the
-- cross-tenant claim role, see 001_schema.sql's admin.claim_workflows) needs
-- the same SELECT/UPDATE it has on workflow_instances today on
-- workflow_leases, since claiming will read and update it; it needs nothing
-- on workflow_payloads, since claiming never touches payload data.
GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE workflow_leases TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE workflow_leases TO cleat_app;
GRANT SELECT, UPDATE ON TABLE workflow_leases TO cleat_dispatcher;

GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE workflow_payloads TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE workflow_payloads TO cleat_app;

-- Reproduced in full and redefined (CREATE OR REPLACE), per this project's own
-- rule for a routine with no version suffix in its name: find the
-- highest-numbered migration that defines it, which until this one is
-- 001_schema.sql, and carry its body forward rather than guessing at a diff.
-- Every new per-tenant role provisioned after this migration needs these two
-- new tables granted the same way workflow_instances already is, immediately
-- after it in the list -- otherwise a new tenant's role can create a workflow
-- but cannot write its own lease or payload row once step 2's dual-write
-- code lands.
CREATE OR REPLACE FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path FROM CURRENT
    AS $$
BEGIN
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_defs TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_instances TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_leases TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_payloads TO %I', current_schema(), p_role_name);
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
