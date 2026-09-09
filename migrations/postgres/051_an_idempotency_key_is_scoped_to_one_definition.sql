-- cleat#1047: an idempotency key was not scoped to a workflow definition.
--
-- idempotency_keys is keyed (key_hash, tenant_id) and carries no definition, so
-- a second request naming a DIFFERENT workflow matched the first row and was
-- handed that workflow's id with alreadyExisted = true, while its own workflow
-- was never started. A caller awaiting that id reads the other workflow's
-- result as its own.
--
-- def_name is stored rather than folded into key_hash because THE RAW KEY IS
-- NOT STORED -- the table holds only its sha256 -- so a hash covering more
-- cannot be recomputed for existing rows. Changing the scheme would silently
-- invalidate every key in flight at upgrade, and a caller retrying would start
-- a SECOND workflow: a duplicate execution, which is the harm idempotency
-- exists to prevent, introduced by the fix for a rarer one.
--
-- NULLABLE, deliberately. The backfill below derives def_name from the owning
-- workflow -- the same authority argument as #1037 -- but a row whose workflow
-- has already been purged has nothing to derive from. Those keep NULL, and the
-- Go side reads NULL as "unknown, allow", which degrades to today's behaviour
-- rather than to a constraint violation. That is the property a NOT NULL column
-- in the primary key could not have offered.
ALTER TABLE idempotency_keys ADD COLUMN IF NOT EXISTS def_name TEXT;

UPDATE idempotency_keys k
SET def_name = w.def_name
FROM workflow_instances w
WHERE w.id = k.workflow_id
  AND k.def_name IS NULL;
