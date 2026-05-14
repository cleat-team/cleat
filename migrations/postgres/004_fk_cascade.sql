-- cleat postgres migration 004: Add ON DELETE CASCADE to all foreign keys
-- referencing workflow_instances(id).
--
-- When a workflow instance is deleted, all related rows in child tables
-- (events, signals, promises, concurrency keys, update requests) must be
-- automatically removed. Without CASCADE these orphaned rows accumulate
-- and cause FK violation errors on delete.
--
-- Reference: F04 from cleat improvement plan.
--
-- Each FK is dropped by looking up the actual constraint name from
-- pg_constraint (rather than hardcoding it) and then re-created with a
-- deterministic name and ON DELETE CASCADE.  This means the migration
-- works even if a previous manual migration renamed the constraint, and
-- it remains idempotent because the constraint is always dropped before
-- being re-added.

-- 1. event_history.workflow_id -> workflow_instances(id)
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT con.conname INTO constraint_name
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    WHERE rel.relname = 'event_history'
      AND con.contype = 'f'
      AND con.confrelid = 'workflow_instances'::regclass;

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE event_history DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

ALTER TABLE event_history
    ADD CONSTRAINT event_history_workflow_id_fkey
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
    ON DELETE CASCADE;

-- 2. workflow_signals.workflow_id -> workflow_instances(id)
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT con.conname INTO constraint_name
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    WHERE rel.relname = 'workflow_signals'
      AND con.contype = 'f'
      AND con.confrelid = 'workflow_instances'::regclass;

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE workflow_signals DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

ALTER TABLE workflow_signals
    ADD CONSTRAINT workflow_signals_workflow_id_fkey
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
    ON DELETE CASCADE;

-- 3. workflow_promises.workflow_id -> workflow_instances(id)
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT con.conname INTO constraint_name
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    WHERE rel.relname = 'workflow_promises'
      AND con.contype = 'f'
      AND con.confrelid = 'workflow_instances'::regclass;

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE workflow_promises DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

ALTER TABLE workflow_promises
    ADD CONSTRAINT workflow_promises_workflow_id_fkey
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
    ON DELETE CASCADE;

-- 4. concurrency_keys.workflow_id -> workflow_instances(id)
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT con.conname INTO constraint_name
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    WHERE rel.relname = 'concurrency_keys'
      AND con.contype = 'f'
      AND con.confrelid = 'workflow_instances'::regclass;

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE concurrency_keys DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

ALTER TABLE concurrency_keys
    ADD CONSTRAINT concurrency_keys_workflow_id_fkey
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
    ON DELETE CASCADE;

-- 5. workflow_update_requests.workflow_id -> workflow_instances(id)
DO $$
DECLARE
    constraint_name text;
BEGIN
    SELECT con.conname INTO constraint_name
    FROM pg_constraint con
    JOIN pg_class rel ON rel.oid = con.conrelid
    WHERE rel.relname = 'workflow_update_requests'
      AND con.contype = 'f'
      AND con.confrelid = 'workflow_instances'::regclass;

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE workflow_update_requests DROP CONSTRAINT %I', constraint_name);
    END IF;
END $$;

ALTER TABLE workflow_update_requests
    ADD CONSTRAINT workflow_update_requests_workflow_id_fkey
    FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
    ON DELETE CASCADE;
