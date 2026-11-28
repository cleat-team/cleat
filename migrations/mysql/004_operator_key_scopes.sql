-- cleat#3552 (B2b-1): an operator key can be narrowed to a scope, and carries the
-- name of the principal it was issued to. See migrations/postgres/004 for the
-- full reasoning; this is the same change.
--
-- MUST BE SAFE TO RUN TWICE. --migrate-only re-applies the newest migration when its
-- tracking row is missing (TestMigrationIsADeployStepOnEveryDialect, step 6), and
-- a deploy interrupted between the ALTER and the tracking row does the same. MySQL 8
-- has no ADD COLUMN IF NOT EXISTS, so the guard is a throwaway procedure that asks
-- information_schema first. The first version of this file was a bare ALTER and the
-- Tier 1 gate failed it with Error 1060, Duplicate column name 'scopes'.
--
-- DDL is not transactional here, so both columns still go in ONE ALTER TABLE: a
-- single statement is atomic, which is what leaves no half-applied state, and why
-- guarding on `scopes` alone is enough to decide for `principal` too.
DROP PROCEDURE IF EXISTS cleat_migrate_004_operator_key_scopes;

DELIMITER ;;
CREATE PROCEDURE cleat_migrate_004_operator_key_scopes()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
         WHERE TABLE_SCHEMA = DATABASE()
           AND TABLE_NAME   = 'operator_api_keys'
           AND COLUMN_NAME  = 'scopes'
    ) THEN
        ALTER TABLE operator_api_keys
          ADD COLUMN scopes VARCHAR(255) NOT NULL DEFAULT 'admin',
          ADD COLUMN principal VARCHAR(128) NULL;
    END IF;
END ;;
DELIMITER ;

CALL cleat_migrate_004_operator_key_scopes();
DROP PROCEDURE cleat_migrate_004_operator_key_scopes;
