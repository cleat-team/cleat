-- cleat#3552 (B2b-2): a tenant created through the provisioning API records WHO created
-- it, and a key minted in that handshake is marked provisional until the creator confirms
-- it was stored. See migrations/postgres/006 for the full reasoning; this is the same change.
--
-- Each ALTER is guarded on its own column, so a re-run is a no-op for whichever already
-- landed. No table, policy or routine is created or redefined.
IF COL_LENGTH(N'admin.tenants', N'created_by') IS NULL
    ALTER TABLE admin.tenants ADD created_by NVARCHAR(255) NULL;
GO

IF COL_LENGTH(N'admin.tenant_api_keys', N'provisional') IS NULL
    ALTER TABLE admin.tenant_api_keys ADD
        provisional BIT NOT NULL
            CONSTRAINT df_tenant_api_keys_provisional DEFAULT 0;
GO
