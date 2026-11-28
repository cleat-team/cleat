-- cleat#3629 (owner decision D59-b, part 3 of 3): which tenants already hold workflow names under a
-- prefix. A publish refuses when any do, listing each tenant and name, so the operator renames first.
--
-- The publisher role (cleat_library_publisher, 005) must not be able to read tenant rows, and under
-- FORCE ROW LEVEL SECURITY it cannot see another tenant's workflow_defs even if it were granted SELECT.
-- So the question goes to this SECURITY DEFINER function, which returns only (tenant, name, versions)
-- for names under the prefix and is executable by the publisher alone. It follows migration 009's
-- pattern and the existing cross-tenant readers (admin.get_due_schedules, admin.in_flight_workflow_ids):
-- created unqualified beside workflow_defs, search_path pinned FROM CURRENT.
--
-- HOW IT READS ACROSS TENANTS depends on its owner. An owner that bypasses row-level security (a
-- superuser or BYPASSRLS, which is what the readers above already rely on) reads workflow_defs
-- directly and sees every tenant. An owner that does not is subject to the tenant policy, so it sets
-- cleat.tenant_id for each tenant in admin.tenants (plus the default tenant) in turn, as
-- admin.drop_tenant does on SQL Server, and puts the setting back. That second path can only see tenants
-- that have an admin.tenants row; a tenant with workflow_defs rows and no admin.tenants row is not seen.
--
-- Only the application's OWN names count (library IS NULL): a row installed from a library is the
-- library's, not a tenant's name to rename. The comparison is the database's own (starts_with is exact
-- on PostgreSQL), never LIKE, where the "_" a library name may hold is a wildcard.
--
-- THE ARGUMENT IS VALIDATED because the publisher can call this directly, not only through the engine:
-- starts_with(name, '') is true of every name, so an unchecked prefix lets that role list every tenant's
-- names, one prefix at a time or all at once. It answers only for a library prefix: a library name (the
-- engine's grammar, at most 128 characters) followed by ONE ".". The publisher can still ask about a
-- library name it guesses, which is what this is for, and can no longer list names it has not guessed.
--
-- DELIBERATE RESIDUAL: a validated reader still answers for ANY library-shaped prefix, so the publisher
-- can list the tenants' names under a prefix it guesses ('payroll.') without publishing anything. That is
-- accepted: the publisher is an operator credential (owner decision D40) and D59-b shows the operator the
-- clash list on a refused publish anyway. The grammar check does not make the publisher unable to
-- enumerate. Tightening it to "the prefix of the library being published in this transaction" would need
-- a marker the engine sets, and the publisher holds the connection, so it could set the same marker.
--
-- Idempotent: CREATE OR REPLACE, and GRANT / REVOKE repeat harmlessly.
CREATE OR REPLACE FUNCTION library_clashing_names(p_prefix text)
    RETURNS TABLE (tenant_id uuid, name text, versions text)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path FROM CURRENT
    AS $$
DECLARE
    v_bypass boolean;
    v_prior  text;
    v_tenant uuid;
BEGIN
    IF p_prefix IS NULL OR length(p_prefix) > 129 OR p_prefix !~ '^[A-Za-z0-9][A-Za-z0-9_-]*\.$' THEN
        RAISE EXCEPTION 'library_clashing_names: % is not a library prefix (a library name of letters, digits, "_" and "-", then one ".")', p_prefix
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    SELECT r.rolsuper OR r.rolbypassrls INTO v_bypass FROM pg_catalog.pg_roles r WHERE r.rolname = current_user;
    IF v_bypass THEN
        RETURN QUERY
            SELECT d.tenant_id, d.name, string_agg(d.version::text, ',' ORDER BY d.version)
            FROM workflow_defs d
            WHERE d.library IS NULL AND starts_with(d.name, p_prefix)
            GROUP BY d.tenant_id, d.name
            ORDER BY d.tenant_id, d.name;
        RETURN;
    END IF;

    v_prior := current_setting('cleat.tenant_id', true);
    FOR v_tenant IN
        SELECT t.tenant_id FROM admin.tenants t
        UNION
        SELECT '00000000-0000-0000-0000-000000000000'::uuid
        ORDER BY 1
    LOOP
        PERFORM set_config('cleat.tenant_id', v_tenant::text, true);
        RETURN QUERY
            SELECT v_tenant, d.name, string_agg(d.version::text, ',' ORDER BY d.version)
            FROM workflow_defs d
            WHERE d.tenant_id = v_tenant AND d.library IS NULL AND starts_with(d.name, p_prefix)
            GROUP BY d.name
            ORDER BY d.name;
    END LOOP;
    PERFORM set_config('cleat.tenant_id', COALESCE(v_prior, ''), true);
    RETURN;
END;
$$;

REVOKE ALL ON FUNCTION library_clashing_names(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION library_clashing_names(text) TO cleat_library_publisher;
