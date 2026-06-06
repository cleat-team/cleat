-- Discover and recreate all FKs referencing workflow_instances with ON DELETE CASCADE.
-- Uses dynamic SQL because Postgres auto-names inline REFERENCES constraints.
-- Each FK is dropped and re-added within a single transaction.
DO $$
DECLARE
    r RECORD;
BEGIN
    SET LOCAL lock_timeout = '30s';
    FOR r IN
        SELECT conname, conrelid::regclass::text AS tbl
        FROM pg_constraint
        WHERE confrelid = 'workflow_instances'::regclass
          AND contype = 'f'
          AND conrelid::regclass::text IN (
              'event_history', 'workflow_signals', 'workflow_promises',
              'concurrency_keys', 'workflow_update_requests'
          )
        ORDER BY tbl
    LOOP
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I', r.tbl, r.conname);
        EXECUTE format(
            'ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id) ON DELETE CASCADE',
            r.tbl, r.conname
        );
    END LOOP;
END $$;
