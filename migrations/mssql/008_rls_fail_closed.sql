-- 008_rls_fail_closed.sql: MSSQL RLS is already silently fail-closed.
-- The inline TVF fn_tenant_filter cannot throw errors (security policy limitation).
-- Application-layer hardening in setSessionContext (mssql_store.go) provides
-- an explicit error when tenantID is empty before any query reaches the database.
-- This migration file exists to keep numbering consistent with Postgres 008.
SELECT 1; -- no-op placeholder
