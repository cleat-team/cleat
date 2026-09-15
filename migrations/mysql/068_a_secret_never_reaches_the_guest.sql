-- cleat migration 068 (mysql): a secret never reaches the guest
--
-- cleat#1570. The PostgreSQL counterpart (080) carries the full reasoning for
-- why a reference is substituted at the host boundary rather than a value being
-- handed to the guest; this header records only what differs on MySQL.
--
-- NUMBER TAKEN BY HAND: 067 is claimed by an open pull request (cleat#1568) and
-- is not visible on develop. See 080's header.
--
-- NO ROW-LEVEL SECURITY, because MySQL has none. Tenant isolation on this
-- dialect is a database per tenant, so another tenant's secrets are not in this
-- database at all -- the separation sits above the table rather than inside it.
--
-- VARCHAR(128) for name rather than TEXT, because it is half of the primary
-- key and MySQL cannot index a TEXT column without a prefix length. 128 matches
-- engine.validSecretName's limit exactly, so a name the resolver would accept
-- always fits and one it would refuse cannot be stored.

CREATE TABLE IF NOT EXISTS tenant_secrets (
    tenant_id   CHAR(36)     NOT NULL,
    name        VARCHAR(128) NOT NULL,
    ciphertext  TEXT         NOT NULL,
    key_version INT          NOT NULL DEFAULT 1,
    updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),

    PRIMARY KEY (tenant_id, name),

    CONSTRAINT fk_tenant_secrets_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id) ON DELETE CASCADE,

    CONSTRAINT ck_tenant_secrets_name_charset
        CHECK (name REGEXP '^[A-Za-z0-9_.-]{1,128}$'),
    CONSTRAINT ck_tenant_secrets_ciphertext_nonempty
        CHECK (CHAR_LENGTH(ciphertext) > 0)
);
