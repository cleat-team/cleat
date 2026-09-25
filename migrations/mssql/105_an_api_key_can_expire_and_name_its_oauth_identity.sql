-- cleat migration 105 (mssql): an API key can expire and name its OAuth identity
--
-- See migrations/postgres/105_an_api_key_can_expire_and_name_its_oauth_identity.sql
-- for the full reasoning: cleat#2352, nullable expires_at (general capability,
-- enforced on every dialect) and nullable oauth_identity (OAuth-only, unused
-- here today), both NULL for every existing key. DATETIMEOFFSET, matching
-- this table's own created_at (001_schema.sql).
--
-- GUARDED, following 099's own precedent.

IF COL_LENGTH(N'admin.tenant_api_keys', N'expires_at') IS NULL
    ALTER TABLE admin.tenant_api_keys
        ADD expires_at DATETIMEOFFSET NULL;
GO

IF COL_LENGTH(N'admin.tenant_api_keys', N'oauth_identity') IS NULL
    ALTER TABLE admin.tenant_api_keys
        ADD oauth_identity NVARCHAR(512) NULL;
GO
