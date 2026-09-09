-- cleat migration 055 (mysql): scope the workflow memory profile to a tenant
--
-- See migrations/postgres/055_the_memory_profile_is_scoped_to_a_tenant.sql for
-- the full finding (cleat#1040). In short: workflow_memory_stats and
-- workflow_memory_samples were keyed by def_name alone while workflow_defs is
-- keyed (tenant_id, name, version), so the EWMA blended the memory profile of
-- different code sharing a name, GET /api/definitions returned the blend
-- inside a tenant-scoped response, and CleanupMemorySamples deleted other
-- tenants' rows.
--
-- MySQL is the dialect where this matters most and the one with the least to
-- fall back on: it has no row-level security and no static tenant-predicate
-- guard (cleat#1031), so the predicate in the Go SQL is the entire isolation.
-- There is no second layer to add here, which is why the Go change and its
-- test carry the whole weight on this dialect.
--
-- Existing rows are discarded rather than attributed; the reasoning is in the
-- PostgreSQL file and applies unchanged.

-- Discard first: there is no owning row to derive a tenant from.
TRUNCATE TABLE workflow_memory_stats;
TRUNCATE TABLE workflow_memory_samples;

-- MySQL 8.0 has no ADD COLUMN IF NOT EXISTS. These run once, guarded by the
-- migration version table, in the same style as the other MySQL migrations.
ALTER TABLE workflow_memory_stats
    ADD COLUMN tenant_id CHAR(36) NOT NULL
    DEFAULT '00000000-0000-0000-0000-000000000000';

ALTER TABLE workflow_memory_samples
    ADD COLUMN tenant_id CHAR(36) NOT NULL
    DEFAULT '00000000-0000-0000-0000-000000000000';

-- One summary row per (tenant, name). The upsert's ON DUPLICATE KEY UPDATE
-- fires off this key, so repointing it is what makes the statement scope
-- correctly rather than merge two tenants into one row.
ALTER TABLE workflow_memory_stats DROP PRIMARY KEY;
ALTER TABLE workflow_memory_stats ADD PRIMARY KEY (tenant_id, def_name);

CREATE INDEX idx_memory_samples_tenant_def
    ON workflow_memory_samples (tenant_id, def_name, recorded_at DESC);
