-- cleat migration 082 (mssql): an org groups a customer's tenants
--
-- cleat#1898. See migrations/postgres/091_an_org_groups_a_customers_tenants.sql
-- for the full reasoning: admin.orgs carries identity only, org_id is
-- immutable by trigger rather than by convention, and existing tenants get a
-- default org rather than a nullable sentinel.
--
-- NO SECURITY POLICY TO TOGGLE, unlike 080/081's workflow_schedules --
-- checked, not assumed. TenantFilter_Defs/Instances/EventHistory/Signals/
-- Schedules/Tags/Routing (001_schema.sql) are the complete list of RLS
-- filter predicates in this dialect, and none targets admin.tenants:
--
--     grep -n "ON admin.tenants" migrations/mssql/*.sql
--
-- returns only the object_id existence check below, never a security
-- policy. That is the right shape: admin.tenants is the identity/routing
-- table itself, not tenant-owned data, so nothing needs to see it filtered.
--
-- UNIQUEIDENTIFIER for org_id, matching admin.tenants.tenant_id's own type.

IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'admin.orgs') AND type = N'U')
CREATE TABLE admin.orgs (
    org_id     UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
    name       NVARCHAR(255)    NOT NULL,
    created_at DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    suspended  BIT              NOT NULL DEFAULT 0,
    CONSTRAINT pk_admin_orgs PRIMARY KEY (org_id),
    CONSTRAINT uq_admin_orgs_name UNIQUE (name)
);
GO

IF NOT EXISTS (SELECT 1 FROM admin.orgs WHERE org_id = '00000000-0000-0000-0000-000000000000')
    INSERT INTO admin.orgs (org_id, name) VALUES ('00000000-0000-0000-0000-000000000000', N'default');
GO

IF COL_LENGTH(N'admin.tenants', N'org_id') IS NULL
    ALTER TABLE admin.tenants ADD org_id UNIQUEIDENTIFIER NULL;
GO

UPDATE admin.tenants SET org_id = '00000000-0000-0000-0000-000000000000' WHERE org_id IS NULL;
GO

ALTER TABLE admin.tenants ALTER COLUMN org_id UNIQUEIDENTIFIER NOT NULL;
GO

-- A DEFAULT CONSTRAINT, not just NOT NULL -- see the postgres migration's
-- comment at the same step. SQL Server's ALTER COLUMN carries no DEFAULT of
-- its own; a default needs its own named constraint. Without one, a direct
-- INSERT into admin.tenants that does not name org_id fails, and roughly a
-- dozen call sites across engine/*_test.go do exactly that.
IF NOT EXISTS (SELECT 1 FROM sys.default_constraints WHERE name = N'df_tenants_org_id')
    ALTER TABLE admin.tenants
        ADD CONSTRAINT df_tenants_org_id DEFAULT '00000000-0000-0000-0000-000000000000' FOR org_id;
GO

IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = N'tenants_org_id_fk')
    ALTER TABLE admin.tenants
        ADD CONSTRAINT tenants_org_id_fk FOREIGN KEY (org_id) REFERENCES admin.orgs(org_id);
GO

-- org_id IS IMMUTABLE. SQL Server has no BEFORE trigger -- only AFTER,
-- reading the change through the inserted/deleted virtual tables, and
-- rolling back if it finds one. This fires for the connection's own
-- transaction regardless of role, the same property the postgres and mysql
-- triggers have and a REVOKEd privilege would not: the table owner and
-- sysadmin are not exempt from a trigger the way they are from GRANT/REVOKE.
IF EXISTS (SELECT 1 FROM sys.triggers WHERE name = N'trg_tenants_org_id_immutable')
    DROP TRIGGER admin.trg_tenants_org_id_immutable;
GO

CREATE TRIGGER admin.trg_tenants_org_id_immutable
ON admin.tenants
AFTER UPDATE
AS
BEGIN
    SET NOCOUNT ON;
    IF EXISTS (
        SELECT 1
        FROM inserted i
        JOIN deleted d ON i.tenant_id = d.tenant_id
        WHERE i.org_id <> d.org_id
    )
    BEGIN
        RAISERROR(N'admin.tenants.org_id is immutable and cannot be changed', 16, 1);
        ROLLBACK TRANSACTION;
    END
END;
GO
