-- cleat#1098 (postgres): close the RLS gap on the two workflow-memory tables --
-- workflow_memory_stats, workflow_memory_samples.
--
-- cleat#1040 / cleat#1096 gave both tables a tenant_id column, scoped all twelve
-- statements with an explicit `tenant_id = $N`, and bound both to
-- dbo.fn_tenant_filter on SQL Server (migrations/mssql/059). PostgreSQL got the
-- column and the Go predicate and NO POLICY, so these were tenant-scoped tables
-- carrying a single layer on the one dialect where a second is available.
--
-- ---------------------------------------------------------------------------
-- Why this could not be done in cleat#1096, and what changed.
--
-- All four PostgreSQL access sites read outside an RLS transaction --
-- RecordWorkflowMemorySample opened s.db.BeginTx, and LoadMemoryEstimates,
-- LoadMemoryStats and CleanupMemorySamples went straight to s.db.QueryContext /
-- s.db.ExecContext. A fail-closed `tenant_id = cleat.assert_tenant_set()` policy
-- raises the moment any of them runs, because cleat.tenant_id is set per
-- TRANSACTION by setRLSOnTx and none of them opened one.
--
-- The commit carrying this migration restructures all four onto
-- s.beginTxWithRLS, matching QueueDepth eighty lines away in the same file. That
-- restructuring is the change; the policy below is what it buys.
--
-- CleanupMemorySamples needed more than a swapped opener: its def listing and
-- its per-def DELETEs ran on two unrelated connections. They are now one
-- transaction, which is the only shape under which a per-transaction setting can
-- cover both.
--
-- ---------------------------------------------------------------------------
-- Why a policy at all, when the Go predicate already filters.
--
-- Because a single layer cannot be shown to work. cleat#1096 demonstrated the
-- opposite on SQL Server: with the Go predicates REMOVED, the isolation test
-- still passed, because the security policy alone held. PostgreSQL had no
-- equivalent property, and the same experiment there would have leaked every
-- other tenant's rows.
--
-- engine/memory_profile_rls_layer_test.go holds both halves apart and proves
-- each alone: the policy filtering a query that carries no tenant predicate,
-- and the Go predicate filtering over a superuser connection that the policy
-- cannot reach.
--
-- ---------------------------------------------------------------------------
-- Scope: these two tables only. This migration deliberately does not sweep the
-- remaining unprotected tenant-scoped tables. 031's header reasons about each
-- one it declined -- idempotency_keys is read before any RLS context exists,
-- admin.tenant_api_keys is read by the authenticator before a tenant is known,
-- kv_store and feature_flags are plugin-owned and not created by this file --
-- and none of those reasons has changed. A blanket apply would make this
-- migration a claim rather than a check.
ALTER TABLE workflow_memory_stats ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_memory_samples ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_memory_stats ON workflow_memory_stats;
CREATE POLICY tenant_isolation_memory_stats ON workflow_memory_stats
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_memory_samples ON workflow_memory_samples;
CREATE POLICY tenant_isolation_memory_samples ON workflow_memory_samples
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

-- FORCE, matching 001_schema.sql's reasoning for the eight tables it protects
-- and 031's for the two it added: without it the table owner -- the role
-- migrations run as, which is normally also the role the worker connects as
-- absent 005_app_role.sql -- is exempt from its own policies.
ALTER TABLE workflow_memory_stats FORCE ROW LEVEL SECURITY;
ALTER TABLE workflow_memory_samples FORCE ROW LEVEL SECURITY;
