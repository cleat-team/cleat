-- cleat migration 098 (postgres): a queue declares its own worker concurrency
--
-- cleat#1917. Owner-approved design recorded on the issue, 2026-09-22: DBOS
-- parity for a queue's PER-WORKER cap -- how many of a queue's holders one
-- worker process may own at once, independent of `concurrency_limit` (093),
-- which bounds the queue's total across every worker. A worker serving two
-- tenants, each capped at 1 on a `gpu` queue, may run one of each at the same
-- time; that is intended, not a bug -- see the issue's "Purpose" section.
--
-- ONE NULLABLE COLUMN ON queues, NOT A NEW TABLE, for the same reason
-- rate_limit/rate_period_seconds (096) are columns rather than a table: this
-- is a property of the queue itself, one value per (tenant_id, name), no
-- history, no independent lifecycle.
--
-- NULL, MATCHING concurrency_limit's OWN CONVENTION ONE LEVEL UP. NULL means
-- no per-worker cap -- every existing row gets NULL by ADD COLUMN's own
-- default, so every queue created before this migration keeps behaving
-- exactly as it does today.
--
-- THE PAIRED CHECK REFERENCES concurrency_limit DIRECTLY. The design's
-- decision 5 is "1 <= worker_concurrency <= concurrency_limit": a worker cap
-- above the global cap can never bind, so it is refused rather than silently
-- accepted as a no-op. engine.QueueStore enforces the same rule before any
-- statement runs (see queue_store.go), for the reason CreateQueue's own
-- comment gives for rate_limit: a constraint violation does not tell an
-- operator which flag they got wrong. This CHECK is the backstop, not the
-- primary defense.
--
-- IF NOT EXISTS on both the column and the constraint, following 096/097's
-- own reasoning: cmd/cleat-worker runs several instances against one
-- database at startup, each applying pending migrations, and a plain ADD
-- COLUMN / ADD CONSTRAINT is not idempotent under that race. Measured on
-- 096 in the Cluster Integration Tests job (cleat#1918 PR #1961): four
-- workers applying it concurrently, one won, the other three failed
-- permanently. 062, 091 and 096 all hit the same race; this follows their
-- pattern.
ALTER TABLE queues
    ADD COLUMN IF NOT EXISTS worker_concurrency INTEGER;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_queues_worker_concurrency_positive') THEN
        ALTER TABLE queues ADD CONSTRAINT ck_queues_worker_concurrency_positive
            CHECK (worker_concurrency IS NULL OR worker_concurrency >= 1);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_queues_worker_concurrency_le_concurrency') THEN
        ALTER TABLE queues ADD CONSTRAINT ck_queues_worker_concurrency_le_concurrency
            CHECK (worker_concurrency IS NULL OR worker_concurrency <= concurrency_limit);
    END IF;
END $$;
