-- cleat#2169: an operator credential for admin HTTP routes. See
-- migrations/postgres/010_operator_api_keys.sql for what the credential is and
-- why it is the shape of tenant_api_keys minus tenant_id; this file records
-- only what differs on MySQL, which is two things.
--
-- UNQUALIFIED, unlike the other two dialects. MySQL has no schema namespace
-- here -- migrations/mysql never creates an `admin` schema -- so the same table
-- is written bare, exactly as tenant_api_keys is (`CREATE TABLE IF NOT EXISTS
-- tenants` here against `admin.tenants` in the other two; cleat#1719 recorded
-- that cross-dialect membership must be compared on the BARE name for this
-- reason).
--
-- AND THE "OUTSIDE THE TENANT-SCOPED SURFACE" PROPERTY IS REACHED BY A
-- DIFFERENT ROUTE HERE, which is why it is worth stating rather than assuming:
-- MySQL has no row-level security at all -- zero CREATE POLICY or ROW LEVEL
-- SECURITY statements across migrations/mysql/*.sql -- so there is no RLS
-- posture to be outside of. Isolation on MySQL is structural (one database per
-- tenant, see MySQLStoreFactory), so a table in the base schema with no
-- tenant_id is not tenant-scoped BY CONSTRUCTION rather than by policy. The
-- conclusion is the same on all three dialects; the reason is not, and the
-- reason is what a future reader checks.
--
-- No GRANT: MySQL's application identity is granted per DATABASE by the
-- app-login path, not per table from a migration, so there is nothing to add
-- here.
CREATE TABLE IF NOT EXISTS operator_api_keys (
  key_id char(36) NOT NULL,
  key_hash varbinary(32) NOT NULL,
  description varchar(1024) NOT NULL DEFAULT '',
  created_at timestamp(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  disabled_at timestamp(6) NULL DEFAULT NULL,
  expires_at timestamp(6) NULL DEFAULT NULL,
  updated_at timestamp(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (key_id),
  KEY idx_operator_api_keys_hash (key_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
