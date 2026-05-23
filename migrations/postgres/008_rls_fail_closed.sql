-- 008_rls_fail_closed.sql: Replace fail-open COALESCE with fail-closed assert function.

-- (1) Create the assert function. Throws if cleat.tenant_id is not set.
CREATE OR REPLACE FUNCTION cleat.assert_tenant_set()
RETURNS uuid AS $$
DECLARE
    tid text;
BEGIN
    tid := current_setting('cleat.tenant_id', true);
    IF tid IS NULL THEN
        RAISE EXCEPTION 'cleat.tenant_id is not set — tenant context required for RLS-scoped query';
    END IF;
    RETURN tid::uuid;
END;
$$ LANGUAGE plpgsql;

-- (2) Recreate all 5 RLS policies with the assert function instead of COALESCE.
DROP POLICY IF EXISTS tenant_isolation_defs ON workflow_defs;
CREATE POLICY tenant_isolation_defs ON workflow_defs
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_instances ON workflow_instances;
CREATE POLICY tenant_isolation_instances ON workflow_instances
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_events ON event_history;
CREATE POLICY tenant_isolation_events ON event_history
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_signals ON workflow_signals;
CREATE POLICY tenant_isolation_signals ON workflow_signals
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_schedules ON workflow_schedules;
CREATE POLICY tenant_isolation_schedules ON workflow_schedules
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());
