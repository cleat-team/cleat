-- cleat migration 081 (postgres): a secret never reaches the guest
--
-- cleat#1570. Every playbook in docs/playbooks/ needs per-tenant third-party
-- credentials -- payment keys, CRM tokens, model API keys -- and kvstore, which
-- is where they would otherwise go, is a versioned JSONB store with no
-- encryption, no rotation and no access audit.
--
-- NUMBER TAKEN BY HAND. 080 is claimed by an open pull request (cleat#1568), which itself moved off 079 when #1590 merged and
-- is therefore not visible on develop, so the next free number read from the
-- merged tree is wrong. WORKSTREAM.md records the same hazard for
-- IMPROVEMENT-PLAN section numbers and the same remedy: take the next one
-- deliberately and say so. scripts/check-migration-versions.sh would have
-- caught a collision only once both branches were merged, and by then one of
-- the two files would never apply to any database that had already recorded
-- that version.
--
-- ---------------------------------------------------------------------------
-- WHAT MAKES THIS MORE THAN AN ENCRYPTED COLUMN.
--
-- Encryption at rest is the easy half. The half that matters is that the
-- plaintext must not appear in two places cleat writes constantly: guest memory
-- and event history.
--
-- A secrets host call returning the value to WASM fails both. The guest holds
-- the plaintext, and whatever the guest then passes to a recorded call is
-- persisted -- where engine.Redact is a FIELD-NAME HEURISTIC and would not
-- catch a credential under a field named `config`.
--
-- So a workflow passes a REFERENCE, ${secret:name}, and the host substitutes on
-- the way in to the plugin. The structural part, which is why this works rather
-- than merely usually working: PluginCall records `PluginInput: inputJSON` and
-- separately hands that same string to the plugin function. Substituting inside
-- a WRAPPED function cannot change what was recorded, because the recorder
-- reads the outer variable. The history keeps the reference by construction,
-- not by a redaction rule that has to keep up with field names.
--
-- ---------------------------------------------------------------------------
-- ciphertext IS OPAQUE TO THE DATABASE, and stored base64 rather than BYTEA.
--
-- The value is AES-256-GCM sealed under a key derived per tenant, with the
-- tenant id as additional authenticated data, so a row copied into another
-- tenant fails to open rather than decrypting to something. Base64 because the
-- three dialects disagree about binary types and the encoding cost is
-- irrelevant beside a network round trip -- the same trade migration 001 makes
-- elsewhere for portability.
--
-- key_version EXISTS SO ROTATION IS NOT PRECLUDED, and is not used yet.
-- Rotation with overlapping validity is explicitly out of scope for cleat#1570;
-- a column added later would need a backfill over ciphertext nobody can read
-- without the old key, which is the migration you least want to discover you
-- need.

CREATE TABLE IF NOT EXISTS tenant_secrets (
    tenant_id   UUID NOT NULL
        REFERENCES admin.tenants(tenant_id) ON DELETE CASCADE,

    -- Matches engine.validSecretName. Constrained here as well as in Go so a
    -- row written by any other means cannot carry a name the resolver's regexp
    -- would refuse to match -- a secret that exists and can never be referenced.
    name        TEXT NOT NULL,

    -- Base64 of nonce || AES-256-GCM ciphertext. Never logged, never returned
    -- to a guest, and never placed in event history.
    ciphertext  TEXT NOT NULL,

    key_version INTEGER NOT NULL DEFAULT 1,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, name),

    CONSTRAINT ck_tenant_secrets_name_charset
        CHECK (name ~ '^[A-Za-z0-9_.-]{1,128}$'),
    CONSTRAINT ck_tenant_secrets_ciphertext_nonempty
        CHECK (length(ciphertext) > 0)
);

-- Row-level security, in the shape 039 established and 079 follows. A secrets
-- table without it would be the worst possible placement of the gap.
--
-- FORCE is what makes it real for the table owner: without it RLS is silently
-- bypassed for the role that ran the migration, which normally owns what it
-- creates. See 001_schema.sql and IMPROVEMENT-PLAN 1.10.
ALTER TABLE tenant_secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_secrets FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_secrets ON tenant_secrets;
CREATE POLICY tenant_isolation_secrets ON tenant_secrets
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

-- Deletion is by foreign key, following 039 and 079: admin.drop_tenant
-- enumerates tables by hand and 039's header records tenant_settings being
-- absent from that list for twenty migrations. The name is still added to
-- cleatctl's dropTenantTables for the count it reports.
