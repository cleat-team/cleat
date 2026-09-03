-- cleat migration 038 (mysql): creating a second tenant fails, loudly
--
-- D1 in tiers.yaml: MySQL is single-tenant only. Until this file, that was
-- enforced by BREAKAGE AT THE POINT OF USE rather than by a check. A second
-- tenant could be created without complaint, MySQLStoreFactory would create it
-- a database on first use, nothing would ever apply a schema to that database
-- -- the worker migrates exactly one, the default tenant's, at boot -- and the
-- tenant would fail every operation with
-- "Table 'cleat_<uuid>.workflow_instances' doesn't exist". Measured 2026-09-03
-- through the factory: database created, 0 tables.
--
-- That is a bad failure in the specific way this repo cares about: it happens
-- far from the cause, it names a missing table rather than an unsupported
-- configuration, and it sends the reader into the schema instead of into
-- tiers.yaml.
--
-- The mechanism is a singleton unique key rather than a trigger, and the
-- reason is a MySQL restriction rather than a preference: a trigger may not
-- read the table it is defined on, so the obvious
-- "BEFORE INSERT ... IF (SELECT COUNT(*) FROM tenants) > 0 THEN SIGNAL"
-- is not expressible. A column that is always 1, with a UNIQUE index on it,
-- says the same thing declaratively and holds against ANY client -- the Go
-- API, cleatctl, a psql-equivalent session, a migration someone writes later.
-- There is no Go chokepoint to put this in even if one were wanted:
-- auth.TenantStore.CreateTenant has no non-test callers, and its SQL is
-- PostgreSQL-only in any case.
--
-- The index name is the error message. MySQL reports
--   Error 1062 (23000): Duplicate entry '1' for key
--   'tenants.uq_tenants_mysql_is_single_tenant_only_see_tiers_yaml_d1'
-- which names the rule and where the rule is written down. 56 characters,
-- inside MySQL's 64-character identifier limit.
--
-- Note for anyone editing this file: no comment here may contain a semicolon.
-- migration/runner.go splits MySQL migrations on the semicolon character with
-- no comment awareness. See IMPROVEMENT-PLAN 3.13.
--
-- Ordering is what makes this safe to apply to a live database. 002_defaults
-- seeds the single default tenant, and it runs long before this file, so the
-- column arrives on a table that already has exactly one row and the unique
-- index is satisfiable. On a database with more than one tenant row -- which
-- D1 says should not exist and which nothing supports -- the index creation
-- fails and the migration stops, which is the correct outcome: it refuses to
-- pretend a multi-tenant MySQL deployment is single-tenant.
--
-- INSERT IGNORE is the one caller that does not get an error, because MySQL
-- downgrades duplicate-key errors to warnings under IGNORE. That is exactly
-- what 002_defaults uses, so re-running migrations against a seeded database
-- stays idempotent -- and the row is still not created, which is the property
-- that matters.

SET @add_singleton := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenants ADD COLUMN singleton TINYINT NOT NULL DEFAULT 1',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenants'
      AND COLUMN_NAME = 'singleton'
);
PREPARE add_singleton FROM @add_singleton;
EXECUTE add_singleton;
DEALLOCATE PREPARE add_singleton;

SET @add_singleton_idx := (
    SELECT IF(COUNT(*) = 0,
        'CREATE UNIQUE INDEX uq_tenants_mysql_is_single_tenant_only_see_tiers_yaml_d1 ON tenants (singleton)',
        'DO 0')
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenants'
      AND INDEX_NAME = 'uq_tenants_mysql_is_single_tenant_only_see_tiers_yaml_d1'
);
PREPARE add_singleton_idx FROM @add_singleton_idx;
EXECUTE add_singleton_idx;
DEALLOCATE PREPARE add_singleton_idx;
