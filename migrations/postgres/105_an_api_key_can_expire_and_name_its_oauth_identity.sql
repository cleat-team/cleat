-- cleat migration 105 (postgres): an API key can expire and name its OAuth identity
--
-- cleat#2352, a separable piece of #2340 (OAuth login). Design v2 §(3)/(4)
-- (see the issue): a manually-provisioned service key stays permanent until
-- revoked (NULL = no expiry, unchanged), but an OAuth-minted key must track
-- the IdP session's own lifetime rather than becoming a forever credential --
-- so admin.tenant_api_keys gains an expires_at a caller can set. Both columns
-- are added on all three dialects even though OAuth login itself is
-- Postgres-only in 0.3.0 (owner decision on #2340): expiry is a general
-- capability of this table, enforced identically by
-- auth.TenantStore.ResolveTenantFromAPIKey on every dialect, and a
-- schema that only matched OAuth's own dialect scope would leave
-- MySQL/MSSQL unable to express an expiring key at all, for reasons that
-- have nothing to do with the column's own meaning. 001_schema.sql carries
-- both columns directly, as this table's other retirement-spelling columns
-- already do (see 087's header on why this file states the FINAL column
-- set rather than the incremental one).
--
-- oauth_identity is nullable and unindexed here on purpose: it is set only
-- by OAuth-minted keys (format "<provider>:<identity>", e.g.
-- "github:alice@example.com"), NULL for every key this migration runs
-- against today, and its lookup pattern (revoke every live key for one
-- identity, on removal from an allowlist -- design v2 §(4)) belongs to
-- oauthprovider's own migration once #2340 lands that table, not to this
-- one. Adding an index speculatively, before there is a query to serve,
-- is the kind of unwired mechanism CLAUDE.md's Finding-class-5 warns
-- against -- a column that LOOKS like infrastructure for a feature that
-- is not here yet.
--
-- NO CHANGE TO idx_api_keys_hash. It stays a plain lookup on key_hash
-- (WHERE disabled_at IS NULL); this migration's expiry check is applied at
-- read time in the query text (auth.TenantStore's resolveAPIKeyStmt), not
-- baked into an index predicate, so a key that has not yet expired keeps
-- using the same index it always did and one that has stops matching the
-- query's own WHERE clause -- no separate expiry index is needed for that.

ALTER TABLE admin.tenant_api_keys
    ADD COLUMN IF NOT EXISTS expires_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS oauth_identity TEXT;

COMMENT ON COLUMN admin.tenant_api_keys.expires_at IS
    'cleat#2352: when this key stops authenticating. NULL = no expiry (the default for every key created before this column existed, and for any manually-provisioned service key today). Enforced by auth.TenantStore.ResolveTenantFromAPIKey on every dialect.';

COMMENT ON COLUMN admin.tenant_api_keys.oauth_identity IS
    'cleat#2352: "<provider>:<identity>" for a key minted by OAuth login (e.g. "github:alice@example.com"); NULL for every other key. Set by oauthprovider (#2340), not by this migration.';
