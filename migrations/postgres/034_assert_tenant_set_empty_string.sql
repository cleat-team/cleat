-- cleat migration 034 (postgres): cleat.assert_tenant_set() must treat an empty
-- tenant id the same as an unset one.
--
-- The function raises when cleat.tenant_id is missing, so that a query reaching
-- an RLS-forced table without a tenant context fails loudly instead of
-- silently matching nothing. It tested only for NULL:
--
--     tid := current_setting('cleat.tenant_id', true);
--     IF tid IS NULL THEN
--         RAISE EXCEPTION 'cleat.tenant_id is not set -- ...';
--     END IF;
--     RETURN tid::uuid;
--
-- But engine's setRLSOnTx sets the GUC with
--
--     SELECT set_config('cleat.tenant_id', $1, true)
--
-- where the third argument is is_local: the setting is scoped to the
-- transaction and reset when it ends. Reset does not mean undefined. Once a
-- session has set the GUC even once, current_setting('cleat.tenant_id', true)
-- returns the EMPTY STRING on that connection rather than NULL, so the IS NULL
-- guard does not fire and control reaches `RETURN tid::uuid`, which fails with
--
--     invalid input syntax for type uuid: "" (22P02)
--
-- Measured 2026-09-01 on one pinned connection, before and after a single
-- set_config transaction:
--
--     fresh connection                    -> cleat.tenant_id is not set (P0001)
--     after one RLS transaction, same conn -> invalid input syntax for uuid (22P02)
--
-- The consequence is diagnostic, not a data-integrity problem: both cases
-- correctly refuse the query. But database connections come from a pool, so
-- which of the two errors a given query produces depends on whether that
-- particular connection has served an RLS transaction before -- which is to say
-- it is effectively random, and identical bug reports arrive wearing two
-- different error messages, one of which does not mention tenants at all. The
-- 22P02 form also reads like a data problem (a malformed uuid somewhere in the
-- caller's input) rather than what it is: a query that forgot its tenant
-- context.
--
-- CREATE OR REPLACE rather than DROP + CREATE: the function is referenced by
-- every tenant_isolation_* policy created in 001_schema.sql, and dropping it
-- would require dropping and recreating all of them.
--
-- This is a new migration rather than an edit to 001_schema.sql because
-- migration.Runner records applied migrations by name and never re-runs one, so
-- editing 001 in place would fix only databases created after the edit and
-- silently leave every existing deployment on the old definition.

CREATE OR REPLACE FUNCTION cleat.assert_tenant_set()
RETURNS uuid AS $$
DECLARE
    tid text;
BEGIN
    tid := current_setting('cleat.tenant_id', true);
    IF tid IS NULL OR tid = '' THEN
        RAISE EXCEPTION 'cleat.tenant_id is not set -- tenant context required for RLS-scoped query';
    END IF;
    RETURN tid::uuid;
END;
$$ LANGUAGE plpgsql;
