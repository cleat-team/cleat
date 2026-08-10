-- cleat migration 010 (postgres): scope idempotency keys to a tenant
--
-- Bug: idempotency_keys is keyed by key_hash alone and has no tenant_id
-- column, and the hash is sha256(idempotencyKey) with the tenant nowhere in
-- it. An Idempotency-Key is therefore global across every tenant in the
-- deployment. Two tenants that pick the same string -- "order-123",
-- "daily-report", "1" -- collide: the second is handed the *first* tenant's
-- workflow ID with already_started = true, and its own workflow is never
-- started. Idempotency-Key is a client-supplied request header, so this is
-- the expected outcome of two customers naming things the way people name
-- things, not an attack. IMPROVEMENT-PLAN 3.10.
--
-- PostgreSQL's row-level security cannot help here: RLS filters on a tenant
-- column, and there is no tenant column. This is the one tenancy defect in
-- the set that all three dialects share equally.
--
-- Fix: add tenant_id and make the primary key (key_hash, tenant_id), so a
-- key is unique *within* a tenant and two tenants can hold the same key.
--
-- Upgrade behaviour, which is the whole reason this is a migration rather
-- than a one-line change to the hash. Folding the tenant into the hash needs
-- no schema change, but it changes every hash: after the upgrade no existing
-- key matches, so a retried request starts a *second* workflow -- precisely
-- what idempotency exists to prevent. Adding the column instead lets existing
-- rows take the default tenant, which is the tenant a single-tenant
-- deployment already writes under, so deduplication survives the upgrade.
--
-- Note on the version number: this file is 010 because IMPROVEMENT-PLAN /
-- PARALLEL-WORKSTREAMS reserve 010-019 for tenancy work. Migration versions
-- 006-015 were used by the pre-consolidation numbering that eb6b082 folded
-- into 001, so a develop-tracking database migrated before that commit may
-- already have version 10 recorded and would skip this file. That failure is
-- loud rather than silent -- StartNewRun's SELECT and INSERT both name
-- tenant_id, so every idempotent start errors with "column tenant_id does
-- not exist" instead of quietly mis-scoping.

-- Pin the creation target; see the note in 001_schema.sql. The default
-- search_path is "$user", public, and 001 creates a schema called "cleat"
-- while the shipped compose connects as POSTGRES_USER=cleat, so unqualified
-- names below would resolve against the wrong schema.
SET search_path = public;

ALTER TABLE idempotency_keys
    ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL
    DEFAULT '00000000-0000-0000-0000-000000000000';

-- Swap the single-column primary key for the composite one. Guarded on the
-- current shape rather than run unconditionally: the file is also applied by
-- deploy/postgres/100-apply-migrations.sh and by the test-schema helper in
-- engine/testutil, both of which can run it against a database that already
-- has it.
DO $$
DECLARE
    pk_columns integer;
BEGIN
    SELECT cardinality(c.conkey) INTO pk_columns
    FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    JOIN pg_namespace n ON n.oid = t.relnamespace
    WHERE n.nspname = 'public'
      AND t.relname = 'idempotency_keys'
      AND c.contype = 'p';

    IF pk_columns IS NULL THEN
        ALTER TABLE idempotency_keys
            ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (key_hash, tenant_id);
    ELSIF pk_columns = 1 THEN
        ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
        ALTER TABLE idempotency_keys
            ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (key_hash, tenant_id);
    END IF;
END $$;
