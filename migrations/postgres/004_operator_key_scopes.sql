-- cleat#3552 (B2b-1): an operator key can be narrowed to a scope, and carries the
-- name of the principal it was issued to.
--
-- scopes is a space-separated list, the shape OAuth uses, so one text column
-- holds it identically on all three dialects. DEFAULT 'admin' is what keeps every
-- existing key exactly as powerful as it was: a key minted before this column
-- existed reads back as ["admin"], which the middleware admits to every
-- /api/admin/* route. A narrower scope is opt-in at mint time (cleatctl
-- operator-key create --scope ...); nothing here narrows an existing key.
--
-- principal names who the key was issued to ("clew-service"), and is what a
-- later change records as the creator of a tenant. It is a NAME the operator
-- chooses and not an authenticator, so rotating a key (a new key_id, the same
-- principal) does not orphan what the principal created. NULL for the keys that
-- predate it, and for any key whose scopes do not need one.
--
-- One ALTER TABLE, so it is all-or-nothing on every dialect, and IF NOT EXISTS
-- so a re-run after a failure elsewhere in the same migration run is harmless.
ALTER TABLE admin.operator_api_keys
    ADD COLUMN IF NOT EXISTS scopes text NOT NULL DEFAULT 'admin',
    ADD COLUMN IF NOT EXISTS principal text;
