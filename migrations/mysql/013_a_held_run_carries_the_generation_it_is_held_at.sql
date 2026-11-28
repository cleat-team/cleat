-- cleat#4275: a run held in memory through a wait records the generation it is held at. See migrations/postgres/012 for the full reasoning; this is the same change on
-- workflow_instances, which is where MySQL keeps the lease columns.
--
-- MUST BE SAFE TO RUN TWICE (see 004): MySQL 8 has no ADD COLUMN IF NOT EXISTS, so the guard is a throwaway procedure that asks information_schema first.
--
-- No index: the held arm of the reclaim keeps the ordinary `heartbeat_at < NOW(6) - INTERVAL ...` range predicate at the shorter interval, which the existing
-- status/heartbeat_at index serves; held_generation is filtered on the rows that range returns.
DROP PROCEDURE IF EXISTS cleat_migrate_013_held_generation;

DELIMITER ;;
CREATE PROCEDURE cleat_migrate_013_held_generation()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
         WHERE TABLE_SCHEMA = DATABASE()
           AND TABLE_NAME   = 'workflow_instances'
           AND COLUMN_NAME  = 'held_generation'
    ) THEN
        ALTER TABLE workflow_instances ADD COLUMN held_generation BIGINT NULL;
    END IF;
END ;;
DELIMITER ;

CALL cleat_migrate_013_held_generation();
DROP PROCEDURE cleat_migrate_013_held_generation;
