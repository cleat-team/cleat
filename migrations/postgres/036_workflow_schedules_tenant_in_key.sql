-- cleat migration 036 (postgres): put the tenant in the schedule key
--
-- D7, decided 2026-09-02: names are per-tenant. IMPROVEMENT-PLAN 3.77 step 3.
-- 035 did the same to workflow_defs; this is the second of the three tables
-- that key on a user-chosen name.
--
-- Simpler than 035, and the reason is worth stating rather than assumed: no
-- foreign key anywhere references workflow_schedules, on any dialect. Re-derive
-- with
--
--     grep -rEn 'REFERENCES [a-z]*\.?workflow_schedules' migrations/*/*.sql
--
-- and note the schema qualifier in that pattern is load-bearing -- the version
-- without it reports zero on SQL Server for workflow_defs, which is the error
-- 3.77 records having written into a heading, a PR description and a commit
-- message before checking.
--
-- What this fixes, measured through the ordinary store API before it was
-- written: two tenants each creating a schedule called 'nightly-report' is a
-- primary key violation on all three dialects. One tenant naming a schedule
-- takes that name away from every other tenant on the deployment, and the
-- error names a constraint rather than the problem.

-- ── The primary key ──────────────────────────────────────────────────────────
--
-- PostgreSQL names an inline column PRIMARY KEY after the table:
-- workflow_schedules_pkey. Verified against a live database rather than
-- inferred, because a hand-named constraint here would make the DROP silently
-- match nothing:
--
--     SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
--     WHERE conrelid = 'workflow_schedules'::regclass;
--     -> workflow_schedules_pkey | PRIMARY KEY (name)
--
-- Guarded on the current shape so re-running is a no-op, following 010's
-- precedent for idempotency_keys. Widening a key cannot fail on existing data.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_constraint c
        WHERE c.conrelid = 'workflow_schedules'::regclass
          AND c.contype = 'p'
          AND array_length(c.conkey, 1) = 1
    ) THEN
        ALTER TABLE workflow_schedules DROP CONSTRAINT workflow_schedules_pkey;
        ALTER TABLE workflow_schedules ADD PRIMARY KEY (tenant_id, name);
    END IF;
END
$$;

-- ── What deliberately does NOT change ────────────────────────────────────────
--
-- The RLS policy. tenant_isolation_schedules is already
-- `tenant_id = cleat.assert_tenant_set()` with no default-tenant OR clause --
-- unlike workflow_defs, which carried one for the adoption window 035 deleted.
-- There is nothing here to narrow.
--
-- idx_schedules_tenant_enabled (tenant_id, enabled, next_run_at) also stays.
-- It serves the due-schedule scan, which filters on enabled and next_run_at
-- and not on name, so the new primary key does not subsume it.
--
-- admin.get_due_schedules() (024) stays exactly as it is. It reads across
-- tenants by design and already returns tenant_id in its column list, which is
-- what the worker re-scopes the firing on -- so it needs no change to keep
-- telling two same-named schedules apart. Its RETURNS TABLE is the contract
-- with engine's scanDueSchedules; touching it here would drift that contract
-- for no gain.
