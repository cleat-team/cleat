-- cleat migration 072 (postgres): a schedule carries the idempotency key that
-- created it, so a retry can be told from a name collision.
--
-- POST /api/schedules honours Idempotency-Key. cleat#1495.
--
-- The endpoint could not tell a retried request from a genuine name collision,
-- because it had nothing to tell them apart WITH: both arrive as a second
-- INSERT under a name that is already taken, and both were answered
-- `409 schedule_exists`. The key is the discriminator, and this is where it
-- lives.
--
-- WHY NOT idempotency_keys, WHICH ALREADY EXISTS. Three reasons, in ascending
-- order of how long they would take to undo:
--
--   * `idempotency_keys.workflow_id` is NOT NULL and a schedule has no workflow
--     id. Making it nullable to admit one means auditing every reader for a NULL
--     it has never seen -- 22 sites across 11 files, re-derive with
--     `git grep -n idempotency_keys -- 'engine/*.go' | grep -v _test | grep -c workflow_id`
--     -- to give one endpoint a capability.
--
--   * RETENTION WOULD HAVE TO BE TOLD WHAT A SCHEDULE'S KEY MEANS. cleat#1258
--     deletes a key with the workflow it names and cleat#1264 expires it on
--     `expires_at`. Both are right for a run, which is short-lived and has an
--     obvious end. A schedule is long-lived and has neither, so a key sitting in
--     that table would need a lifetime rule invented for it, and the sweeps
--     would need to learn the exception. On the schedule row the question does
--     not arise: the key lives exactly as long as the thing it created, and
--     `admin.drop_tenant` already deletes from workflow_schedules.
--
--   * A SEPARATE schedule_idempotency_keys TABLE avoids both and costs more: a
--     second copy of the mismatch comparison, the tenant cascade and the
--     retention policy that cleat#1169 and cleat#1494/#1496 have just finished
--     unifying. Two implementations of one policy, free to drift.
--
-- NULLABLE, AND NOT BACKFILLED. A schedule created before this migration was
-- created without a key, which is a fact about it rather than a gap -- the same
-- rule `*_a_duplicate_key_with_a_different_payload_is_refused.sql` sets for a
-- NULL request_digest, and the reason engine.checkIdempotencyInput reads a NULL
-- stored digest as UNKNOWN rather than as a mismatch. Inventing a key for an
-- existing row would make a request that never happened look like it had.
--
-- THE INDEX IS PARTIAL, AND THAT IS LOAD-BEARING RATHER THAN AN OPTIMISATION.
-- Most schedules have no key. A plain UNIQUE over (tenant_id, idempotency_key)
-- is fine on PostgreSQL, whose NULLs never collide, but the filter states the
-- intent for the reader and matches what SQL Server REQUIRES: there a unique
-- index treats NULLs as equal, so a second keyless schedule would be refused by
-- the index. See migrations/mssql/067.
--
-- The key is scoped by tenant, not global: two tenants retrying under the same
-- caller-chosen key are two unrelated requests, and a global unique index would
-- let one tenant's key deny another's -- a cross-tenant denial of service out of
-- a column that exists to make retries safe.

ALTER TABLE workflow_schedules ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE workflow_schedules ADD COLUMN IF NOT EXISTS request_digest TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_workflow_schedules_idempotency_key
    ON workflow_schedules (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
