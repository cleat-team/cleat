-- cleat#3640 (owner D60-b): workflow-definition names are compared EXACTLY on MySQL, as on PostgreSQL and (since
-- migration 011, cleat#3635) SQL Server. Until now every one of these columns inherited the server default,
-- utf8mb4_0900_ai_ci, which ignores case AND accents: 'Acme.checkout' = 'acme.checkout' and 'résumé' = 'resume'.
--
-- WHICH COLLATION, MEASURED on MySQL 8.4.11 against PostgreSQL 16 on the same six probes (an identical-string
-- control returned equal on all five). PostgreSQL says "different" to every probe, and only utf8mb4_0900_bin does too:
--
--                                utf8mb4_0900_ai_ci   utf8mb4_0900_as_cs   utf8mb4_bin   utf8mb4_0900_bin   PostgreSQL
--   case (Acme / acme)                  equal               different         different      different       different
--   accent (résumé / resume)            equal               different         different      different       different
--   trailing space ('x' / 'x ')         different          different          EQUAL         different       different
--   NFC é / NFD e + U+0301              equal               EQUAL            different      different       different
--   zero-width space in 'ab'            equal               EQUAL            different      different       different
--   sharp s (Straße / Strasse)          equal               different         different      different       different
--
-- utf8mb4_bin is PAD SPACE, so it still equates 'x' with 'x '; utf8mb4_0900_as_cs equates canonically equivalent
-- strings and ignores a zero-width space, so two names that differ only invisibly would still collide. The issue
-- named those two; utf8mb4_0900_bin (NO PAD, byte comparison, MySQL 8.0.17 and later) is the one that matches PostgreSQL's
-- deterministic comparison on every probe. The CI image is 8.4.11.
--
-- A COLLATION CHANGE IS NOT RELIABLY IN-PLACE ON MYSQL, measured per column on 8.4.11: ALGORITHM=INPLACE was accepted
-- for some of these tables and refused for others (errors 1845, 1846 and 3780 all appeared), and a toy table accepted it
-- where event_history did not, so nothing here depends on it. Each ALTER asks for ALGORITHM=COPY, LOCK=SHARED outright:
-- the table is COPIED once with READS allowed and WRITES BLOCKED for the whole copy, and the cost grows with the table.
-- Measured on MySQL 8.4.11 with a 128 MB buffer pool, 20,000 definitions, 1,000,000 instances, 1,000,000 history events
-- and 390,000 rows in the other four tables (2.5 GB): 127 s and 135 s for the whole file, with no probe call failing; an
-- instance insert waited up to 66 s, an event insert up to 13 s, a definition read at most 0.8 s. Run it with workers
-- stopped, as every migration here is.
-- (SQL Server's equivalent is metadata-only; do not carry that expectation over.)
--
-- A foreign key between columns of different collations is refused (error 3780), so the three keys that carry a
-- definition name are dropped first and put back last (same columns, same NO ACTION rules).
--
-- DDL IS NOT TRANSACTIONAL ON MYSQL, unlike SQL Server's, so this file CANNOT roll back. What it does instead:
--   * the preflight is read-only and runs first: a refusal changes nothing;
--   * each ALTER TABLE is one statement, which InnoDB makes atomic, and every step asks information_schema first, so
--     a run that stopped anywhere converges when it is run again. THAT IS THE RECOVERY: run the migration again.
--   * what an interrupted run leaves behind, until it is run again: some of these ten columns converted and some not,
--     and possibly the three foreign keys missing. MEASURED, and QUIETER than one would expect: a comparison between a
--     converted and an unconverted name column does NOT fail (no error 1267); MySQL lets the binary collation win, so
--     the comparison is silently EXACT, while two unconverted columns still fold case and accent among themselves. With
--     the keys missing, nothing stops an orphan being written. Nothing announces any of it; it ends when the migration
--     completes, so a failed run must be re-run, not left.
--   * WHERE A FAILURE LANDS, MEASURED with a held shared lock and lock_wait_timeout=3. A lock on a table the foreign keys
--     tie to workflow_instances (event_history) stops the very first DROP FOREIGN KEY: ERROR 1205, NOTHING changed, all
--     ten columns still case-insensitive and all three keys present. A lock on a table nothing else references
--     (idempotency_keys) stops that table's ALTER late: ERROR 1205 with 8 of 10 columns converted, 0 of 3 keys present,
--     and an instance row naming a definition that does not exist WAS ACCEPTED in that window. Run again, it converged
--     (10 of 10, 3 of 3), and a third run on the finished database changed nothing. If such an orphan is still there when
--     it is run again, the preflight refuses ("no exact parent", nothing changed); delete or repair the row and re-run.
-- Same convention as 004: a throwaway procedure, because MySQL has no IF NOT EXISTS for any of this.
--
-- THE REFUSAL TEXT IS CAPPED AT 128 CHARACTERS, because SIGNAL's MESSAGE_TEXT is: a longer one is error 1648
-- ("Data too long for condition item"), which would REPLACE the readable refusal with an opaque one. It carries the
-- count and the first offender; the CHANGELOG entry has the query that lists every one.
--
-- NOT HERE: run ids (cleat#3639), and the tables plugins create at boot. MEASURED 2026-10-31 on a database that already
-- had this migration applied: schedules.workflow_name, event_subscriptions.def_name and task_queue.def_name are still
-- utf8mb4_0900_ai_ci, so a name compared there still folds case and accent. That is a plugin's own migration to change;
-- cleat#3658 tracks MySQL's (cleat#3641 is SQL Server's). Beyond those three, a scan of every column named
-- def_name, workflow_name, definition_name, child_name, workflow_def or def outside the ten found no other.
DROP PROCEDURE IF EXISTS cleat_migrate_012_workflow_names_exact;

DELIMITER ;;
CREATE PROCEDURE cleat_migrate_012_workflow_names_exact()
BEGIN
    DECLARE n BIGINT DEFAULT 0;
    DECLARE ex VARCHAR(300) DEFAULT '';
    DECLARE msg VARCHAR(500) DEFAULT '';

    -- 1a. FAIL CLOSED on a foreign key this file does not know. ALTER would stop on it with error 3780 or 1832,
    -- after some tables had already been converted; refuse before anything is touched.
    SELECT COUNT(*), MIN(CONCAT(kcu.CONSTRAINT_NAME, ' on ', kcu.TABLE_NAME)) INTO n, ex
      FROM information_schema.KEY_COLUMN_USAGE kcu
     WHERE kcu.TABLE_SCHEMA = DATABASE()
       AND kcu.REFERENCED_TABLE_NAME IS NOT NULL
       AND kcu.CONSTRAINT_NAME NOT IN ('fk_instances_def', 'fk_workflow_routing_def', 'fk_workflow_tags_def')
       AND ((kcu.TABLE_NAME = 'workflow_defs' AND kcu.COLUMN_NAME = 'name')
         OR (kcu.REFERENCED_TABLE_NAME = 'workflow_defs' AND kcu.REFERENCED_COLUMN_NAME = 'name')
         OR (kcu.TABLE_NAME IN ('workflow_instances', 'workflow_memory_samples', 'workflow_memory_stats', 'workflow_schedules', 'idempotency_keys') AND kcu.COLUMN_NAME = 'def_name')
         OR (kcu.TABLE_NAME IN ('workflow_routing', 'workflow_tags') AND kcu.COLUMN_NAME = 'workflow_name')
         OR (kcu.TABLE_NAME = 'event_history' AND kcu.COLUMN_NAME = 'child_name')
         OR (kcu.TABLE_NAME = 'plugin_dep_overrides' AND kcu.COLUMN_NAME = 'definition_name'));
    IF n > 0 THEN
        SET msg = LEFT(CONCAT('cleat#3640 refused, nothing changed; drop this foreign key, rerun: ', ex), 128);
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = msg;
    END IF;

    -- 1b. CASE- OR ACCENT-ONLY ORPHANS. A child that matches its parent under the old collation (so the key accepted
    -- it) and not under the new one. THROUGH A NARROW TEMP TABLE of definition names, for the reason cleat#3642 learned
    -- the hard way: comparing a child to the wide workflow_defs rows with a COLLATE on both sides cannot use an index
    -- and the optimiser may scan the definitions once per child row. Here every probe is a primary-key or
    -- secondary-key seek. Both spellings are stored with their collation explicit, so the answer does not depend on
    -- which columns a previous, interrupted run already converted.
    -- (workflow_routing.tenant_id is the one nullable key column, and a foreign key with a NULL component is not
    -- enforced at all, so a routing row with no tenant never matched a parent by the key and is not an orphan of it;
    -- it is left out of that check, as the key left it out.)
    DROP TEMPORARY TABLE IF EXISTS cleat_012_defnames;
    CREATE TEMPORARY TABLE cleat_012_defnames (
        tenant_id  CHAR(36)     COLLATE utf8mb4_0900_ai_ci NOT NULL,
        exact_name VARCHAR(255) COLLATE utf8mb4_0900_bin   NOT NULL,
        loose_name VARCHAR(255) COLLATE utf8mb4_0900_ai_ci NOT NULL,
        version    INT NOT NULL,
        PRIMARY KEY (tenant_id, exact_name, version),
        KEY k_loose (tenant_id, loose_name)
    );
    INSERT INTO cleat_012_defnames (tenant_id, exact_name, loose_name, version)
        SELECT tenant_id, name, name, version FROM workflow_defs;

    SELECT COUNT(*), MIN(CONCAT(c.def_name, '@', c.def_version)) INTO n, ex
      FROM workflow_instances c
     WHERE NOT EXISTS (SELECT 1 FROM cleat_012_defnames d
                        WHERE d.tenant_id = c.tenant_id AND d.version = c.def_version
                          AND d.exact_name = c.def_name COLLATE utf8mb4_0900_bin);
    IF n > 0 THEN
        SET msg = LEFT(CONCAT('cleat#3640 refused, nothing changed: no exact parent. ', n, ' in workflow_instances.def_name, first ', ex), 128);
        DROP TEMPORARY TABLE cleat_012_defnames;
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = msg;
    END IF;

    SELECT COUNT(*), MIN(CONCAT(c.workflow_name, '@', c.target_version)) INTO n, ex
      FROM workflow_routing c
     WHERE c.tenant_id IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM cleat_012_defnames d
                        WHERE d.tenant_id = c.tenant_id AND d.version = c.target_version
                          AND d.exact_name = c.workflow_name COLLATE utf8mb4_0900_bin);
    IF n > 0 THEN
        SET msg = LEFT(CONCAT('cleat#3640 refused, nothing changed: no exact parent. ', n, ' in workflow_routing.workflow_name, first ', ex), 128);
        DROP TEMPORARY TABLE cleat_012_defnames;
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = msg;
    END IF;

    SELECT COUNT(*), MIN(CONCAT(c.workflow_name, '@', c.version)) INTO n, ex
      FROM workflow_tags c
     WHERE NOT EXISTS (SELECT 1 FROM cleat_012_defnames d
                        WHERE d.tenant_id = c.tenant_id AND d.version = c.version
                          AND d.exact_name = c.workflow_name COLLATE utf8mb4_0900_bin);
    IF n > 0 THEN
        SET msg = LEFT(CONCAT('cleat#3640 refused, nothing changed: no exact parent. ', n, ' in workflow_tags.workflow_name, first ', ex), 128);
        DROP TEMPORARY TABLE cleat_012_defnames;
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = msg;
    END IF;

    -- 1c. A STORED-ONLY column that is still LOOKED UP. workflow_schedules.def_name has no foreign key,
    -- handleCreateSchedule stores it as typed, and a due schedule starts its workflow by that name: a schedule for
    -- 'checkout' under a definition named 'Checkout' works today and fails at every tick once the lookup is exact
    -- (cleat-review-69 measured this on the SQL Server migration). Refuse a row that matches a definition the way the OLD
    -- collation compares (utf8mb4_0900_ai_ci, forced so a re-run over converted columns still sees it) and matches none
    -- exactly. A schedule that names no definition in ANY case was already broken and is not refused. The other
    -- stored-only columns are converted without a check on purpose: idempotency_keys.def_name is compared in Go with
    -- `!=`, event_history.child_name is only written and read back, and plugin_dep_overrides is an append-only audit
    -- record that nothing looks up (re-derive: git grep -n -E 'plugin_dep_overrides|child_name|idempotency_keys' --
    -- '*.go' ':!*_test.go').
    -- (A MySQL temporary table cannot be named twice in one statement -- error 1137, "Can't reopen table" -- so the
    -- exact-spelling probe reads a second, narrower copy.)
    DROP TEMPORARY TABLE IF EXISTS cleat_012_exactnames;
    CREATE TEMPORARY TABLE cleat_012_exactnames (
        tenant_id  CHAR(36)     COLLATE utf8mb4_0900_ai_ci NOT NULL,
        exact_name VARCHAR(255) COLLATE utf8mb4_0900_bin   NOT NULL,
        PRIMARY KEY (tenant_id, exact_name)
    );
    INSERT INTO cleat_012_exactnames (tenant_id, exact_name) SELECT DISTINCT tenant_id, exact_name FROM cleat_012_defnames;
    SELECT COUNT(*), MIN(CONCAT(c.name, '=', c.def_name)) INTO n, ex
      FROM workflow_schedules c
     WHERE EXISTS (SELECT 1 FROM cleat_012_defnames d
                    WHERE d.tenant_id = c.tenant_id AND d.loose_name = c.def_name COLLATE utf8mb4_0900_ai_ci)
       AND NOT EXISTS (SELECT 1 FROM cleat_012_exactnames e
                        WHERE e.tenant_id = c.tenant_id AND e.exact_name = c.def_name COLLATE utf8mb4_0900_bin);
    DROP TEMPORARY TABLE cleat_012_exactnames;
    DROP TEMPORARY TABLE cleat_012_defnames;
    IF n > 0 THEN
        SET msg = LEFT(CONCAT('cleat#3640 refused (nothing changed): fix case/accent, rerun. ', n, ' in workflow_schedules (name=def_name), first ', ex), 128);
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = msg;
    END IF;

    -- 2. Drop the three keys that carry a definition name (guarded, so a run that already dropped them is fine).
    IF EXISTS (SELECT 1 FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances' AND CONSTRAINT_NAME = 'fk_instances_def' AND CONSTRAINT_TYPE = 'FOREIGN KEY') THEN
        ALTER TABLE workflow_instances DROP FOREIGN KEY fk_instances_def;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_routing' AND CONSTRAINT_NAME = 'fk_workflow_routing_def' AND CONSTRAINT_TYPE = 'FOREIGN KEY') THEN
        ALTER TABLE workflow_routing DROP FOREIGN KEY fk_workflow_routing_def;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_tags' AND CONSTRAINT_NAME = 'fk_workflow_tags_def' AND CONSTRAINT_TYPE = 'FOREIGN KEY') THEN
        ALTER TABLE workflow_tags DROP FOREIGN KEY fk_workflow_tags_def;
    END IF;

    -- 3. The columns, one table at a time, the key-bearing group first and the largest tables last. Type and
    -- nullability are exactly what information_schema held when this was written (no defaults, no comments);
    -- only the collation changes. Each is skipped if it is already converted.
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_defs' AND COLUMN_NAME = 'name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE workflow_defs MODIFY name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_routing' AND COLUMN_NAME = 'workflow_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE workflow_routing MODIFY workflow_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_tags' AND COLUMN_NAME = 'workflow_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE workflow_tags MODIFY workflow_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances' AND COLUMN_NAME = 'def_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE workflow_instances MODIFY def_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_memory_stats' AND COLUMN_NAME = 'def_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE workflow_memory_stats MODIFY def_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_memory_samples' AND COLUMN_NAME = 'def_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE workflow_memory_samples MODIFY def_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_schedules' AND COLUMN_NAME = 'def_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE workflow_schedules MODIFY def_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'plugin_dep_overrides' AND COLUMN_NAME = 'definition_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE plugin_dep_overrides MODIFY definition_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'idempotency_keys' AND COLUMN_NAME = 'def_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE idempotency_keys MODIFY def_name VARCHAR(255) COLLATE utf8mb4_0900_bin NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;
    IF (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'event_history' AND COLUMN_NAME = 'child_name') <> 'utf8mb4_0900_bin' THEN
        ALTER TABLE event_history MODIFY child_name VARCHAR(255) COLLATE utf8mb4_0900_bin NULL, ALGORITHM=COPY, LOCK=SHARED;
    END IF;

    -- 4. Put the keys back, definition for definition. Adding a key validates every existing row, which is the
    -- backstop for anything the preflight above did not see.
    IF NOT EXISTS (SELECT 1 FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances' AND CONSTRAINT_NAME = 'fk_instances_def' AND CONSTRAINT_TYPE = 'FOREIGN KEY') THEN
        ALTER TABLE workflow_instances ADD CONSTRAINT fk_instances_def FOREIGN KEY (tenant_id, def_name, def_version) REFERENCES workflow_defs (tenant_id, name, version);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_routing' AND CONSTRAINT_NAME = 'fk_workflow_routing_def' AND CONSTRAINT_TYPE = 'FOREIGN KEY') THEN
        ALTER TABLE workflow_routing ADD CONSTRAINT fk_workflow_routing_def FOREIGN KEY (tenant_id, workflow_name, target_version) REFERENCES workflow_defs (tenant_id, name, version);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_tags' AND CONSTRAINT_NAME = 'fk_workflow_tags_def' AND CONSTRAINT_TYPE = 'FOREIGN KEY') THEN
        ALTER TABLE workflow_tags ADD CONSTRAINT fk_workflow_tags_def FOREIGN KEY (tenant_id, workflow_name, version) REFERENCES workflow_defs (tenant_id, name, version);
    END IF;

    -- 5. FINISH BY ASSERTING, because the half-converted state is SILENT (measured: no error, a comparison between a
    -- converted and an unconverted column is quietly exact). Read every target column back from information_schema and
    -- every key, and fail loudly, naming the first one left behind. A run that gets here has converted everything; a
    -- run that does not has said which. The recovery is the same either way: run it again.
    SELECT COUNT(*), MIN(CONCAT(TABLE_NAME, '.', COLUMN_NAME)) INTO n, ex
      FROM information_schema.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE() AND COLLATION_NAME <> 'utf8mb4_0900_bin'
       AND ((TABLE_NAME = 'workflow_defs' AND COLUMN_NAME = 'name')
         OR (TABLE_NAME = 'workflow_instances' AND COLUMN_NAME = 'def_name')
         OR (TABLE_NAME = 'workflow_routing' AND COLUMN_NAME = 'workflow_name')
         OR (TABLE_NAME = 'workflow_tags' AND COLUMN_NAME = 'workflow_name')
         OR (TABLE_NAME = 'workflow_memory_samples' AND COLUMN_NAME = 'def_name')
         OR (TABLE_NAME = 'workflow_memory_stats' AND COLUMN_NAME = 'def_name')
         OR (TABLE_NAME = 'workflow_schedules' AND COLUMN_NAME = 'def_name')
         OR (TABLE_NAME = 'idempotency_keys' AND COLUMN_NAME = 'def_name')
         OR (TABLE_NAME = 'event_history' AND COLUMN_NAME = 'child_name')
         OR (TABLE_NAME = 'plugin_dep_overrides' AND COLUMN_NAME = 'definition_name'));
    IF n > 0 THEN
        SET msg = LEFT(CONCAT('cleat#3640 INCOMPLETE: ', n, ' name column(s) not utf8mb4_0900_bin, first ', ex, '. Run the migration again.'), 128);
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = msg;
    END IF;
    SELECT COUNT(*) INTO n
      FROM information_schema.TABLE_CONSTRAINTS
     WHERE CONSTRAINT_SCHEMA = DATABASE() AND CONSTRAINT_TYPE = 'FOREIGN KEY'
       AND CONSTRAINT_NAME IN ('fk_instances_def', 'fk_workflow_routing_def', 'fk_workflow_tags_def');
    IF n <> 3 THEN
        SET msg = LEFT(CONCAT('cleat#3640 INCOMPLETE: ', 3 - n, ' of the 3 definition-name foreign keys are missing. Run the migration again.'), 128);
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = msg;
    END IF;
END ;;
DELIMITER ;

CALL cleat_migrate_012_workflow_names_exact();
DROP PROCEDURE cleat_migrate_012_workflow_names_exact;
