-- cleat migration 057 (postgres): scope concurrency keys to a tenant
--
-- Bug: concurrency_keys is keyed `PRIMARY KEY (key_hash)` and the hash is
-- digest(<key text>, 'sha256') with the tenant nowhere in it. The key
-- namespace is therefore global while every operation on the table is
-- tenant-scoped and the table is under RLS, so the uniqueness dimension and
-- the access dimension disagree. Tenant 2 acquiring a key tenant 1 already
-- holds is refused; its release carries `AND tenant_id = <its own>` and
-- matches nothing; and RLS correctly hides the blocking row, so it cannot
-- even see what stopped it. Blocked, unclearable, invisible until the TTL
-- expires. The names that collide are the ones everyone picks -- "nightly",
-- "sync", "cleanup". cleat#1189.
--
-- THIS IS THE SECOND INSTANCE OF A CLASS THAT ALREADY HAS A WRITTEN FIX.
-- idempotency_keys had exactly this shape and was repaired by migration 010:
-- same client-supplied string, same global namespace, same consequence. That
-- file's post-mortem is the one to read; this is the sibling table it left
-- behind.
--
-- Fix: make the primary key (key_hash, tenant_id), so a key is unique WITHIN
-- a tenant and two tenants can hold the same key string at once.
--
-- WHY NOT FOLD THE TENANT INTO THE HASH, which needs no migration at all.
-- Migration 010 rejected that for idempotency_keys because it changes every
-- hash, so no existing key matches after the upgrade and a retried request
-- starts a second workflow. The reasoning transfers with the consequence
-- changed: here, existing locks would become invisible to their holders, and
-- a second workflow could acquire a key someone is still holding -- a
-- double-acquire window for the length of the TTL, in a table whose entire
-- purpose is mutual exclusion. Changing the key preserves every row.
--
-- Upgrade is safe without a data step: key_hash was globally unique, so
-- (key_hash, tenant_id) is strictly weaker and no existing row can violate
-- it. concurrency_keys already carries tenant_id NOT NULL DEFAULT the
-- all-zero tenant, which is what a single-tenant deployment already writes
-- under, so existing locks keep working across the upgrade.

-- Pin the creation target; see the note in 001_schema.sql. The default
-- search_path is "$user", public, and 001 creates a schema called "cleat"
-- while the shipped compose connects as POSTGRES_USER=cleat, so unqualified
-- names below would resolve against the wrong schema.
SET search_path = public;

DO $$
DECLARE
    pk_columns integer;
BEGIN
    SELECT cardinality(c.conkey) INTO pk_columns
    FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    JOIN pg_namespace n ON n.oid = t.relnamespace
    WHERE n.nspname = 'public'
      AND t.relname = 'concurrency_keys'
      AND c.contype = 'p';

    IF pk_columns IS NULL THEN
        ALTER TABLE concurrency_keys
            ADD CONSTRAINT concurrency_keys_pkey PRIMARY KEY (key_hash, tenant_id);
    ELSIF pk_columns = 1 THEN
        ALTER TABLE concurrency_keys DROP CONSTRAINT concurrency_keys_pkey;
        ALTER TABLE concurrency_keys
            ADD CONSTRAINT concurrency_keys_pkey PRIMARY KEY (key_hash, tenant_id);
    END IF;
END $$;
