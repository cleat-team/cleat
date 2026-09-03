-- cleat migration 037 (postgres): put the tenant in the deployment-tag key
--
-- D7, decided 2026-09-02: names are per-tenant. IMPROVEMENT-PLAN 3.77 step 4,
-- and the last of the three tables that key on a user-chosen name. 035 did
-- workflow_defs, 036 did workflow_schedules.
--
-- What this closes, and it is a gap step 2 OPENED rather than one that predates
-- it. Before 035, two tenants could not both hold a definition called
-- "order-processor", so the question of both tagging it "stable" never arose.
-- After 035 they can hold the definition -- and could not tag it, because
-- workflow_tags still keyed on (workflow_name, tag) globally. Measured through
-- the store API on the shipped schema, one dialect at a time, and the three
-- answers were all different:
--
--   postgres  ERROR: new row violates row-level security policy (USING
--             expression) for table "workflow_tags" (42501)
--   mssql     Violation of PRIMARY KEY constraint 'pk_workflow_tags'.
--             The duplicate key value is (order-processor, stable).
--   mysql     no error at all -- see migrations/mysql/036 for what happens
--             instead, which is worse than either of these.
--
-- A tag is not data about a workflow: ResolveVersionByTag turns "stable" into
-- a version number at start time, so a tag decides WHICH CODE RUNS. That is
-- why this table is worth the same care as workflow_defs itself.
--
-- No foreign key references workflow_tags on any dialect. Re-derive with
--
--     grep -rEn 'REFERENCES [a-z]*\.?workflow_tags' migrations/*/*.sql
--
-- and keep the schema qualifier: without it the same pattern reports zero on
-- SQL Server, which 3.77 records having believed and written down.

-- ── The primary key ──────────────────────────────────────────────────────────
--
-- Declared inline in 001_schema.sql as PRIMARY KEY (workflow_name, tag), so
-- PostgreSQL names it after the table. Verified against a live database rather
-- than inferred:
--
--     SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
--     WHERE conrelid = 'workflow_tags'::regclass;
--     -> workflow_tags_pkey        | PRIMARY KEY (workflow_name, tag)
--        workflow_tags_def_fkey    | FOREIGN KEY (tenant_id, workflow_name, version)
--                                    REFERENCES workflow_defs(tenant_id, name, version)
--
-- Note the foreign key ALREADY carries the tenant -- 035 widened it when it
-- changed workflow_defs' key -- so there is nothing to drop and re-add here.
-- That is the whole reason this migration is three lines of DDL where 035 was
-- a catalogue loop.
--
-- Guarded on current shape; widening a key cannot fail on existing data.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_constraint c
        WHERE c.conrelid = 'workflow_tags'::regclass
          AND c.contype = 'p'
          AND array_length(c.conkey, 1) = 2
    ) THEN
        ALTER TABLE workflow_tags DROP CONSTRAINT workflow_tags_pkey;
        ALTER TABLE workflow_tags ADD PRIMARY KEY (tenant_id, workflow_name, tag);
    END IF;
END
$$;

-- ── What deliberately does NOT change ────────────────────────────────────────
--
-- tenant_isolation_tags is already `tenant_id = cleat.assert_tenant_set()` with
-- no default-tenant OR clause, so there is nothing to narrow -- unlike
-- workflow_defs, whose policy carried one for the adoption window 035 deleted.
--
-- The RLS policy is also what made PostgreSQL's failure above a 42501 rather
-- than a duplicate-key error: SetWorkflowTag's ON CONFLICT DO UPDATE tried to
-- update the other tenant's row, and the policy refused the result. Worth
-- knowing when reading that error, because "violates row-level security" reads
-- like a misconfiguration and was in fact the key being too narrow.
