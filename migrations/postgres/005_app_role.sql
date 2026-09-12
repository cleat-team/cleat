-- cleat application role (005)
--
-- Creates the role the engine is meant to run as, and grants it exactly the
-- privileges it needs -- no more.
--
-- Why this exists
-- ---------------
-- Every tenant-scoped table has Row-Level Security enabled and FORCEd
-- (001_schema.sql), and for GetWorkflowByID and ListWorkflows those policies
-- are the *only* tenant isolation there is: neither carries an
-- application-level tenant_id filter. So whether cleat isolates tenants at all
-- comes down to whether the connecting role is subject to RLS.
--
-- PostgreSQL exempts two kinds of role, and the exemption is not something the
-- schema can close:
--
--   * a superuser -- unconditionally, and there is no way to force RLS onto
--     one. Not a setting, not an oversight: it is documented behaviour.
--   * the table owner -- unless FORCE ROW LEVEL SECURITY is set, which
--     001_schema.sql does. That gap is closed.
--
-- Which left the superuser gap wide open in every configuration cleat shipped.
-- docker-compose.cluster.yml connects as POSTGRES_USER=cleat, a superuser in
-- the official image; CI and local development connect as `postgres`. So the
-- policies were present, correct, tested -- and bypassed in practice by every
-- connection that ever ran against them.
--
-- Ownership matters as much as superuser here. cleat_app must NOT own these
-- tables: an owner is subject to RLS only while FORCE is set, so ownership
-- makes isolation depend on a flag that a later migration or a manual ALTER
-- could clear. A role that owns nothing is subject to RLS unconditionally.
--
-- The password
-- ------------
-- The role is created NOLOGIN and without a password, deliberately: a
-- credential does not belong in a file that is committed, mounted into
-- containers, and applied by every worker at boot. The deployment supplies it:
--
--   ALTER ROLE cleat_app LOGIN PASSWORD '...';
--
-- deploy/postgres/900-app-role.sh does this for docker-compose.cluster.yml
-- from CLEAT_APP_PASSWORD. Until something does, the role exists and is
-- granted but cannot connect, which fails closed.
--
-- Migrations still need an owner
-- ------------------------------
-- cleat_app cannot run migrations -- it has no DDL rights, by design. Workers
-- take --migrate-db (CLEAT_MIGRATE_DATABASE_URL) for that, and use --db for
-- everything after. See cmd/cleat-worker.

-- Unqualified names below resolve through search_path, set by the runner
-- rather than by this file since cleat#1287; see 001_schema.sql's header.
--
-- The GRANTs further down cannot be written that way -- GRANT ... ON SCHEMA
-- takes a name, not a search order -- so they ask for it with
-- current_schema() inside a DO block. That works in every applier of these
-- files without any of them having to substitute anything.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        -- NOBYPASSRLS is the default, but stating it makes the intent
        -- reviewable: this role must never be exempt from a policy.
        CREATE ROLE cleat_app
            NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END $$;

-- Re-assert the attributes on every run. A role that has drifted -- someone
-- granted SUPERUSER or BYPASSRLS to debug something and left it -- is exactly
-- the failure this migration exists to prevent, and re-applying the migrations
-- should correct it rather than preserve it.
ALTER ROLE cleat_app NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;

-- Schema access. USAGE only: no CREATE, so the role cannot come to own
-- anything in these schemas later.
DO $do$ BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO cleat_app', current_schema());
END $do$;
GRANT USAGE ON SCHEMA admin TO cleat_app;
GRANT USAGE ON SCHEMA cleat TO cleat_app;

-- Data access. DML on everything the engine reads or writes, and nothing
-- structural.
DO $do$ BEGIN
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO cleat_app', current_schema());
END $do$;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA admin TO cleat_app;
DO $do$ BEGIN
    EXECUTE format('GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %I TO cleat_app', current_schema());
END $do$;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA admin TO cleat_app;

-- Stored procedures: finalize_workflow_status, flush_event_step,
-- batch_flush_events, cleat.assert_tenant_set and the admin.* helpers.
DO $do$ BEGIN
    EXECUTE format('GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA %I TO cleat_app', current_schema());
END $do$;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA admin TO cleat_app;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA cleat TO cleat_app;

-- Objects created later -- by a plugin migration, or a future core migration
-- -- must be reachable too, or the role silently loses access to half the
-- system the first time a plugin adds a table. DEFAULT PRIVILEGES apply to
-- objects created by the role running this statement, which is the same owner
-- role that runs every migration.
DO $do$ BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO cleat_app', current_schema());
END $do$;
ALTER DEFAULT PRIVILEGES IN SCHEMA admin
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO cleat_app;
DO $do$ BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT USAGE, SELECT ON SEQUENCES TO cleat_app', current_schema());
END $do$;
DO $do$ BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT EXECUTE ON FUNCTIONS TO cleat_app', current_schema());
END $do$;
ALTER DEFAULT PRIVILEGES IN SCHEMA admin
    GRANT EXECUTE ON FUNCTIONS TO cleat_app;
