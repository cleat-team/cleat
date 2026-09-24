-- cleat migration 102 (mysql): deployment-wide credentials live in their own table
--
-- cleat#1992 part (1). The PostgreSQL counterpart (103) carries the full
-- reasoning; this header records only what differs on MySQL.
--
-- NO REVOKE, because MySQL has no cleat_app counterpart at all
-- (`git grep -n cleat_app -- migrations/mysql/` is empty) -- there is one
-- database login and MySQL's own isolation story is a database per tenant,
-- not a role split within one, so there is nothing to remove write access
-- from without also removing the worker's own read path. Protection here is
-- the same as on every dialect: cleat_app-equivalent SQL only ever sees
-- ciphertext, and the master key never touches the database.
--
-- VARCHAR(128) for name, matching tenant_secrets' mysql migration (069):
-- it is the primary key and MySQL cannot index TEXT without a prefix
-- length. 128 matches engine.validSecretName's limit exactly.

CREATE TABLE IF NOT EXISTS deployment_secrets (
    name        VARCHAR(128) NOT NULL,
    ciphertext  TEXT         NOT NULL,
    key_version INT          NOT NULL DEFAULT 1,
    disabled_at TIMESTAMP(6) NULL,
    updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),

    PRIMARY KEY (name),

    CONSTRAINT ck_deployment_secrets_name_charset
        CHECK (name REGEXP '^[A-Za-z0-9_.-]{1,128}$'),
    CONSTRAINT ck_deployment_secrets_ciphertext_nonempty
        CHECK (CHAR_LENGTH(ciphertext) > 0)
);
