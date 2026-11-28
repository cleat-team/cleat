-- cleat#3540 (PR 1 of 4): the schema for workflow libraries. A library is a set of
-- immutable, versioned workflow members that the cleat operator publishes once and
-- a tenant INSTALLS BY REFERENCE: its workflow_defs row keeps its own key and all
-- of its metadata, and only the bytes live elsewhere. PostgreSQL and SQL Server
-- only (owner decision D41); migrations/mysql has no 005.
--
-- Nothing here changes what a deploy or a run does. No row has a library yet, and
-- the CHECK below holds for every existing row (library IS NULL, wasm_bytes IS NOT
-- NULL). The readers that follow a reference, and the install and publish paths,
-- are later pull requests.
--
-- 1. THE LIBRARY SCHEMA IS OPERATOR-ONLY BECAUSE IT IS NEW. A table in the
-- application schema is writable by cleat_app through the default privileges
-- above; a table in a new schema gets nothing (measured on PostgreSQL 16.14:
-- has_table_privilege false, and no schema USAGE). So the library tables are in
-- their own schema, the app role is granted USAGE and SELECT only, and a separate
-- NOLOGIN role, cleat_library_publisher, gets INSERT and DELETE. "Only the cleat
-- operator publishes" (D40) is then a database grant, not a check a route might
-- forget. Do NOT add ALTER DEFAULT PRIVILEGES for this schema: that is what makes
-- admin's tables writable by the app role, and is the thing this avoids.
-- Nothing grants to the publisher role except this file (cleat#3552).
--
-- 2. members ARE IMMUTABLE. No UPDATE is granted to anyone, and wasm_sha256 and
-- wasm_length are checked against wasm_bytes on every insert, so a member's stored
-- digest cannot disagree with its bytes. A reader can then use the stored columns
-- instead of hashing a large value on every load.
--
-- 3. THE BYTES-OR-REFERENCE RULE IS A CHECK, NOT CODE. A workflow_defs row has
-- either its own bytes or a library reference, never both and never neither. That
-- also refuses the one writer that bypasses the engine: `cleat deploy --db` upserts
-- wasm_bytes straight into the table, and on a row with a library the CHECK
-- rejects it.
--
-- 4. THE FOREIGN KEY IS WHAT STOPS A DELETE. (library, name, version) references
-- library.members ON DELETE RESTRICT, so an operator cannot delete a member any
-- tenant still uses, with no scan and no count. It sees rows a tenant policy hides
-- (measured on PostgreSQL 16.14 under FORCE ROW LEVEL SECURITY as a non-superuser:
-- tenant A sees none of tenant B's rows and the delete still fails). The row's own
-- name and version are two of the three key parts, so one new column is enough.
-- With library NULL the key is not checked (MATCH SIMPLE).
--
-- 5. tenant_library_installs records which bundle of a library a tenant has
-- installed; an explicit upgrade replaces the row. It has tenant_id and a
-- row-level security policy like every tenant table, and drop_tenant removes it.
-- That needs admin.drop_tenant redefined from migrations/postgres/001 (its table
-- list is explicit on PostgreSQL), and admin.grant_core_tables_to_tenant_role,
-- so that a tenant role created later can read the library; roles that already
-- exist are granted below.
--
-- Every statement is idempotent, so a re-run after a failure elsewhere in the same
-- migration run is harmless.

CREATE SCHEMA IF NOT EXISTS library;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_library_publisher') THEN
        CREATE ROLE cleat_library_publisher
            NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END
$$;

CREATE TABLE IF NOT EXISTS library.members (
    library text NOT NULL,
    name text NOT NULL,
    version integer NOT NULL,
    wasm_bytes bytea NOT NULL,
    wasm_sha256 text NOT NULL,
    wasm_length bigint NOT NULL,
    abi_version integer NOT NULL,
    min_version integer DEFAULT 0 NOT NULL,
    plugin_deps jsonb DEFAULT '{}'::jsonb NOT NULL,
    entry_points text[] DEFAULT '{}'::text[] NOT NULL,
    task_queue text DEFAULT 'default'::text NOT NULL,
    max_history_length integer DEFAULT 0 NOT NULL,
    dag_spec jsonb,
    entry_point_schemas jsonb,
    exposure text DEFAULT 'auth'::text NOT NULL,
    input_validation_disabled boolean DEFAULT false NOT NULL,
    published_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    CONSTRAINT members_pkey PRIMARY KEY (library, name, version),
    CONSTRAINT ck_members_exposure CHECK (exposure = ANY (ARRAY['public'::text, 'auth'::text, 'internal'::text])),
    CONSTRAINT ck_members_digest CHECK (wasm_sha256 = encode(sha256(wasm_bytes), 'hex')),
    CONSTRAINT ck_members_length CHECK (wasm_length = length(wasm_bytes))
);

CREATE TABLE IF NOT EXISTS library.bundles (
    library text NOT NULL,
    bundle_version text NOT NULL,
    published_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    published_by text,
    CONSTRAINT bundles_pkey PRIMARY KEY (library, bundle_version)
);

-- Which members a bundle contains. A member that did not change between two bundles
-- is one members row listed by both. RESTRICT on both keys: a bundle or a member
-- that is listed cannot be deleted from under the listing.
CREATE TABLE IF NOT EXISTS library.bundle_members (
    library text NOT NULL,
    bundle_version text NOT NULL,
    name text NOT NULL,
    version integer NOT NULL,
    CONSTRAINT bundle_members_pkey PRIMARY KEY (library, bundle_version, name),
    CONSTRAINT bundle_members_bundle_fkey FOREIGN KEY (library, bundle_version)
        REFERENCES library.bundles (library, bundle_version) ON DELETE RESTRICT,
    CONSTRAINT bundle_members_member_fkey FOREIGN KEY (library, name, version)
        REFERENCES library.members (library, name, version) ON DELETE RESTRICT
);

-- The readers of the library are the application role, the sweeper, and the
-- default tenant's role; tenant roles created or existing are granted below and in
-- admin.grant_core_tables_to_tenant_role. Read only: no UPDATE for anyone.
GRANT USAGE ON SCHEMA library TO cleat_app;
GRANT USAGE ON SCHEMA library TO cleat_sweep;
GRANT USAGE ON SCHEMA library TO cleat_tenant_00000000_0000_0000_0000_000000000000;
GRANT SELECT ON library.members, library.bundles, library.bundle_members TO cleat_app;
GRANT SELECT ON library.members, library.bundles, library.bundle_members TO cleat_sweep;
GRANT SELECT ON library.members, library.bundles, library.bundle_members
    TO cleat_tenant_00000000_0000_0000_0000_000000000000;

-- The publisher writes the library and nothing else: no UPDATE, and no access to
-- any tenant table.
GRANT USAGE ON SCHEMA library TO cleat_library_publisher;
GRANT SELECT, INSERT, DELETE ON library.members, library.bundles, library.bundle_members
    TO cleat_library_publisher;

ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS library text;
ALTER TABLE workflow_defs ALTER COLUMN wasm_bytes DROP NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_workflow_defs_bytes_or_library'
                   AND conrelid = 'workflow_defs'::regclass) THEN
        ALTER TABLE ONLY workflow_defs
            ADD CONSTRAINT ck_workflow_defs_bytes_or_library
            CHECK ((library IS NULL) <> (wasm_bytes IS NULL));
    END IF;
    -- NOT VALID, so adding the key does not scan workflow_defs. Validating it does,
    -- as the table's owner, and FORCE ROW LEVEL SECURITY applies the tenant policy to
    -- that scan: with no tenant set it raises "cleat.tenant_id is not set" -- for a
    -- migrating role that is not a superuser (managed PostgreSQL; measured by
    -- TestTheMigrationSetAppliesWithoutASuperuser), on an EMPTY table too. Nothing is
    -- lost: the key is enforced for every later insert, update and delete of a member,
    -- and every existing row has library NULL, which a foreign key never checks.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_defs_library_fkey'
                   AND conrelid = 'workflow_defs'::regclass) THEN
        ALTER TABLE ONLY workflow_defs
            ADD CONSTRAINT workflow_defs_library_fkey
            FOREIGN KEY (library, name, version)
            REFERENCES library.members (library, name, version) ON DELETE RESTRICT
            NOT VALID;
    END IF;
END
$$;

-- What the foreign key's delete check scans: only the rows that reference a member.
CREATE INDEX IF NOT EXISTS idx_defs_library ON workflow_defs USING btree (library, name, version)
    WHERE (library IS NOT NULL);

CREATE TABLE IF NOT EXISTS tenant_library_installs (
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000000'::uuid NOT NULL,
    library text NOT NULL,
    bundle_version text NOT NULL,
    installed_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    installed_by text,
    CONSTRAINT tenant_library_installs_pkey PRIMARY KEY (tenant_id, library)
);

ALTER TABLE ONLY tenant_library_installs FORCE ROW LEVEL SECURITY;
ALTER TABLE tenant_library_installs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_library_installs ON tenant_library_installs;
CREATE POLICY tenant_isolation_library_installs ON tenant_library_installs
    USING ((tenant_id = cleat.assert_tenant_set()));

GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE tenant_library_installs TO cleat_app;
GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE tenant_library_installs
    TO cleat_tenant_00000000_0000_0000_0000_000000000000;

-- Tenant roles that already exist. The routine below covers the ones created
-- from now on.
DO $$
DECLARE
    v_role text;
BEGIN
    FOR v_role IN SELECT role_name FROM admin.tenant_roles LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role) THEN
            EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.tenant_library_installs TO %I', current_schema(), v_role);
            EXECUTE format('GRANT USAGE ON SCHEMA library TO %I', v_role);
            EXECUTE format('GRANT SELECT ON library.members, library.bundles, library.bundle_members TO %I', v_role);
        END IF;
    END LOOP;
END
$$;

-- admin.grant_core_tables_to_tenant_role and admin.drop_tenant, each as
-- migrations/postgres/001_schema.sql defines it, plus the one addition marked
-- "cleat#3540" in each.

CREATE OR REPLACE FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path FROM CURRENT
    AS $$
BEGIN
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_defs TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_instances TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_leases TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_payloads TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.event_history TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_signals TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_schedules TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_promises TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.idempotency_keys TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.concurrency_keys TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_update_requests TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_tags TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT ON %I.tenant_settings TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.queues TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.queue_holders TO %I', current_schema(), p_role_name);

    -- cleat#1918. The rate-limit counter table; granted beside queue_holders,
    -- its nearest sibling in shape.
    EXECUTE format('GRANT SELECT, INSERT, DELETE ON %I.queue_rate_tokens TO %I', current_schema(), p_role_name);

    -- cleat#3540. A workflow_defs read joins library.members, so a role that can
    -- read the one needs to read the other; the library tables are read-only here.
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.tenant_library_installs TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT USAGE ON SCHEMA library TO %I', p_role_name);
    EXECUTE format('GRANT SELECT ON library.members, library.bundles, library.bundle_members TO %I', p_role_name);

    EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA %I TO %I', current_schema(), p_role_name);
END;
$$;

CREATE OR REPLACE FUNCTION admin.drop_tenant(p_tenant_id uuid, p_schema text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
    AS $_$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_plugin_table RECORD;
    v_deleted BIGINT;
    v_core_table TEXT;
BEGIN
    IF p_schema IS NULL OR p_schema = '' THEN
        RAISE EXCEPTION 'admin.drop_tenant: p_schema is required -- it names the schema holding this deployment''s cleat tables, and must match the --schema cleat-worker was started with. Passing it is what stops the DELETEs resolving through the caller''s search_path (cleat#1363)';
    END IF;

    IF p_tenant_id = '00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'admin.drop_tenant: refusing to delete the default tenant (00000000-0000-0000-0000-000000000000) -- it is shared by every single-tenant deployment and by workflow_defs/plugin_defs, which are not tenant-owned data';
    END IF;

    IF to_regnamespace(p_schema) IS NULL THEN
        RAISE EXCEPTION 'admin.drop_tenant: schema % does not exist. Deleting nothing and reporting success is the failure this function was fixed to stop, so it refuses instead', p_schema;
    END IF;

    -- Makes every DELETE below correct regardless of whether the owning or
    -- executing role is a superuser.
    PERFORM set_config('cleat.tenant_id', p_tenant_id::text, true);

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    -- The seven core tables, in dependency order, each schema-qualified from
    -- p_schema. The order is 066's and is load bearing:
    --
    --   event_history       no FK/CASCADE back to workflow_instances (dropped
    --                       deliberately by 003_procedures.sql), so it must go
    --                       first or it is orphaned.
    --   workflow_instances  cascades workflow_signals, workflow_promises,
    --                       concurrency_keys and workflow_update_requests via
    --                       real ON DELETE CASCADE FKs.
    --   workflow_schedules  no FK to workflow_instances; own tenant_id.
    --   workflow_tags       FK to workflow_defs (NO ACTION), own tenant_id.
    --   workflow_routing    as workflow_tags.
    --   idempotency_keys    own tenant_id (010); result/error_msg hold a
    --                       workflow's actual output.
    --   workflow_defs       LAST of the seven: workflow_instances carries an FK
    --                       to it (NO ACTION), so it cannot be deleted while an
    --                       instance references it. By here there are none.
    --
    -- plugin_defs is deliberately absent: it has no tenant_id column at all
    -- (PRIMARY KEY (name, version)) and is genuinely shared.
    FOREACH v_core_table IN ARRAY ARRAY[
        'event_history',
        'workflow_instances',
        'workflow_schedules',
        'workflow_tags',
        'workflow_routing',
        'idempotency_keys',
        -- cleat#1644. Both carry tenant_id since 056 and neither has a foreign
        -- key to anything, so nothing cascaded them either: a dropped tenant's
        -- memory profile -- the names of the workflows it ran and how much
        -- memory each used -- survived indefinitely. Position in this array is
        -- free for exactly that reason; they are here rather than at the end so
        -- that workflow_defs stays visibly last.
        'workflow_memory_samples',
        'workflow_memory_stats',
        -- cleat#3540. Tenant-keyed, no foreign key to or from anything, so nothing
        -- cascades it: a dropped tenant's record of which library it had installed
        -- would survive. library.members and the other library tables are NOT here:
        -- they have no tenant_id and are shared by every tenant.
        'tenant_library_installs',
        'workflow_defs'
    ] LOOP
        IF to_regclass(format('%I.%I', p_schema, v_core_table)) IS NULL THEN
            RAISE EXCEPTION 'admin.drop_tenant: %.% does not exist. Either p_schema is wrong or this deployment is not fully migrated; either way, continuing would delete some of this tenant''s data and report success', p_schema, v_core_table;
        END IF;
        EXECUTE format('DELETE FROM %I.%I WHERE tenant_id = $1', p_schema, v_core_table)
            USING p_tenant_id;
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        RAISE DEBUG 'admin.drop_tenant: deleted % row(s) from %.%',
            v_deleted, p_schema, v_core_table;
    END LOOP;

    -- cleat#1289. Every table a plugin declared TenantScoped, which is the same
    -- declaration that gave it a row-level security policy. Schema-qualified
    -- from the registry, which is where a plugin table's schema is recorded --
    -- NOT from p_schema, because a plugin table need not live in the same
    -- schema as the core tables.
    FOR v_plugin_table IN
        SELECT schema_name, table_name
        FROM admin.plugin_tables
        WHERE tenant_scoped
        ORDER BY schema_name, table_name
    LOOP
        -- A registry row can outlive its table: a plugin is removed from the
        -- build, or its migration is reversed, and nothing deletes the row.
        -- Without this guard the first such row makes admin.drop_tenant raise
        -- and TENANT DELETION STOPS WORKING ENTIRELY, for every tenant.
        --
        -- A warning rather than silence, because the other reading of a missing
        -- table is a registration that recorded the wrong schema -- in which
        -- case rows DO survive, and the skip is the only evidence.
        IF to_regclass(format('%I.%I', v_plugin_table.schema_name,
                              v_plugin_table.table_name)) IS NULL THEN
            RAISE WARNING 'admin.drop_tenant: admin.plugin_tables names %.%, which does not exist -- skipping. If that table does exist under another schema, this tenant''s rows in it are NOT deleted.',
                v_plugin_table.schema_name, v_plugin_table.table_name;
            CONTINUE;
        END IF;

        EXECUTE format('DELETE FROM %I.%I WHERE tenant_id = $1',
                       v_plugin_table.schema_name, v_plugin_table.table_name)
            USING p_tenant_id;
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        RAISE DEBUG 'admin.drop_tenant: deleted % row(s) from %.%',
            v_deleted, v_plugin_table.schema_name, v_plugin_table.table_name;
    END LOOP;

    -- Must precede the admin.tenants delete: admin.tenant_api_keys' FK to
    -- admin.tenants has no ON DELETE clause (NO ACTION), so deleting
    -- admin.tenants first fails for any tenant that ever had a key issued.
    DELETE FROM admin.tenant_api_keys WHERE tenant_id = p_tenant_id;

    -- DROP ROLE refuses to drop a role that still holds privileges anywhere,
    -- and that error aborts the whole function -- rolling back every DELETE
    -- above with it, since a single CALL is one transaction. DROP OWNED BY
    -- strips the grants first. Guarded on existence because DROP OWNED BY has
    -- no IF EXISTS form and the role may legitimately not exist (single-tenant
    -- mode warns and skips role creation).
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        EXECUTE format('DROP OWNED BY %I', v_role_name);
    END IF;
    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_schema_name);
    EXECUTE format('DROP ROLE IF EXISTS %I', v_role_name);

    DELETE FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    DELETE FROM admin.tenants WHERE tenant_id = p_tenant_id;
END;
$_$;
