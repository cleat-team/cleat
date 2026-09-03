-- cleat migration 035 (postgres): put the tenant in the workflow-definition key
--
-- D7, decided 2026-09-02: names are per-tenant. "It doesn't make any sense for
-- one tenant to need to worry about clashes with some other tenant's
-- workflows." IMPROVEMENT-PLAN 3.77.
--
-- workflow_defs' primary key is (name, version), and definition names are
-- chosen by whoever deploys, so two customers both calling something
-- "order-processor" is ordinary rather than exotic -- and until now the second
-- one could not have it. 3.12 made taking a name loud instead of silent; this
-- makes it a non-event, because each tenant gets its own row.
--
-- This is the same shape as migration 010, which widened idempotency_keys'
-- primary key for the same reason one table over, and it is guarded the same
-- way: on the current shape rather than run unconditionally, because
-- deploy/postgres/100-apply-migrations.sh and engine/testutil both apply these
-- files against databases that may already have them.
--
-- Option 4 of the four in 3.77: change the key outright rather than backfill.
-- There is no deployed workflow_defs data to preserve, so the two options that
-- exist to preserve it -- adopt-on-evidence and fan-out-per-tenant -- buy
-- nothing here. What that choice additionally allows is the deletion of
-- canAdoptDef and the default-tenant adoption window, which exist ONLY because
-- (name, version) is a shared namespace needing an adjudicator. Under
-- (tenant_id, name, version) there is nothing to adjudicate. The read side of
-- that window is the policy clause at the bottom of this file.

SET search_path = public;

-- ── 1. Drop the foreign keys that point at the old key ───────────────────────
--
-- Found through the catalogue rather than named literally. PostgreSQL
-- auto-names an inline FOREIGN KEY (<table>_<cols>_fkey), and 001_schema.sql
-- declares all three inline, so the names are conventions rather than
-- guarantees. Dropping whatever actually references workflow_defs is both
-- shorter and correct against a database whose constraint was renamed.
DO $$
DECLARE
    c record;
BEGIN
    FOR c IN
        SELECT con.conname, cl.relname
        FROM pg_constraint con
        JOIN pg_class cl ON cl.oid = con.conrelid
        JOIN pg_class ref ON ref.oid = con.confrelid
        JOIN pg_namespace n ON n.oid = cl.relnamespace
        WHERE con.contype = 'f'
          AND n.nspname = 'public'
          AND ref.relname = 'workflow_defs'
    LOOP
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I', c.relname, c.conname);
    END LOOP;
END $$;

-- ── 2. Swap the primary key ──────────────────────────────────────────────────
--
-- Widening, not narrowing: every row that satisfied (name, version) satisfies
-- (tenant_id, name, version), so this cannot fail on existing data.
DO $$
DECLARE
    pk_columns integer;
BEGIN
    SELECT cardinality(c.conkey) INTO pk_columns
    FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    JOIN pg_namespace n ON n.oid = t.relnamespace
    WHERE n.nspname = 'public'
      AND t.relname = 'workflow_defs'
      AND c.contype = 'p';

    IF pk_columns IS NULL THEN
        ALTER TABLE workflow_defs
            ADD CONSTRAINT workflow_defs_pkey PRIMARY KEY (tenant_id, name, version);
    ELSIF pk_columns = 2 THEN
        ALTER TABLE workflow_defs DROP CONSTRAINT workflow_defs_pkey;
        ALTER TABLE workflow_defs
            ADD CONSTRAINT workflow_defs_pkey PRIMARY KEY (tenant_id, name, version);
    END IF;
END $$;

-- ── 3. Put the foreign keys back, with the tenant in them ────────────────────
--
-- All three referencing tables already carry tenant_id, so this is mechanical.
-- The constraint now says what it always meant: this instance runs a definition
-- *of its own tenant*, not merely a definition that exists.
--
-- No ON DELETE clause, matching what these carried before: NO ACTION, so
-- deleting a definition a running workflow still references is refused. That is
-- the protection worth keeping -- an operator cannot delete the code a workflow
-- is mid-replay of.
ALTER TABLE workflow_instances
    ADD CONSTRAINT workflow_instances_def_fkey
    FOREIGN KEY (tenant_id, def_name, def_version)
    REFERENCES workflow_defs(tenant_id, name, version);

ALTER TABLE workflow_tags
    ADD CONSTRAINT workflow_tags_def_fkey
    FOREIGN KEY (tenant_id, workflow_name, version)
    REFERENCES workflow_defs(tenant_id, name, version);

ALTER TABLE workflow_routing
    ADD CONSTRAINT workflow_routing_def_fkey
    FOREIGN KEY (tenant_id, workflow_name, target_version)
    REFERENCES workflow_defs(tenant_id, name, version);

-- ── 4. Close the read side of the adoption window ────────────────────────────
--
-- tenant_isolation_defs was
--
--     USING (tenant_id = cleat.assert_tenant_set()
--            OR tenant_id = '00000000-0000-0000-0000-000000000000')
--
-- so a definition owned by the default tenant was readable by EVERY tenant.
-- That was deliberate and is documented in 001_schema.sql: with a shared
-- (name, version) namespace, definitions deployed before per-tenant ownership
-- existed had to stay reachable or the first redeploy after an upgrade broke
-- for every tenant at once.
--
-- The namespace is no longer shared, so the exception has no job. Removing it
-- is what makes the key change mean something on the read path: without this,
-- tenant B still sees any default-tenant definition by name.
--
-- Note this is NOT a behaviour change for a single-tenant deployment: it runs
-- as the default tenant, so its rows still match the first clause.
DROP POLICY IF EXISTS tenant_isolation_defs ON workflow_defs;
CREATE POLICY tenant_isolation_defs ON workflow_defs
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());
