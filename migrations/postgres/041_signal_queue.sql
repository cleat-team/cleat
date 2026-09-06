-- cleat migration 041 (postgres): a signal is a delivery, not a row
--
-- IMPROVEMENT-PLAN 3.215. The table was keyed
--
--     PRIMARY KEY (workflow_id, signal_name)
--
-- so it could hold exactly one signal per name per workflow, and every dialect
-- resolved the resulting conflict by overwriting the payload. Sending
-- "approve" twice before the workflow consumed it discarded the first payload
-- with no error -- data loss on the ordinary path, not a race. Any workflow
-- that accumulates (a counter, one approval per reviewer, a batch of items)
-- silently received only the last.
--
-- The fix is to model a signal as what it is. A delivery is an event with an
-- identity; a name is not an identity. So the primary key becomes a surrogate
-- monotonic id and (workflow_id, signal_name) becomes an ordinary index, which
-- makes the table a FIFO queue per (workflow, name).
--
-- Why the id has to be monotonic rather than, say, a UUID: the await path
-- takes the OLDEST unconsumed delivery, and "oldest" has to be a total order
-- the database can index. delivered_at is not it -- two signals delivered in
-- the same microsecond tie, and the tie has to break the same way on every
-- read or a replay can see a different signal than the original run did.
--
-- Guarded on the column rather than on the constraint, because the constraint
-- name is what a fresh 001_schema.sql database has and an already-migrated one
-- does not. The column is the thing this migration is really asserting.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'workflow_signals' AND column_name = 'id'
    ) THEN
        ALTER TABLE workflow_signals DROP CONSTRAINT IF EXISTS workflow_signals_pkey;
        ALTER TABLE workflow_signals ADD COLUMN id BIGSERIAL PRIMARY KEY;
    END IF;
END $$;

-- The queue read: filter by (workflow_id, signal_name), take the lowest id.
-- Deliberately NOT (workflow_id, signal_name, id) -- a workflow has a handful
-- of unconsumed signals of one name at most, so the ordering is a sort over a
-- tiny set, and the two-column form is the one MySQL can also create before
-- the id column exists (see migrations/mysql/040_signal_queue.sql, where that
-- ordering constraint is forced rather than chosen).
CREATE INDEX IF NOT EXISTS idx_workflow_signals_queue
    ON workflow_signals (workflow_id, signal_name);
