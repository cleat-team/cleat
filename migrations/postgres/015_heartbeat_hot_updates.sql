-- cleat#3245 Phase 2 (combined #3272 + #3274, owner decision 2026-10-09: "one
-- combined PR"). Measured on a synthetic population before this was written
-- (see cleat#3272's design-note comment): neither change below produces any
-- observable HOT-update or WAL benefit alone -- only together do they, a
-- measured ~44% HOT ratio and ~37% WAL reduction on a fresh batch of
-- heartbeats. Land them in the same migration for that reason, not out of
-- convenience.
--
-- THE MECHANISM. A Postgres UPDATE can only be HOT (Heap-Only Tuple -- the
-- new row version stays on the same page, and no index needs touching) if
-- (a) it changes no column any index on the table indexes, and (b) the page
-- has room for the new version. heartbeat_at is written on every single
-- heartbeat and was itself indexed by BOTH idx_instances_heartbeat and
-- idx_instances_stale, so (a) failed unconditionally; the default fillfactor
-- (100, pages packed as tight as the bulk loader can manage) meant (b) also
-- failed even once (a) was fixed. Fixing only one of the two leaves the
-- other blocking every heartbeat exactly as before -- confirmed by measuring
-- all four combinations, not by inference from the mechanism alone.
--
-- idx_instances_heartbeat's heartbeat_at column is confirmed dead weight:
-- its only identified consumer, HeartbeatBatchFenced's SELECT
-- (engine/db.go), filters on assigned_to and status alone and never
-- references heartbeat_at; EXPLAIN (ANALYZE, BUFFERS) against the narrowed
-- index produced an identical plan and cost.
--
-- idx_instances_stale's heartbeat_at column is NOT dead weight.
-- ReapStaleInstances and ListStaleHolders (engine/store_lifecycle.go)
-- genuinely range-scan and ORDER BY heartbeat_at, and dropping the column
-- forces a full scan of the 'running' set plus an in-memory sort instead --
-- measured 23x slower at 20,000 concurrently-running workflows than with
-- the index (0.24ms -> 5.56ms), and 19ms at 100,000 (parallel workers engage
-- and the cost grows sub-linearly past that point). Accepted deliberately:
-- the reap sweep runs on its own periodic timer, not on every heartbeat, so
-- a bounded tens-of-milliseconds cost that scales with the CONCURRENTLY
-- RUNNING population (itself bounded by provisioned worker capacity, not by
-- how large workflow_instances has grown historically) is a reasonable
-- trade against a WAL/HOT benefit that applies continuously, to every
-- heartbeat, of every running workflow, for as long as it runs. If the
-- concurrently-running population at a real deployment's scale makes this
-- cost a problem in practice, that is its own follow-up (a redesign of how
-- staleness is tracked, likely Phase 3 territory per cleat#3245's own
-- scoping -- not something to build speculatively here).
--
-- NO CONCURRENTLY: this migration runner wraps every file in one
-- transaction (migration/runner.go), and CREATE/DROP INDEX CONCURRENTLY
-- cannot run inside one -- structurally unavailable here, not an oversight
-- (see DefaultLockTimeout's doc comment in migration/runner.go for the
-- project's existing, deliberate tradeoff: an ACCESS EXCLUSIVE lock bounded
-- by a 30s timeout, rather than a lock-free rebuild). Both indexes are
-- PARTIAL (WHERE status = 'running'), so a rebuild's cost is bounded by the
-- concurrently-running population, not by workflow_instances' full size.
-- fillfactor is NOT retroactive: it only reserves free space on pages
-- written AFTER this setting takes effect, so an existing, already-packed
-- database sees no immediate HOT improvement -- the benefit ramps up
-- gradually as ordinary churn rewrites pages, or an operator can force it
-- immediately with VACUUM FULL workflow_instances during a maintenance
-- window (ACCESS EXCLUSIVE for the duration of the rebuild; do not run it
-- as part of this migration, which already takes a lock of its own below).
-- Measured directly: fillfactor=70 with no repack produced the SAME 0% HOT
-- ratio as fillfactor=100 until the table was rewritten.
ALTER TABLE workflow_instances SET (fillfactor = 70);

DROP INDEX IF EXISTS idx_instances_heartbeat;
CREATE INDEX IF NOT EXISTS idx_instances_heartbeat ON workflow_instances USING btree (assigned_to) WHERE (status = 'running'::text);

DROP INDEX IF EXISTS idx_instances_stale;
CREATE INDEX IF NOT EXISTS idx_instances_stale ON workflow_instances USING btree (status) WHERE (status = 'running'::text);
