-- cleat migration 103 (postgres): deployment-wide credentials live in their own table
--
-- cleat#1992 part (1). Five plugin credentials (blobstore's S3 key pair,
-- email's SendGrid key, one key per configured llm provider, slacknotify's
-- request-signing secret, scheduledbackup's backup-target DSN) are read once
-- from --plugin-config at worker boot today, so rotating any of them needs
-- every worker restarted. This table and engine/deployment_secrets.go move
-- them to a store a plugin reads per use, the same property tenant_secrets
-- already has for per-tenant credentials.
--
-- NO tenant_id. These credentials are not scoped to any tenant -- there is
-- nothing for a row-level policy to key on -- so this is deliberately not
-- tenant_secrets with an optional tenant column (owner decision,
-- issuecomment-5805744413, "option B", relayed by the coordinator).
--
-- PROTECTION IS BY ENCRYPTION, NOT A DEDICATED READER ROLE (same decision).
-- There is no cleat_deployment_secrets_reader login, no new DSN, no
-- launch-site change anywhere a worker is deployed. The worker reads this
-- table over its ordinary connection, as cleat_app, exactly as it reads
-- every other table. What protects a secret is that cleat_app only ever
-- sees CIPHERTEXT and the master key that opens it never touches the
-- database (engine/deployment_secrets.go never hands a plugin the key or
-- this store's *sql.DB). The REVOKE below removes cleat_app's ability to
-- WRITE this table -- the ALTER DEFAULT PRIVILEGES in 005_app_role.sql
-- would otherwise hand it INSERT/UPDATE/DELETE the same as every other
-- table -- so a worker process (or anything reaching SQL through it, which
-- is the actual threat this addresses: tenant-reachable SQL running as
-- cleat_app, not a plugin's own first-party code -- see readonlydb.go's
-- doc comment for the same distinction stated about a different table) can
-- read a deployment secret but cannot rotate or plant one. Writes stay on
-- cleatctl's elevated connection, the same asymmetry tenant_secrets already
-- has.
--
-- DOMAIN-SEPARATED ENCRYPTION (same decision). engine/deployment_secrets.go
-- derives its key with an HKDF info string distinct from tenant_secrets',
-- and additionally authenticates the ciphertext against the secret's own
-- NAME rather than a tenant id. So a row copied to a different name fails
-- to open, and a deployment-secret ciphertext placed in tenant_secrets (or
-- the reverse) fails to open too -- even for an operator who has configured
-- the SAME master key for both, which this deployment does: see
-- "SAME KEY RING" below.
--
-- SAME KEY RING AS tenant_secrets, not a second one an operator must
-- generate and hold (same decision). CLEAT_SECRET_MASTER_KEY and its
-- _PREVIOUS pair, already documented in docs/how-to/use-secrets.md, are
-- what this table is sealed under too. Domain separation above is what
-- keeps that safe rather than merely convenient.
--
-- key_version, disabled_at, updated_at, created_at: the same four columns
-- tenant_secrets carries, in its FINAL shape after cleat#1702's entity
-- contract (081_a_secret_never_reaches_the_guest.sql,
-- 102_a_worker_publishes_the_secret_keys_it_can_open.sql,
-- 086_two_entities_record_when_they_were_created.sql) -- rotation is not
-- precluded, a secret can be retired without losing its ciphertext, and
-- both "last changed" and "when created" are tracked. name is the primary
-- key because there is no tenant dimension to pair it with. Unlike
-- tenant_secrets, created_at needs no separate backfill migration: this
-- table has no rows yet, so it is simply part of the CREATE TABLE below --
-- scripts/entity-contract.tsv classifies this table as a member, and the
-- grandfather list a new member could lean on instead is capped at empty
-- (scripts/check-entity-contract.py), so a brand-new member has to conform
-- on arrival.

CREATE TABLE IF NOT EXISTS deployment_secrets (
    -- Matches engine.validSecretName, the same charset tenant_secrets uses,
    -- and the fixed, documented names this feature reads by (e.g.
    -- "blobstore.access_key_id", "llm.providers.openai.api_key").
    name        TEXT PRIMARY KEY,

    -- Base64 of nonce || AES-256-GCM ciphertext. Never logged, never
    -- returned to a plugin except via DeploymentSecrets.Get's own return
    -- value, and never placed in event history (nothing here is reachable
    -- from a workflow at all -- these are plugin Init/dial-out credentials,
    -- not something a workflow author references).
    ciphertext  TEXT NOT NULL,

    key_version INTEGER NOT NULL DEFAULT 1,
    disabled_at TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_deployment_secrets_name_charset
        CHECK (name ~ '^[A-Za-z0-9_.-]{1,128}$'),
    CONSTRAINT ck_deployment_secrets_ciphertext_nonempty
        CHECK (length(ciphertext) > 0)
);

REVOKE ALL ON deployment_secrets FROM PUBLIC;

-- cleat_app keeps SELECT (granted by 005_app_role.sql's default privileges,
-- and needed for the worker's own per-use lookups); INSERT/UPDATE/DELETE are
-- removed so a worker process reading this table cannot also write it.
REVOKE INSERT, UPDATE, DELETE ON deployment_secrets FROM cleat_app;
