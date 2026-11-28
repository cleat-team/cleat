-- cleat#3629 (owner decision D59, part 2 of 3): which published library, if any, owns a workflow name.
--
-- A deploy refuses a name that starts with a published library's prefix "<library>.". On SQL Server the
-- deploying login (cleat_admin, which cleatctl and the cross-tenant tooling deploy through) cannot read
-- the library schema, so a check that SELECTed library.bundles itself failed every deploy through it;
-- see migrations/mssql/009. On PostgreSQL nothing failed: 005 gives the library to the application role,
-- the sweeper and the default tenant's role, and a per-tenant role from admin.create_tenant_role CAN read
-- it too (measured). This function is here so that both dialects answer the question the same way, and so
-- that the check does not depend on the library grant reaching every role that deploys.
--
-- The check asks this function instead. It is SECURITY DEFINER, so the library schema stays closed to
-- the deploying role, and it answers one question for one name: the NAME of the library whose prefix
-- the argument starts with, or NULL. That is what the refusal message says anyway, and under owner
-- decision D45 any tenant may install any library, so the catalogue is not tenant-secret.
--
-- The comparison is the database's own equality (starts_with is exact on PostgreSQL), never LIKE: the
-- "_" a library name may contain is a wildcard there. search_path is pinned to pg_catalog and the table is
-- schema-qualified (library is not a configurable schema), so a caller's search_path cannot redirect
-- anything this function resolves. That is the stronger pin for a SECURITY DEFINER function and it is
-- possible here because the body needs only built-ins and one qualified table; the other readers in this
-- chain that must name workflow_defs unqualified use SET search_path FROM CURRENT instead.
--
-- It is created UNQUALIFIED, beside workflow_defs and the rest of the application tables, and called
-- unqualified. A role that can reach workflow_defs reaches it by the same search_path, which is the
-- whole requirement: the first draft put it in the cleat schema on the belief that every role already
-- had USAGE there, and a per-tenant login role (admin.create_tenant_role) does not ("permission
-- denied for schema cleat", measured with a deploy through such a role). EXECUTE stays with PUBLIC, the
-- default, for the same reason.
--
-- Idempotent: CREATE OR REPLACE.
CREATE OR REPLACE FUNCTION library_owning_prefix(p_name text) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog
    AS $$
    SELECT b.library
    FROM library.bundles b
    WHERE starts_with(p_name, b.library || '.')
    ORDER BY b.library
    LIMIT 1;
$$;
