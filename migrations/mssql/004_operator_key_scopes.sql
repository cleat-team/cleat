-- cleat#3552 (B2b-1): an operator key can be narrowed to a scope, and carries the
-- name of the principal it was issued to. See migrations/postgres/004 for the
-- full reasoning; this is the same change.
--
-- One ALTER TABLE ... ADD, so both columns land together or not at all. The guard
-- makes a re-run a no-op; it checks one of the two because they only ever arrive
-- together.
IF COL_LENGTH(N'admin.operator_api_keys', N'scopes') IS NULL
    ALTER TABLE admin.operator_api_keys ADD
        scopes NVARCHAR(255) NOT NULL
            CONSTRAINT df_operator_api_keys_scopes DEFAULT N'admin',
        principal NVARCHAR(128) NULL;
GO
