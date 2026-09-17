-- cleat migration 073 (mysql): every entity records when it last changed
--
-- cleat#1702, the MySQL half of postgres/085. See that file for why existing
-- rows are backfilled from created_at rather than stamped with the migration's
-- own clock, and for why nothing maintains the column yet.
--
-- NO SCHEMAS IN THIS DIALECT, so every table is named bare where Postgres says
-- `admin.` for three of them. Contract membership is matched on the bare name
-- for exactly this reason (cleat#1719).
--
-- MySQL HAS NO `ADD COLUMN IF NOT EXISTS`, so each step is a prepared statement
-- built from information_schema -- the idiom this tree already uses, including
-- in migration 072 next door. `DO 0` is the no-op branch.
--
-- TIMESTAMP(6) NOT NULL DEFAULT NOW(6), matching updated_at on tenant_settings
-- (039) and tenant_secrets (069) exactly. The explicit DEFAULT also keeps MySQL
-- from attaching its implicit `DEFAULT CURRENT_TIMESTAMP ON UPDATE
-- CURRENT_TIMESTAMP` to a first TIMESTAMP column -- which would have made this
-- dialect alone maintain the value, so the same schema would mean different
-- things on different engines.
--

SET @upd_tenant_api_keys := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_api_keys ADD COLUMN updated_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_api_keys'
      AND COLUMN_NAME = 'updated_at'
);
PREPARE upd_tenant_api_keys FROM @upd_tenant_api_keys; EXECUTE upd_tenant_api_keys; DEALLOCATE PREPARE upd_tenant_api_keys;
UPDATE tenant_api_keys SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE tenant_api_keys MODIFY COLUMN updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);

SET @upd_tenant_roles := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_roles ADD COLUMN updated_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_roles'
      AND COLUMN_NAME = 'updated_at'
);
PREPARE upd_tenant_roles FROM @upd_tenant_roles; EXECUTE upd_tenant_roles; DEALLOCATE PREPARE upd_tenant_roles;
UPDATE tenant_roles SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE tenant_roles MODIFY COLUMN updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);

SET @upd_tenant_egress_allow := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_egress_allow ADD COLUMN updated_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_egress_allow'
      AND COLUMN_NAME = 'updated_at'
);
PREPARE upd_tenant_egress_allow FROM @upd_tenant_egress_allow; EXECUTE upd_tenant_egress_allow; DEALLOCATE PREPARE upd_tenant_egress_allow;
UPDATE tenant_egress_allow SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE tenant_egress_allow MODIFY COLUMN updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);

SET @upd_workflow_defs := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_defs ADD COLUMN updated_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_defs'
      AND COLUMN_NAME = 'updated_at'
);
PREPARE upd_workflow_defs FROM @upd_workflow_defs; EXECUTE upd_workflow_defs; DEALLOCATE PREPARE upd_workflow_defs;
UPDATE workflow_defs SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE workflow_defs MODIFY COLUMN updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);

SET @upd_workflow_schedules := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_schedules ADD COLUMN updated_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_schedules'
      AND COLUMN_NAME = 'updated_at'
);
PREPARE upd_workflow_schedules FROM @upd_workflow_schedules; EXECUTE upd_workflow_schedules; DEALLOCATE PREPARE upd_workflow_schedules;
UPDATE workflow_schedules SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE workflow_schedules MODIFY COLUMN updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);

SET @upd_workflow_routing := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_routing ADD COLUMN updated_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_routing'
      AND COLUMN_NAME = 'updated_at'
);
PREPARE upd_workflow_routing FROM @upd_workflow_routing; EXECUTE upd_workflow_routing; DEALLOCATE PREPARE upd_workflow_routing;
UPDATE workflow_routing SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE workflow_routing MODIFY COLUMN updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);

SET @upd_workflow_tags := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE workflow_tags ADD COLUMN updated_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'workflow_tags'
      AND COLUMN_NAME = 'updated_at'
);
PREPARE upd_workflow_tags FROM @upd_workflow_tags; EXECUTE upd_workflow_tags; DEALLOCATE PREPARE upd_workflow_tags;
UPDATE workflow_tags SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE workflow_tags MODIFY COLUMN updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);

SET @upd_tenant_domains := (
    SELECT IF(COUNT(*) = 0,
        'ALTER TABLE tenant_domains ADD COLUMN updated_at TIMESTAMP(6) NULL DEFAULT NULL',
        'DO 0')
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'tenant_domains'
      AND COLUMN_NAME = 'updated_at'
);
PREPARE upd_tenant_domains FROM @upd_tenant_domains; EXECUTE upd_tenant_domains; DEALLOCATE PREPARE upd_tenant_domains;
UPDATE tenant_domains SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE tenant_domains MODIFY COLUMN updated_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6);
