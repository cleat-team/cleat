-- ===========================================================================
-- A worker publishes the secret keys it can open (admin.workers.secret_key_versions)
--
-- cleat#1991. NOTHING WRITES THIS COLUMN YET. It is added on its own, ahead of
-- the Go that uses it, so that the schema change lands before the migration
-- freeze (cleat#2059) and the rotation work (cleat#1991, PR 2) then lands on a
-- column that already exists. A column nothing writes is easy to mistake for
-- finished work; this is the first half of a change, and the second half is
-- named here so a reader can find it.
--
-- WHAT IT IS FOR
-- --------------
-- Rotating the tenant-secrets master key means workers hold different key sets
-- during a rolling deploy -- {old}, {old,new}, {new} -- while cleatctl
-- reseal-secrets rewrites rows. The invariant is that no serving worker ever
-- meets a row whose key_version it cannot open. specs/CleatKeyRotation.tla
-- (cleat#2121) checks it, and shows that two mechanisms are BOTH required:
--
--   a new worker meeting old rows   -> the worker refuses to start (boot check)
--   a new write meeting old workers -> the writer refuses version v unless every
--                                      live worker can open v
--
-- The second needs the writer to know what every live worker can open, and the
-- registry that already knows who the live workers are is admin.workers
-- (migration 076 / 065 / 069, cleat#1487): membership with a lease and a
-- heartbeat. This column is the missing fact.
--
-- THE THREE STATES OF THE VALUE, which are the reason it is nullable:
--
--   NULL     unknown -- the worker predates this column. A gate reading it must
--            treat this as "cannot say", never as "can open anything".
--   ""       the worker holds NO key. It can open no version, so it blocks every
--            write while it is live. That is correct: a keyless worker meeting a
--            row is exactly the state the invariant forbids.
--   "1,2"    the key_versions this worker can open, ascending, comma-separated.
--
-- Text rather than a child table or a JSON column: a ring holds a handful of
-- versions, the value is written once per registration and read by a writer
-- that then compares it against one integer, and one nullable text column is the
-- one shape that is identical on all three dialects.
--
-- No index. The only read is over the live rows of a table with one row per
-- worker process, and it is already bounded by last_heartbeat_at.
-- ===========================================================================

IF COL_LENGTH(N'admin.workers', N'secret_key_versions') IS NULL
    ALTER TABLE admin.workers
        ADD secret_key_versions NVARCHAR(255) NULL;
GO
