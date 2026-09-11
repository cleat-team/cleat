-- cleat migration 058 (postgres): record the concurrency key a run wants
--
-- cleat#1186. The user's decision was to DEFER a start blocked by a
-- concurrency key rather than reject it with 409, "so as not to be
-- gratuitously different from DBOS", where a limit throttles how many run at
-- once rather than whether a request is accepted.
--
-- WHY THIS NEEDS A COLUMN AT ALL, which was not obvious and is the reason the
-- change is larger than the issue assumed.
--
-- Today the key is acquired by the HTTP handler, after StartNewRun has already
-- created the run: on failure the losing run is TERMINATED and 409 returned.
-- The comment beside that call states the constraint plainly -- "the HTTP layer
-- is the only enforcement point for Cleat-Concurrency-Key; ClaimWorkflows does
-- not consult concurrency_keys".
--
-- Deferral means acquiring at DISPATCH time instead of at request time. For the
-- dispatcher to do that it has to know which key a waiting run wants, and
-- nothing records it: concurrency_keys maps key -> the run that HOLDS it, which
-- is the opposite direction. A run waiting for a key it does not hold appears
-- in no table.
--
-- So: the run carries its own key, and the claim path tests it.
--
-- NULLABLE, and NULL is not "no key" by accident -- it is the overwhelming
-- majority. Most runs have no concurrency key at all, and the claim path's
-- filter short-circuits on IS NULL before it looks at concurrency_keys, so the
-- common case pays one null test rather than a subquery.
--
-- THE JOIN IS ON THE HASH, AND THAT WAS THE THIRD DESIGN. The first two each
-- had a defect worth recording, because both looked obviously right:
--
--   1. Hash the row's key in SQL. PostgreSQL can: digest(w.concurrency_key,
--      'sha256'). SQL Server's HASHBYTES over NVARCHAR hashes UTF-16, and
--      key_hash was computed in Go over UTF-8, so the two NEVER match -- the
--      filter would exclude nothing on SQL Server, every deferred run would be
--      claimed at once, and the feature would be silently absent on one dialect
--      while passing on another.
--
--   2. Join on key_text, which concurrency_keys already stores, so no hashing
--      anywhere. key_text is TEXT on MySQL and NVARCHAR(MAX) on SQL Server:
--      neither can be an index key column. SQL Server says so as Msg 1919 and
--      MySQL silently declines. An unindexed join on the dispatcher's hot path
--      is not a design, it is a scan per claim.
--
--   3. Store the hash on the run, computed in Go by the same function the
--      acquire uses, and join on it. key_hash is the PRIMARY KEY of
--      concurrency_keys on all three dialects, so this is a PK probe with no
--      new index, no encoding question, and one hashing implementation rather
--      than two that must agree.
--
-- The text is kept alongside it. A WAITING run does not appear in
-- concurrency_keys at all -- that table records holders -- so without the text
-- on the row there is no way to answer "what is this run waiting for", which is
-- the question an operator asks first. See also cleat#1172.
ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS concurrency_key TEXT,
    ADD COLUMN IF NOT EXISTS concurrency_key_hash BYTEA;

-- Partial: only rows that actually want a key are candidates for the join, and
-- on a deployment where few runs use one this index stays small enough to be
-- worth having on the hot path. The claim path filters `status` first, so this
-- exists to make the concurrency test cheap for the rows that survive that.
CREATE INDEX IF NOT EXISTS idx_instances_concurrency_key
    ON workflow_instances (tenant_id, concurrency_key_hash)
    WHERE concurrency_key_hash IS NOT NULL;


COMMENT ON COLUMN workflow_instances.concurrency_key IS
    'The Cleat-Concurrency-Key this run was started with, or NULL. Recorded so the '
    'dispatcher can test it at claim time: a run whose key is held by another live '
    'run is not claimable and waits, rather than being rejected at the request '
    'boundary. See cleat#1186.';
