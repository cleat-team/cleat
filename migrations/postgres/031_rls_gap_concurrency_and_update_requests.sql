-- cleat migration 031 (postgres): close the RLS gap on two tenant-scoped
-- tables -- concurrency_keys, workflow_update_requests.
--
-- Finding S1. 001_schema.sql enables and FORCEs Row-Level Security on eight
-- tables (workflow_defs, workflow_instances, event_history, workflow_signals,
-- workflow_schedules, workflow_tags, workflow_routing, workflow_promises).
-- Six other tenant-scoped tables were left out:
--
--   concurrency_keys           -- protected here
--   workflow_update_requests   -- protected here
--   idempotency_keys           -- deliberately NOT protected; see below
--   admin.tenant_api_keys      -- deliberately NOT protected; see below
--   kv_store                   -- cannot be protected from this file; see below
--   feature_flags              -- cannot be protected from this file; see below
--
-- This migration does not blanket-apply RLS to all six. Each of the four not
-- touched here has a specific, checked reason; see the comment for each
-- below rather than treating the omission as an oversight.
--
-- ---------------------------------------------------------------------------
-- Why concurrency_keys and workflow_update_requests: every Postgres access
-- site for both already runs inside a transaction opened by
-- s.beginTxWithRLS (engine/db.go's AcquireConcurrencyKey,
-- ReleaseConcurrencyKey, ReleaseWorkflowConcurrencyKeys,
-- ReapExpiredConcurrencyKeys, GetConcurrencyKeyCount; engine/store_promises.go's
-- CreateUpdateRequest, GetPendingUpdateRequests, CompleteUpdateRequest) --
-- checked by reading every "concurrency_keys" and "workflow_update_requests"
-- occurrence in engine/*.go outside the mysql_*.go and mssql_*.go files.
-- beginTxWithRLS calls setRLSOnTx before the caller runs a single statement,
-- so cleat.tenant_id is always set by the time these tables are touched. A
-- fail-closed assert_tenant_set() policy, identical in shape to the eight
-- tables 001_schema.sql already protects, adds a real backstop with no
-- currently-known path that would break under it.
--
-- Both tables also already carry an explicit tenant_id = $N predicate in Go
-- at every one of those call sites (verified by reading them), so this
-- migration adds a second, independent layer rather than the only one --
-- exactly the shape CLAUDE.md asks be provable by removing each layer in
-- turn. See engine/rls_gap_concurrency_and_update_requests_test.go for that
-- proof.
--
-- ---------------------------------------------------------------------------
-- Why NOT idempotency_keys: engine/store_lifecycle.go's StartNewRun reads
-- and writes idempotency_keys *before* any RLS context exists on the
-- connection it uses.
--
--   - The existing-key lookup (`SELECT workflow_id FROM idempotency_keys
--     WHERE key_hash = $1 AND tenant_id = $2 ...`) runs on s.db directly --
--     no transaction, no set_config.
--   - The INSERT that follows opens its own tx via `s.db.BeginTx` (not
--     s.beginTxWithRLS), and setRLSOnTx is called on that tx only *after*
--     the idempotency INSERT's RowsAffected has already been read.
--
-- A fail-closed `tenant_id = cleat.assert_tenant_set()` policy would reject
-- both: assert_tenant_set() raises "cleat.tenant_id is not set" the moment
-- either statement runs, so every call to StartNewRun with an idempotency
-- key would start failing. Fixing that requires reordering StartNewRun's
-- transaction structure, which touches lines this stream does not own
-- (engine/store_lifecycle.go's ownership here is scoped to the three
-- idempotency_keys UPDATE statements in CompleteWorkflow/FailWorkflow/
-- MoveToDeadLetterQueue, not StartNewRun) and overlaps WS-1's ownership of
-- idempotency_keys (migrations/*/010_idempotency_keys_tenant_id.sql,
-- IMPROVEMENT-PLAN 3.10). Left for whoever picks that up next; this
-- migration does not touch idempotency_keys.
--
-- Why NOT admin.tenant_api_keys: it is read by
-- engine/store_deployment.go's ResolveTenantFromAPIKey specifically to
-- *determine* the tenant from an API key, before any tenant is known --
-- `s.db.QueryRowContext(... SELECT tenant_id FROM admin.tenant_api_keys
-- WHERE key_hash = $1 ...)`, again with no transaction and no set_config. A
-- tenant_id-scoped RLS policy here would raise assert_tenant_set()'s
-- exception on every authentication attempt, since authenticating is
-- exactly the step that has not yet established a tenant. This table is
-- correctly unscoped by design, not an accidental gap; auth/tenant_store.go
-- and engine/mssql_deployment.go treat it the same way.
--
-- Why NOT kv_store / feature_flags: both are created by plugin migrations
-- (plugins/kvstore/migrations.go, plugins/featureflags/migrations.go) at
-- plugin-load time, not by anything under migrations/postgres/. A statement
-- here (`ALTER TABLE kv_store ...`) would run before those tables exist on
-- a fresh deploy, since deploy/postgres/100-apply-migrations.sh applies
-- every file under migrations/postgres/ before the worker process starts
-- and loads plugins. RLS for these two tables has to be added as a new
-- Migration entry inside each plugin's own Migrations() -- files this
-- stream's declared ownership does not include (engine/version_handler.go,
-- cmd/cleat-worker/main.go's registration site, engine/store_lifecycle.go's
-- three idempotency UPDATEs, and migrations/{postgres,mssql}/). Both tables
-- already carry a tenant_id column and a Go-level tenant_id filter in every
-- query (plugins/kvstore/queries.go, plugins/featureflags), so they are not
-- unscoped, only unbacked by a database-level policy. Flagged for a
-- follow-up in the plugin packages themselves.

SET search_path = public;

ALTER TABLE concurrency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_update_requests ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_concurrency_keys ON concurrency_keys;
CREATE POLICY tenant_isolation_concurrency_keys ON concurrency_keys
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_update_requests ON workflow_update_requests;
CREATE POLICY tenant_isolation_update_requests ON workflow_update_requests
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

-- FORCE, matching 001_schema.sql's reasoning for the other eight tables:
-- without it, the table owner (the role migrations run as, which is
-- normally also the role the worker connects as absent 005_app_role.sql)
-- is exempt from its own policies.
ALTER TABLE concurrency_keys FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_update_requests FORCE ROW LEVEL SECURITY;
