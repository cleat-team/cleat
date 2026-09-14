-- ===========================================================================
-- 076: workers are enumerable (admin.workers)
--
-- Why this exists
-- ---------------
-- cleat#1487: database connections are a cluster-global resource, and a
-- cluster-global policy cannot be apportioned to participants that cannot be
-- counted. Today a worker exists in the database ONLY as a value on rows it
-- currently holds -- `assigned_to` on workflow_instances, refreshed by
-- BatchHeartbeat, which is a per-WORKFLOW heartbeat rather than a per-WORKER
-- one.
--
-- That inverts exactly wrong for this purpose. A freshly started or idle
-- worker has already opened its fixed pools (75 connections on a default
-- single-node worker) and appears nowhere at all. The workers hardest to see
-- are precisely the ones consuming connections without doing work.
--
-- This table is membership, and nothing else: no budget arithmetic, no
-- policy, no behaviour change to any pool. Those are cleat#1487's second half
-- and are deliberately not here.
--
-- Liveness is lease + heartbeat + expiry, which is not a new concept in this
-- schema -- it is how workflow ownership already works, with a different
-- subject. A crashed worker's row expires by the same reasoning its claims do.
--
-- Not tenant-scoped, and that is a property rather than an omission
-- ----------------------------------------------------------------
-- A worker serves every tenant, so there is no tenant_id to carry and no row
-- that belongs to one tenant. It sits in `admin` alongside admin.tenants for
-- that reason. It gets no row-level security policy, in the same sense and for
-- the same reason that admin.tenants gets none: it is global BY DESIGN, not
-- unscoped by oversight. Those two look identical in a schema, so it is said
-- here rather than left to be inferred. Cf. plugin.Migration.TenantScoped,
-- whose doc draws the same distinction for plugin tables.
--
-- last_heartbeat_at carries an index because the only hot query over this
-- table is "how many workers are live", which is a range scan against a
-- cutoff. Writing it without the index would make the count cost grow with
-- every worker that has ever run rather than with the number running now.
-- ===========================================================================

CREATE TABLE IF NOT EXISTS admin.workers (
    worker_id         TEXT PRIMARY KEY,
    hostname          TEXT NOT NULL DEFAULT '',
    pid               INTEGER NOT NULL DEFAULT 0,
    concurrency       INTEGER NOT NULL DEFAULT 0,
    connection_budget INTEGER NOT NULL DEFAULT 0,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_workers_last_heartbeat
    ON admin.workers (last_heartbeat_at);
