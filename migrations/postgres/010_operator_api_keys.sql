-- cleat#2169: an operator credential for admin HTTP routes.
--
-- cleat's HTTP auth has no operator identity. auth/ has no role or scope
-- concept, --enable-admin-api is deployment-wide, and callerOwnsTarget
-- (cmd/cleat-worker/api_admin.go) limits every /api/admin/* route to the
-- caller's OWN tenant -- so no HTTP caller can act on another tenant and the
-- operator surfaces are cleatctl-only. This table is the credential that
-- changes that.
--
-- THE SAME SHAPE AS admin.tenant_api_keys, MINUS tenant_id, deliberately: the
-- two questions it answers are the same ones -- which hash is live, and when
-- did it stop being live -- so rotation and expiry are existing machinery
-- (auth.TenantStore enforces expiry since cleat#2352) rather than something
-- invented here.
--
-- OUTSIDE THE TENANT-SCOPED SURFACE, BY SHAPE. Like admin.tenant_api_keys and
-- unlike every tenant-owned table, this one carries no tenant_id and has NO
-- row-level security: nothing ENABLES it, which is the stronger statement than
-- "no policy names it" -- a policy alone would do nothing without the enable.
-- An operator request therefore never reaches a tenant-scoped data path AS a
-- tenant; it has no tenant to be scoped to. The MySQL and MSSQL migrations
-- reach the same property by different routes, because neither has this
-- schema's RLS posture to inherit -- see each file's own comment.
CREATE TABLE IF NOT EXISTS admin.operator_api_keys (
    key_id uuid DEFAULT gen_random_uuid() NOT NULL,
    key_hash bytea NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    disabled_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ DEFAULT now() NOT NULL,
    PRIMARY KEY (key_id)
);

-- Partial on disabled_at IS NULL, mirroring idx_api_keys_hash: every lookup is
-- "is this presented hash a live key", and a disabled row is never a candidate.
CREATE INDEX IF NOT EXISTS idx_operator_api_keys_hash
    ON admin.operator_api_keys USING btree (key_hash) WHERE (disabled_at IS NULL);

-- EXPLICIT, for the reason 008_payload_encryption_ever_enabled.sql gives for
-- its own single grant: this creates a whole new table, and there is no
-- precedent in this tree for trusting the default-privilege path for a
-- credential. The privilege list mirrors admin.tenant_api_keys' grant in
-- 001_schema.sql rather than being narrowed to SELECT, because key creation and
-- rotation may come to run through the worker's own admin routes; a grant too
-- narrow fails at runtime in a path no test covers yet. Narrow it to SELECT
-- once key management is settled as an operator action outside the worker.
GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE admin.operator_api_keys TO cleat_app;
