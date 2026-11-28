-- cleat#3552 (B3b-2): dropping a tenant over the admin API leaves a TOMBSTONE, and the
-- tombstone is what stops the id being reused by accident.
--
-- A drop removes every row the tenant owned, its keys, its role and its admin.tenants row. What
-- survives outside the database is keyed by the tenant id: worker-side store and pool caches, the
-- module byte cache, logs and metrics. A tenant created again under the same id would inherit
-- whatever is still keyed by it, so reuse of a dropped id is refused (409 tenant_id_dropped) unless
-- an operator deletes the tombstone first, which is a deliberate and audited act.
--
-- The tombstone is written in the SAME TRANSACTION as the drop. It is the one record of the drop that
-- is atomic with it: the audit log's row for the request is enqueued after the handler returns, and a
-- crash between the commit and that enqueue loses it. So the tombstone carries what an investigator
-- needs without the audit row -- who dropped, when, which tenant, and where the dropped tenant's own
-- audit chain ended, because that chain is deleted with the tenant (audit_events and
-- audit_chain_heads are registered tenant-scoped plugin tables, which admin.drop_tenant deletes).
--
-- THE KEY COLUMN IS NOT NAMED tenant_id, on purpose, and the reason is on SQL Server: its
-- admin.drop_tenant deletes from EVERY table that has a tenant_id column. A tombstone keyed by
-- tenant_id would be deleted by the drop that wrote it. PostgreSQL's procedure lists its tables, so it
-- would be safe there; the name is the same on both dialects so the two do not diverge by accident.
--
-- audit_chain_seq and audit_chain_hash are NULL when the tenant had no chain (the audit plugin was
-- not installed, or it never recorded a row for this tenant). NULL means "no chain existed", not
-- "unknown".
--
-- No UPDATE is granted: a tombstone is written once and deleted by an operator or not at all.
-- admin.drop_tenant is NOT redefined here.
CREATE TABLE IF NOT EXISTS admin.tenant_tombstones (
    dropped_tenant_id uuid        NOT NULL,
    name              text        NOT NULL,
    org_id            uuid        NOT NULL,
    created_by        text,
    dropped_by        text        NOT NULL,
    dropped_at        timestamptz NOT NULL DEFAULT now(),
    audit_chain_seq   bigint,
    audit_chain_hash  char(64),
    CONSTRAINT tenant_tombstones_pkey PRIMARY KEY (dropped_tenant_id)
);

REVOKE ALL ON TABLE admin.tenant_tombstones FROM PUBLIC;
GRANT SELECT, INSERT, DELETE ON TABLE admin.tenant_tombstones TO cleat_app;
