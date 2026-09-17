-- cleat migration 079 (mssql): one reversible off-switch for API keys
--
-- cleat#1702, the SQL Server half of postgres/087. Migration 076 already added
-- `disabled_at` to every member, so this backfills and removes rather than
-- adding. See that file for why
-- `revoked_at` -> `disabled_at` is a rename with an EXACT backfill, and for why
-- `workflow_defs.deprecated` is not converted alongside it.
--
-- NO SECURITY POLICY TOGGLE, AND THIS IS THE LINE NOT TO COPY FROM 078. That
-- migration disables dbo.TenantFilter_Settings and dbo.TenantFilter_Secrets
-- around its backfill, because SQL Server filter predicates exempt nobody --
-- not even sysadmin -- so a filtered UPDATE matches zero rows and reports
-- success. That hazard is real and it does not reach this table:
-- admin.tenant_api_keys carries no security policy on any engine. Checked
-- against every ALTER SECURITY POLICY in migrations/mssql rather than inferred
-- from the neighbouring file.
--
-- The reason it has none is structural, from postgres/061: the authenticator
-- reads this table BEFORE a tenant is known, and a predicate keyed on the
-- current tenant cannot be evaluated by the query that discovers it. So this
-- is a property of the table's role rather than a gap someone will close
-- later, and a future policy on it would be the thing to question.
--
-- THE FILTERED INDEX MUST GO BEFORE THE COLUMN. `idx_api_keys_hash` is
-- `WHERE revoked_at IS NULL` (001_schema.sql:511), so SQL Server refuses to
-- drop the column while the index depends on it -- unlike Postgres, which
-- would drop the index silently along with it. Rebuilt on `disabled_at IS NULL`
-- so the authentication lookup stays covered.
--
-- THE BACKFILL IS INSIDE EXEC(), WHICH IS NOT DECORATION. SQL Server's deferred
-- name resolution covers missing TABLES, not missing COLUMNS: a batch naming
-- `revoked_at` fails to compile once the column is gone, even under an IF whose
-- branch is not taken. EXEC on a string defers the compile to execution, which
-- never happens on a second run. The same reasoning is why the MySQL half uses
-- PREPARE.

-- Exact, not a bound: revoked_at already holds the instant.
IF COL_LENGTH(N'admin.tenant_api_keys', N'revoked_at') IS NOT NULL
    EXEC(N'UPDATE admin.tenant_api_keys
              SET disabled_at = revoked_at
            WHERE revoked_at IS NOT NULL
              AND disabled_at IS NULL');
GO

IF COL_LENGTH(N'admin.tenant_api_keys', N'revoked_at') IS NOT NULL
   AND EXISTS (SELECT 1 FROM sys.indexes
                WHERE name = N'idx_api_keys_hash'
                  AND object_id = OBJECT_ID(N'admin.tenant_api_keys'))
    DROP INDEX idx_api_keys_hash ON admin.tenant_api_keys;
GO

IF COL_LENGTH(N'admin.tenant_api_keys', N'revoked_at') IS NOT NULL
    ALTER TABLE admin.tenant_api_keys DROP COLUMN revoked_at;
GO

IF NOT EXISTS (SELECT 1 FROM sys.indexes
                WHERE name = N'idx_api_keys_hash'
                  AND object_id = OBJECT_ID(N'admin.tenant_api_keys'))
    CREATE INDEX idx_api_keys_hash ON admin.tenant_api_keys(key_hash) WHERE disabled_at IS NULL;
GO
