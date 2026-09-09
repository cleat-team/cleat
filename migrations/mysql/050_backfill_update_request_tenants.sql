-- cleat#1037: MySQL rows written before #1034 carry the default tenant, and
-- the predicate #1034 added strands them.
--
-- workflow_update_requests.tenant_id is declared
--
--     tenant_id CHAR(36) NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'
--
-- and MySQL's CreateUpdateRequest did not list the column, so every row written
-- before #1034 carries the default rather than the tenant that created it.
-- #1034 then added `AND tenant_id = ?` to that table's reads and writes. Either
-- fact alone is harmless; together they strand any update request in flight
-- across the upgrade:
--
--   * GetPendingUpdateRequests does not return it, so the update is never
--     delivered to the workflow awaiting it;
--   * CompleteUpdateRequest does not match it, so it cannot be completed
--     either, and stays 'pending' permanently.
--
-- Stranded rather than merely invisible, which is why a backfill is worth the
-- migration.
--
-- MYSQL ONLY, and the other two dialects need nothing: PostgreSQL's
-- store_promises.go and SQL Server's mssql_signals_promises.go both listed
-- tenant_id all along, so neither has rows carrying the default.
--
-- THE OWNING ROW IS THE AUTHORITY, which is what makes this backfillable at
-- all. An update request belongs to a workflow and a workflow knows its tenant,
-- so nothing has to be supplied or guessed. That is the same shape as #1019's
-- idempotency backfill, and it is NOT available for every table of this kind --
-- cleat#1038's workflow_defs rows have no owning row to derive from, which is
-- why that one ships a release note instead.
--
-- SCOPE, swept rather than assumed. #1034 changed six MySQL statements and
-- exactly one of them is an INSERT; the other five are WHERE clauses gaining a
-- predicate, which cannot leave a row carrying a wrong value. Sweeping the
-- pre-#1034 tree for INSERT-shaped statements into the ten tenant-scoped tables
-- that omit the column finds three: this one, and two in
-- engine/versioned_loader.go -- a type with no tenant field and no production
-- caller. So this table is the only live instance and this migration is the
-- whole of the remedy.
UPDATE workflow_update_requests r
JOIN workflow_instances w ON w.id = r.workflow_id
SET r.tenant_id = w.tenant_id
WHERE r.tenant_id = '00000000-0000-0000-0000-000000000000'
  AND w.tenant_id <> '00000000-0000-0000-0000-000000000000';

-- The second predicate is what keeps this a no-op on single-tenant installs --
-- which is most of them, since the column default IS engine.DefaultTenantUUID.
-- Without it the statement would rewrite every row to the value it already
-- holds, turning a targeted repair into a full-table update on every upgrade.
