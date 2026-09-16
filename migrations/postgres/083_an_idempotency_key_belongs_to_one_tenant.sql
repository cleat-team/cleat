-- 083: an idempotency key belongs to one tenant (cleat#1534)
--
-- idempotency_keys has carried tenant_id since migration 010 and an explicit
-- `AND tenant_id = $N` on every statement since. It has never carried a policy.
-- Migration 031 declined it, migration 061 repeated the decline, and both gave
-- the same reason -- the table is "read before any RLS context exists". That was
-- accurate: PostgresStore.startNewRun did its idempotency_keys work first and
-- called setRLSOnTx afterwards, so a fail-closed
-- `tenant_id = cleat.assert_tenant_set()` policy would have raised on all three
-- of its statements.
--
-- The commit carrying this migration moves those three onto transactions that
-- already have the tenant set. That restructuring is the change; the policy
-- below is what it buys -- the same shape as 061, which said so first.
--
-- ---------------------------------------------------------------------------
-- THE THREE STATEMENTS, and why a tenant predicate was not already enough.
--
--   store_lifecycle.go  the live-key lookup        s.db.QueryRowContext  -> tx
--                       the INSERT ... ON CONFLICT  tx, setRLSOnTx AFTER  -> tx
--                       the concurrent re-read      s.db.QueryRowContext  -> tx2
--
-- All three already carried `AND tenant_id = $2`. A policy is applied IN
-- ADDITION to a statement's own predicates, never instead of them, and
-- cleat.assert_tenant_set() raises the moment a candidate row is examined -- so
-- the predicate these statements carry is not what would have scoped them, and
-- would not have saved them either.
--
-- The middle one is the interesting case and the reason the guard in
-- engine/postgres_rls_reachability_test.go changed in the same commit. It ran
-- on a transaction, and that transaction had setRLSOnTx called on it -- eleven
-- lines LATER. The guard asked "does this function establish the tenant
-- anywhere in its body", which is a question this statement answers yes to
-- while being a fault. Measured against develop with idempotency_keys in the
-- RLS set, before any of this commit's changes to the code under test:
--
--   guard as it was     2 faults -- store_lifecycle.go:980, :1026
--   guard as it is now  3 faults -- and :1013, the INSERT
--
-- ---------------------------------------------------------------------------
-- THE TTL SWEEP IS CROSS-TENANT AND STAYS CROSS-TENANT.
--
-- cmd/cleat-worker's idempotencyCleanupLoop issues
--
--   DELETE FROM idempotency_keys WHERE expires_at < now()
--
-- with no tenant predicate, on the plain pool, deliberately: it collects every
-- tenant's expired keys on one tick. A fail-closed policy stops it dead. So the
-- sweep enters cleat_sweep for the duration of its transaction, which is the
-- shape migration 077 established and measured for the plugin tables:
--
--   idempotency_keys_tenant_isolation  TO PUBLIC       USING (tenant_id = cleat.assert_tenant_set())
--   idempotency_keys_cross_tenant      TO cleat_sweep  USING (true)
--
-- Two permissive policies rather than one CASE. 077's header carries the
-- measurements for why: the CASE form lands as a Filter instead of an Index
-- Cond (1454x on 400000 rows), and with `USING (true)` as its own policy the
-- planner does not evaluate the raising one.
--
-- ---------------------------------------------------------------------------
-- MEASURED, PostgreSQL 16.15, this policy, four seeded rows -- two tenants,
-- one expired and one live each. Seeded as a superuser (which bypasses RLS
-- unconditionally, FORCE included) and read back as cleat_app, which does not.
--
--   as cleat_app, no tenant, the sweep DELETE
--     ERROR:  cleat.tenant_id is not set -- tenant context required for RLS-scoped query
--
--   as cleat_app, cleat.tenant_id = A, SELECT with no tenant predicate
--     wf-a-exp, wf-a-live          2 of 4 -- B's two rows present and unseen
--
--   as cleat_app, SET LOCAL ROLE cleat_sweep, same SELECT
--     all 4                        and the raising policy is not evaluated
--   ... then DELETE FROM idempotency_keys WHERE expires_at < now()
--     DELETE 2                     both tenants' expired rows, neither live one
--
--   as cleat_app, cleat.tenant_id = A, INSERT a row for A
--     INSERT 0 1
--   ... the same INSERT naming tenant B
--     ERROR:  new row violates row-level security policy for table "idempotency_keys"
--
-- THE SECOND ROW IS WHY THERE ARE TWO TENANTS IN THE FIXTURE. A policy's USING
-- is a row-level predicate, so against an empty table it is never evaluated and
-- a read succeeds whether the policy is correct, wrong, or absent -- the same
-- empty green as connecting as a superuser, reached through a different door.
-- The rows that make it mean something are the ones the policy must EXCLUDE,
-- so B's are seeded and A's visibility is stated as a fraction. `DELETE 2` is
-- the same discipline on the sweep: 0 and 4 are both wrong, and `all clean`
-- could not have told them apart.
--
-- WHY NOT JUST SWEEP PER TENANT. It would work, and it is a different change:
-- the loop would have to enumerate tenants, and a tenant whose row is gone
-- would keep its keys forever. The sweep's cross-tenant reach is the property
-- cleat#1256 fixed and is worth keeping.
--
-- ---------------------------------------------------------------------------
-- SCOPE: PostgreSQL only. MySQL has no row-level security to add, and SQL
-- Server's idempotency_keys is untouched here -- its engine predicate surface
-- is dbo.fn_tenant_filter and binding this table to it is its own change, with
-- its own sweep problem (there is no SET ROLE; see markCrossTenantOnTx). Saying
-- so here rather than leaving the asymmetry to be discovered.

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL TO PUBLIC USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS idempotency_keys_cross_tenant ON idempotency_keys;
CREATE POLICY idempotency_keys_cross_tenant ON idempotency_keys
    FOR ALL TO cleat_sweep USING (true);

-- The sweep needs the privilege as well as the policy. A policy decides which
-- rows a role may see; the grant decides whether it may issue the statement at
-- all, and getting only the first gives `permission denied for table` -- which
-- reads as an RLS problem and is not one. Migration 077 learned this on schemas
-- and functions; this is the table half of the same lesson.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_sweep') THEN
        GRANT SELECT, DELETE ON idempotency_keys TO cleat_sweep;
    END IF;
END
$$;
