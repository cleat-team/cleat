-- cleat#3600 (owner D52): which plugin NAMES some worker has registered, at least once.
--
-- A deploy or a library install that declares plugin_deps is refused when a plugin it names
-- has never been registered by any worker. That needs a server-side fact, and none existed:
-- admin.workers has no plugin column, and cannot carry this one, because a worker's row is
-- deleted on a clean shutdown (Deregister) and by the expiry sweep (SweepExpired), so
-- "some worker registered it at least once" would collapse into "some worker is live now",
-- and a deploy would start failing on a rolling restart or a maintenance window. This table
-- is never deleted from by anything.
--
-- Keyed by NAME only, with first_seen_at and last_seen_at, so it grows with the number of
-- distinct plugins and not with the number of workers or versions. A plugin's VERSION stays a
-- claim-time check (checkPluginDeps), which is the only place a worker's own set is known.
--
-- Global, not per tenant: a worker process loads one plugin set for every tenant it serves,
-- and admin.workers, written by the same registration, is global too. In `admin`, so it is
-- shared by every --schema pool in a database, like admin.workers (see
-- migration/the_shared_schema_surface_is_a_declared_list_test.go).
--
-- The application role gets SELECT, INSERT and UPDATE, which is all the registration upsert
-- and the check need, and DELETE is REVOKED from it: "nothing deletes from this table" is a
-- privilege and not only a promise in this comment, so a future sweep written against it
-- fails with a permission error instead of quietly turning a deploy into "some worker is live
-- now". (The admin schema's default privileges would otherwise have granted DELETE too.)
CREATE TABLE IF NOT EXISTS admin.registered_plugins (
    plugin_name   text NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_registered_plugins PRIMARY KEY (plugin_name),
    CONSTRAINT ck_registered_plugins_name CHECK (length(plugin_name) BETWEEN 1 AND 128)
);

GRANT SELECT, INSERT, UPDATE ON admin.registered_plugins TO cleat_app;
REVOKE DELETE ON admin.registered_plugins FROM cleat_app;
