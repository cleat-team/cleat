-- cleat migration 072 (mysql): one spelling of retirement for every entity
--
-- cleat#1702, the MySQL half of postgres/084. See that file for why the
-- contract spells retirement as a nullable timestamp rather than a boolean --
-- the class had four spellings of one concept and one of them, `enabled`, is
-- inverted relative to the other three.
--
-- MySQL HAS NO SCHEMAS, so `admin.tenant_api_keys`, `admin.tenant_roles` and
-- `admin.tenant_egress_allow` are plain `tenant_api_keys`, `tenant_roles` and
-- `tenant_egress_allow` here. That is the cleat#1719 finding: comparing
-- qualified names across dialects reports twelve differences, the same six
-- tables in both directions, every one spurious.
--
-- TIMESTAMP(6) NULL, matching this dialect's existing `revoked_at` and the
-- microsecond precision every other timestamp column here carries. NULL means
-- live, and there is no DEFAULT because there is no "unknown" state.
--
-- No backfill: the legacy columns are still the authority until each table's
-- own conversion migration moves the value across.
--
-- MySQL has no `ADD COLUMN IF NOT EXISTS`, so each statement is guarded through
-- information_schema and a prepared statement, which is this dialect's existing
-- idiom for an idempotent ADD COLUMN.
-- ===========================================================================

SET @add_tenant_api_keys := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_api_keys ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_api_keys'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_tenant_api_keys FROM @add_tenant_api_keys; EXECUTE add_tenant_api_keys; DEALLOCATE PREPARE add_tenant_api_keys;

SET @add_tenant_roles := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_roles ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_roles'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_tenant_roles FROM @add_tenant_roles; EXECUTE add_tenant_roles; DEALLOCATE PREPARE add_tenant_roles;

SET @add_tenant_egress_allow := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_egress_allow ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_egress_allow'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_tenant_egress_allow FROM @add_tenant_egress_allow; EXECUTE add_tenant_egress_allow; DEALLOCATE PREPARE add_tenant_egress_allow;

SET @add_workflow_defs := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_defs ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_defs'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_workflow_defs FROM @add_workflow_defs; EXECUTE add_workflow_defs; DEALLOCATE PREPARE add_workflow_defs;

SET @add_workflow_schedules := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_schedules ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_workflow_schedules FROM @add_workflow_schedules; EXECUTE add_workflow_schedules; DEALLOCATE PREPARE add_workflow_schedules;

SET @add_workflow_routing := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_routing ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_routing'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_workflow_routing FROM @add_workflow_routing; EXECUTE add_workflow_routing; DEALLOCATE PREPARE add_workflow_routing;

SET @add_workflow_tags := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_tags ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_tags'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_workflow_tags FROM @add_workflow_tags; EXECUTE add_workflow_tags; DEALLOCATE PREPARE add_workflow_tags;

SET @add_tenant_settings := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_settings ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_settings'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_tenant_settings FROM @add_tenant_settings; EXECUTE add_tenant_settings; DEALLOCATE PREPARE add_tenant_settings;

SET @add_tenant_secrets := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_secrets ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_secrets'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_tenant_secrets FROM @add_tenant_secrets; EXECUTE add_tenant_secrets; DEALLOCATE PREPARE add_tenant_secrets;

SET @add_tenant_domains := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_domains ADD COLUMN disabled_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_domains'
      AND COLUMN_NAME = 'disabled_at'
);
PREPARE add_tenant_domains FROM @add_tenant_domains; EXECUTE add_tenant_domains; DEALLOCATE PREPARE add_tenant_domains;
