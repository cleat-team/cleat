-- cleat#3552 (B2b-2): a tenant created through the provisioning API records WHO created
-- it, and a key minted in that handshake is marked provisional until the creator confirms
-- it was stored.
--
-- created_by is the creating principal's NAME ("clew-service" for a provision key, or
-- "operator:<KeyID>" for an admin key). It is what limits replaying a create, and
-- confirming it, to the creator, and what a later drop uses to let a provisioner drop only
-- what it created. NULL for every tenant that predates it and for one made offline with
-- --create-tenant: no principal made those through the API, so none may replay them.
--
-- provisional marks a key that exists but whose delivery has not been confirmed. It is live
-- (it authenticates) and short-lived (expires_at), and a confirm clears both. DEFAULT false
-- is what keeps every existing key exactly what it was.
--
-- Two ALTER TABLEs, each IF NOT EXISTS, so a re-run after a failure partway is harmless.
-- No table, policy or routine is created or redefined.
ALTER TABLE admin.tenants
    ADD COLUMN IF NOT EXISTS created_by text;

ALTER TABLE admin.tenant_api_keys
    ADD COLUMN IF NOT EXISTS provisional boolean NOT NULL DEFAULT false;
