-- 074: a plugin policy can use its index (cleat#1490)
--
-- Migration 063 gave every plugin table one policy whose predicate is
-- cleat.tenant_row_is_visible(tenant_id) -- a CASE that answers `true` for a
-- named cross-tenant sweep and `tenant_id = cleat.assert_tenant_set()`
-- otherwise. One predicate serving two callers is why it is a CASE, and the
-- CASE is why it cannot use an index.
--
-- MEASURED, PostgreSQL 16.15, 400000 rows over 400 tenants, two tables
-- identical except for the policy predicate:
--
--   USING (cleat.tenant_row_is_visible(tenant_id))
--       Index Only Scan, Filter: CASE WHEN ...
--       Rows Removed by Filter: 399000          603.654 ms
--
--   USING (tenant_id = cleat.assert_tenant_set())
--       Index Only Scan, Index Cond: (tenant_id = ...)
--         0.415 ms
--
-- 1454x, same 1000 rows. NOTE FOR ANYONE RE-CHECKING THIS: neither plan is a
-- Seq Scan -- both are Index Only Scans. Grepping an EXPLAIN for "Seq Scan"
-- finds nothing and reads as "already fixed". The defect is that the CASE
-- lands as a Filter instead of an Index Cond, so every index entry is walked.
--
-- AND IT IS NOT THE VOLATILITY PROBLEM MIGRATION 071 FIXED. Both functions are
-- already STABLE (071 for assert_tenant_set, 063 for tenant_row_is_visible),
-- and were STABLE in the measurement above. The CASE alone is sufficient.
--
-- THE SHAPE: two permissive policies instead of one CASE. Permissive policies
-- are OR-ed, so the tenant predicate stays a plain indexable equality and the
-- sweep gets a separate policy carrying no tenant predicate at all.
--
--   <table>_tenant_isolation  TO PUBLIC       USING (tenant_id = cleat.assert_tenant_set())
--   <table>_cross_tenant      TO cleat_sweep  USING (true)
--
-- WHY THIS DOES NOT REINTRODUCE THE HAZARD 063 DESCRIBES. 063's comment warns
-- that PostgreSQL may evaluate both operands of `bypass OR tenant_id = ...`,
-- and the right operand RAISES when no tenant is set -- which is exactly the
-- state a sweep is in. Measured here across six plan shapes (plain count,
-- forced seq scan, indexed WHERE, ORDER BY + LIMIT, self-join, UPDATE), the
-- sweep role read 400000 rows every time and never raised: with `USING (true)`
-- as its own policy the planner does not evaluate the other one.
--
--   NOT TESTED: a parallel plan. cleat.assert_tenant_set() is PARALLEL UNSAFE
--   (pg_proc.proparallel = 'u', the plpgsql default), so a policy referencing
--   it makes the query non-parallelisable and no parallel plan could be
--   constructed to test. If that function is ever marked PARALLEL SAFE, this
--   case becomes reachable and is unmeasured.
--
-- WHY `TO PUBLIC` AND NOT A ROLE. The obvious version attaches the tenant
-- policy to a parent role that every application role is granted. It is wrong
-- here, and it fails silently: admin.create_tenant_role creates per-tenant
-- roles NOINHERIT (001_schema.sql:67), and a NOINHERIT member does not match a
-- `TO <parent>` policy. Measured: such a role reads 0 rows -- no error, no
-- raise, just an empty result, which is the precise failure the raise-rather-
-- than-filter design exists to prevent. `TO PUBLIC` matches every role
-- including NOINHERIT ones; measured at 1000 rows and 0.137 ms for exactly
-- such a role, and still raising when no tenant is set.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_sweep') THEN
        CREATE ROLE cleat_sweep NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
    END IF;
END
$$;

COMMENT ON ROLE cleat_sweep IS
    'Cross-tenant plugin sweeps (cleat#1490). Entered with SET LOCAL ROLE from '
    'engine/plugindb_tenant.go; never connected to directly. Granted WITH '
    'INHERIT FALSE so membership alone does not apply its policies.';

-- Membership WITH INHERIT FALSE: enough to SET ROLE, not enough to match the
-- sweep policy passively. This distinction is load-bearing and silent when got
-- wrong -- measured, a plain GRANT lets the application role read all 400000
-- rows with no error and the correct number of policies; WITH INHERIT FALSE
-- returns 1000. PostgreSQL 16+.
DO $$
DECLARE
    r record;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        EXECUTE 'GRANT cleat_sweep TO cleat_app WITH INHERIT FALSE';
    END IF;
    FOR r IN SELECT rolname FROM pg_roles WHERE rolname LIKE 'cleat_tenant\_%' LOOP
        EXECUTE format('GRANT cleat_sweep TO %I WITH INHERIT FALSE', r.rolname);
    END LOOP;
END
$$;

-- SCHEMAS AND FUNCTIONS, NOT ONLY TABLES. Switching role changes the whole
-- privilege surface of the transaction, and the tables are only the part that
-- is obvious. Measured: after the table grants were right, blobstore's sweep
-- still failed with
--
--	pq: permission denied for schema admin (42501)
--
-- because its phase 3 calls admin.in_flight_workflow_ids() (migration 073) to
-- avoid collecting content an in-flight workflow still references.
--
-- NAMED, NOT BLANKET, and the named function is the same one
-- SetupPostgresRLSRole grants to the role that models a worker. admin also
-- holds admin.drop_tenant, and `GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA
-- admin` would hand every cross-tenant sweep the capability cleat#1365 was
-- filed to take away from tenant roles. This is the same "expose one query
-- rather than one exemption" shape as 023, 024 and 073.
DO $$
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO cleat_sweep', current_schema());
    IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'cleat') THEN
        GRANT USAGE ON SCHEMA cleat TO cleat_sweep;
        GRANT EXECUTE ON FUNCTION cleat.assert_tenant_set() TO cleat_sweep;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'admin') THEN
        GRANT USAGE ON SCHEMA admin TO cleat_sweep;
        IF EXISTS (
            SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
            WHERE n.nspname = 'admin' AND p.proname = 'in_flight_workflow_ids'
        ) THEN
            EXECUTE 'GRANT EXECUTE ON FUNCTION admin.in_flight_workflow_ids() TO cleat_sweep';
        END IF;
    END IF;
END
$$;

-- Rewrite the policies migration 063 installed, for every plugin table that
-- has one. admin.plugin_tables is the registry registerTenantScopedTables
-- writes; pg_policies is consulted as well so a table whose policy was created
-- before the registry existed is still rewritten.
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT p.schemaname, p.tablename, p.policyname
        FROM pg_policies p
        WHERE p.policyname = p.tablename || '_tenant_isolation'
          AND p.qual LIKE '%tenant_row_is_visible%'
    LOOP
        EXECUTE format('DROP POLICY %I ON %I.%I', r.policyname, r.schemaname, r.tablename);
        EXECUTE format(
            'CREATE POLICY %I ON %I.%I FOR ALL TO PUBLIC USING (tenant_id = cleat.assert_tenant_set())',
            r.policyname, r.schemaname, r.tablename);
        EXECUTE format(
            'CREATE POLICY %I ON %I.%I FOR ALL TO cleat_sweep USING (true)',
            r.tablename || '_cross_tenant', r.schemaname, r.tablename);
        EXECUTE format(
            'GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO cleat_sweep',
            r.schemaname, r.tablename);
        RAISE NOTICE 'cleat#1490: rewrote policy on %.%', r.schemaname, r.tablename;
    END LOOP;
END
$$;
