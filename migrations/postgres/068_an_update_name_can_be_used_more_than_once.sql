-- cleat migration 068 (postgres): an update name is reusable, so the row
-- needs an identity that is not its name.
--
-- An update name is reusable. cleat#1416.
--
-- workflow_update_requests was PRIMARY KEY (workflow_id, update_name), and
-- completion is an UPDATE ... SET status = 'completed' rather than a delete, so
-- the name was consumed for the LIFE of the workflow. A workflow could accept
-- each update name exactly once. cleat#1330 measured the three dialects
-- answering the second request three different ways; cleat#1392 made them agree
-- on a 409. This makes the second request legal, which is what the repo owner
-- decided: an update is a request, and a request can be made twice.
--
-- The row needs an identity that survives that, and it cannot be promise_id.
-- That column is nullable and means "someone is waiting" -- engine/updater.go
-- has an explicit branch for a request with no promise, and three tests create
-- one. Conflating "who to answer" with "which row" would make a request without
-- a caller unrepresentable.
--
-- WHY THE BACKFILL IS update_name AND NOT A GENERATED VALUE. Under the old
-- primary key, (workflow_id, update_name) is unique BY CONSTRUCTION, so copying
-- the name satisfies the new key without inventing anything. It also buys the
-- Go side its compatibility path for free: an event history written before this
-- migration carries a request key with no request_id, and the name it does
-- carry is exactly what that row's request_id now holds. So a workflow
-- suspended mid-update across the upgrade completes against the right row with
-- no special case. See splitUpdateRequestKey.
--
-- Idempotent because SetupFullSchema re-applies the whole set in tests.

-- NO DEFAULT, and that was a decision rather than an omission. Six test
-- fixtures INSERT into this table directly, naming their columns, so a
-- self-populating identity would have saved changing all six -- and PostgreSQL
-- (gen_random_uuid()) and SQL Server (NEWID()) both express it happily.
--
-- MySQL does not. `ALTER TABLE ... ADD COLUMN request_id VARCHAR(255)
-- DEFAULT (UUID())` is refused outright under statement-based binlogging:
--
--	ERROR 1674 (HY000): Statement is unsafe because it uses a system function
--	                    that may return a different value on the replica.
--
-- Measured 2026-09-13 on MySQL 8. So the default would exist on two dialects
-- and not the third, and the writers it protects would keep working on
-- PostgreSQL and SQL Server while failing on MySQL -- a two-dialect guarantee
-- that reads like a three-dialect one. The six fixtures were changed instead.
ALTER TABLE workflow_update_requests ADD COLUMN IF NOT EXISTS request_id TEXT;

UPDATE workflow_update_requests SET request_id = update_name WHERE request_id IS NULL;

ALTER TABLE workflow_update_requests ALTER COLUMN request_id SET NOT NULL;

ALTER TABLE workflow_update_requests DROP CONSTRAINT IF EXISTS workflow_update_requests_pkey;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'workflow_update_requests_pkey'
          AND conrelid = to_regclass('workflow_update_requests')
    ) THEN
        ALTER TABLE workflow_update_requests
            ADD CONSTRAINT workflow_update_requests_pkey PRIMARY KEY (workflow_id, request_id);
    END IF;
END $$;

-- Dispatch reads pending requests by workflow, oldest first, and now has to
-- distinguish several rows that share a name.
CREATE INDEX IF NOT EXISTS idx_update_requests_pending_name
    ON workflow_update_requests (workflow_id, update_name, status);
