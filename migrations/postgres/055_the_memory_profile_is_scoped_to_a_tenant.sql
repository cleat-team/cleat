-- cleat migration 055 (postgres): scope the workflow memory profile to a tenant
--
-- Bug (cleat#1040): workflow_memory_stats and workflow_memory_samples were
-- keyed by def_name alone --
--
--     def_name TEXT PRIMARY KEY                       -- stats
--     id BIGSERIAL PRIMARY KEY, def_name TEXT NOT NULL -- samples
--
-- with no tenant_id column on either, while workflow_defs is keyed
-- (tenant_id, name, version). A name therefore identifies DIFFERENT WASM per
-- tenant, and the EWMA blended the memory profile of unrelated code that
-- happened to share a name. `process_order` is the obvious case: likely
-- rather than exotic.
--
-- Three consequences, in increasing order of severity, all measured on all
-- three dialects by engine/memory_profile_tenant_scope_test.go:
--
--   1. The stored estimate is a blend. With the default alpha of 0.3, a
--      tenant that recorded 1 MB reads back 3.4 MB after another tenant
--      records 9 MB under the same name.
--   2. GET /api/definitions returns it. handleDefinitions opens with a
--      tenant-scoped store, lists that tenant's definitions through it, then
--      enriches each with LoadMemoryStats -- which reads a table with no
--      tenant column to scope on. The response is correct on the outer read
--      and unscoped on the enrichment.
--   3. CleanupMemorySamples is a cross-tenant DELETE. Retention keeps the N
--      most recent samples per NAME, so one tenant's sweep deletes another
--      tenant's rows. That is not observability; it is one tenant destroying
--      another's data, and it is the reason this is a fix rather than a
--      cleanup.
--
-- No guard could have caught it, and the reason is structural rather than an
-- oversight in the guard. engine/mssql_tenant_predicate_test.go derives its
-- universe from the migrations:
--
--     ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.<table>
--
-- so it answers "is every statement against a KNOWN tenant-scoped table
-- scoped?" and cannot answer "is every table that should be tenant-scoped
-- actually one?" A table with no tenant_id is not in its universe at all.
-- The MSSQL half of this change binds both tables to that policy, which puts
-- them inside the universe from now on -- worth more than the fix itself.
--
-- ---------------------------------------------------------------------------
-- Existing rows are DISCARDED rather than attributed, and that is a choice
-- with a cost. There is no owning row to derive a tenant from: unlike
-- migration 010's idempotency_keys, these tables reference nothing that
-- carries a tenant.
--
-- The alternative considered was defaulting existing rows to the default
-- tenant, which is what 010 did. It is better for the single-tenant majority
-- -- their accumulated statistics are correct and would be preserved -- and
-- wrong for everyone else, because on a multi-tenant deployment the existing
-- rows are precisely the blends this migration exists to eliminate, and
-- defaulting them stamps a blend with a tenant's name and keeps it forever.
--
-- Discard is the option that is never wrong. What a single-tenant deployment
-- loses is a decaying average that rebuilds itself: alpha 0.3 means a fresh
-- estimate is within 5% of steady state after ~9 samples, i.e. the first
-- handful of runs after the upgrade.
-- ---------------------------------------------------------------------------

-- Pin the creation target; see the note in 001_schema.sql.
SET LOCAL search_path TO public;

-- Discard before adding the column: see above. TRUNCATE rather than DELETE
-- because there is nothing to preserve and no trigger to fire.
TRUNCATE TABLE workflow_memory_stats;
TRUNCATE TABLE workflow_memory_samples;

ALTER TABLE workflow_memory_stats
    ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL
    DEFAULT '00000000-0000-0000-0000-000000000000';

ALTER TABLE workflow_memory_samples
    ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL
    DEFAULT '00000000-0000-0000-0000-000000000000';

-- The summary is one row per (tenant, name). Repointing the primary key is
-- the part that makes the upsert's ON CONFLICT target correct; without it
-- ON CONFLICT (tenant_id, def_name) has no unique index to match and the
-- statement errors rather than mis-scoping -- loud, which is the behaviour
-- we want if this migration is ever half-applied.
ALTER TABLE workflow_memory_stats DROP CONSTRAINT IF EXISTS workflow_memory_stats_pkey;
ALTER TABLE workflow_memory_stats ADD CONSTRAINT workflow_memory_stats_pkey
    PRIMARY KEY (tenant_id, def_name);

-- Samples keep their surrogate id and gain an index on the pair every read
-- and the retention sweep now filter by.
CREATE INDEX IF NOT EXISTS idx_memory_samples_tenant_def
    ON workflow_memory_samples (tenant_id, def_name, recorded_at DESC);
