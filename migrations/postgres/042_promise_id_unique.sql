-- cleat migration 042 (postgres): a promise is addressed by its own id
--
-- IMPROVEMENT-PLAN 3.235, cleat#813. workflow_promises is keyed
--
--     PRIMARY KEY (workflow_id, promise_id)
--
-- and every settle path carried both columns in its WHERE clause. The
-- workflow_id it carried was the CALLER's, because a settler holds nothing
-- else -- the whole purpose of a promise is that something other than the
-- waiter completes it, and that something is handed an opaque id and nothing
-- more. So a child settling its parent's promise updated a row that did not
-- exist, and then woke itself. The parent waited until its timeout.
--
-- Settling is now keyed on promise_id alone, which is only sound if the id
-- identifies at most one row per tenant. It is a UUID, so this index does not
-- change which rows exist; it makes the assumption the lookup now depends on
-- explicit and enforced, rather than true by convention and silently violable
-- by anything that ever generates an id another way.
--
-- The primary key is deliberately left alone. It carries ownership, and the
-- ON DELETE CASCADE from workflow_instances that goes with it, which is still
-- what should happen when the creating workflow is deleted.

CREATE UNIQUE INDEX IF NOT EXISTS idx_promises_id_unique
    ON workflow_promises(tenant_id, promise_id);
