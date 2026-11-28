-- cleat#3552 (B3b-2): dropping a tenant over the admin API leaves a TOMBSTONE. See
-- migrations/postgres/007 for the full reasoning; this is the same change.
--
-- THE KEY COLUMN IS NOT NAMED tenant_id, and here that is load bearing rather than tidy:
-- admin.drop_tenant on SQL Server deletes from EVERY table that has a tenant_id column
-- (migrations/mssql/003_procedures.sql), so a tombstone keyed by tenant_id would be deleted by the
-- drop that wrote it.
--
-- The application role reads and writes it through the schema-level grants on [admin] that every
-- other admin table relies on. The row carries no UPDATE path in the code that uses it.
-- admin.drop_tenant is NOT redefined here.
IF OBJECT_ID(N'admin.tenant_tombstones', N'U') IS NULL
    CREATE TABLE admin.tenant_tombstones (
        dropped_tenant_id uniqueidentifier NOT NULL,
        name              nvarchar(255)    NOT NULL,
        org_id            uniqueidentifier NOT NULL,
        created_by        nvarchar(255)    NULL,
        dropped_by        nvarchar(255)    NOT NULL,
        dropped_at        datetimeoffset(7) NOT NULL
            CONSTRAINT df_tenant_tombstones_dropped_at DEFAULT (sysutcdatetime()),
        audit_chain_seq   bigint           NULL,
        audit_chain_hash  char(64)         NULL,
        CONSTRAINT pk_tenant_tombstones PRIMARY KEY CLUSTERED (dropped_tenant_id)
    );
GO
