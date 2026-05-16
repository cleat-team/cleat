-- cleat postgres default data

INSERT INTO admin.tenants (tenant_id, name, display_name)
    VALUES ('00000000-0000-0000-0000-000000000000', 'default', 'Default Tenant')
    ON CONFLICT (tenant_id) DO NOTHING;
UPDATE workflow_defs SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_instances SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE event_history SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_signals SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
UPDATE workflow_schedules SET tenant_id = '00000000-0000-0000-0000-000000000000' WHERE tenant_id IS NULL;
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT tenant_id FROM admin.tenants LOOP
        IF NOT EXISTS (SELECT 1 FROM admin.tenant_roles WHERE tenant_id = t.tenant_id) THEN
            BEGIN
                PERFORM admin.create_tenant_role(t.tenant_id);
            EXCEPTION WHEN OTHERS THEN
                RAISE WARNING 'create_tenant_role failed for tenant % (SQLSTATE: %) — skipping (single-tenant mode)', t.tenant_id, SQLSTATE;
            END;
        END IF;
    END LOOP;
END $$;
