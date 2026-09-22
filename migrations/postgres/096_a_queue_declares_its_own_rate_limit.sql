-- cleat migration 096 (postgres): a queue declares its own rate limit
--
-- cleat#1918. Owner decision recorded on the issue, 2026-09-21: DBOS parity
-- for `Queue(name, limiter={limit, period})` -- a rate limit on ADMISSION,
-- independent of `concurrency_limit` (093), which bounds how many run at
-- once. A run can be admission-rate-limited and still have a free
-- concurrency slot, and vice versa; #1918's design calls this out explicitly
-- as "composition: independent of concurrency_limit".
--
-- TWO NULLABLE COLUMNS ON queues, NOT A NEW TABLE FOR THE CONFIG ITSELF. The
-- rate limit is a property of the queue the same way concurrency_limit is --
-- one value per (tenant_id, name), no history, no independent lifecycle --
-- so it belongs beside it. `queue_rate_tokens` (097) is the counter this
-- config drives; the config and the counter are different lifetimes and
-- different tables for the same reason queues and queue_holders (094) are.
--
-- BOTH NULL, NOT ONE. NULL = unlimited, matching concurrency_limit's own
-- absence-means-unregistered convention one level up (no `queues` row at all
-- = today's bare-key mutex). A rate limit needs both a count and a window to
-- mean anything; ck_queues_rate_limit_paired below refuses a queue that sets
-- one without the other, which would otherwise silently mean "divide by an
-- undefined period" the first time the claim tried to read it.
--
-- NON-BREAKING. Every existing row gets NULL/NULL by ADD COLUMN's own
-- default, so every queue created before this migration keeps behaving
-- exactly as it does today -- unlimited rate, same as unlimited was the only
-- option before this existed.

ALTER TABLE queues
    ADD COLUMN rate_limit          INTEGER,
    ADD COLUMN rate_period_seconds INTEGER;

ALTER TABLE queues
    ADD CONSTRAINT ck_queues_rate_limit_paired
        CHECK ((rate_limit IS NULL) = (rate_period_seconds IS NULL)),
    ADD CONSTRAINT ck_queues_rate_limit_positive
        CHECK (rate_limit IS NULL OR rate_limit >= 1),
    ADD CONSTRAINT ck_queues_rate_period_positive
        CHECK (rate_period_seconds IS NULL OR rate_period_seconds >= 1);
