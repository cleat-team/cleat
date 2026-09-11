-- cleat#1170: a duplicate key with a DIFFERENT payload silently discarded the
-- second request's input.
--
-- idempotency_keys already carries def_name (migration 051, cleat#1047) so a key
-- reused for another workflow definition is refused. It stops there. A second
-- request naming the SAME definition with different arguments matched the row,
-- was handed the first run's id with alreadyExisted = true, and its own input was
-- dropped without a word -- 200, a workflow id, and a run carrying somebody
-- else's arguments. That is quieter than the duplicate execution idempotency keys
-- exist to prevent, which at least leaves a row behind.
--
-- A DIGEST, not the input. The input can be large and is caller-controlled;
-- storing it again would duplicate workflow_instances.input for no gain, and the
-- only question asked of it is equality. engine.IdempotencyInputDigest
-- canonicalises the JSON before hashing, so key order and whitespace do not make
-- two identical requests disagree.
--
-- NULLABLE AND DELIBERATELY NOT BACKFILLED, which is where this differs from 051.
-- 051 could derive def_name from the owning workflow because a name is a name.
-- A digest is the output of a specific function, and computing it in SQL would be
-- a SECOND derivation of the same value -- free to disagree with the Go one over
-- JSON text rendering (PostgreSQL's jsonb output inserts a space after every
-- ":" and ",", MySQL and SQL Server do not) and to do so silently for a whole
-- upgrade. Existing rows keep NULL, the Go side reads NULL as "unknown, allow",
-- and the column starts meaning something from the first key written after the
-- upgrade.
SET @c := (SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
           AND TABLE_NAME = 'idempotency_keys' AND COLUMN_NAME = 'input_digest');
SET @d := IF(@c = 0,
    'ALTER TABLE idempotency_keys ADD COLUMN input_digest VARCHAR(64) NULL',
    'DO 0');
PREPARE s FROM @d; EXECUTE s; DEALLOCATE PREPARE s;
