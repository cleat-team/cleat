-- 009_workflow_tags_routing.sql: Deployment tags and traffic routing for workflow versioning.

-- ===========================================================================
-- Table: dbo.workflow_tags
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_tags') AND type = N'U')
CREATE TABLE dbo.workflow_tags (
    workflow_name NVARCHAR(255)   NOT NULL,
    version       INT             NOT NULL,
    tag           NVARCHAR(255)   NOT NULL,
    created_at    DATETIMEOFFSET  NOT NULL DEFAULT SYSUTCDATETIME(),
    tenant_id     UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    CONSTRAINT pk_workflow_tags PRIMARY KEY (workflow_name, tag),
    CONSTRAINT fk_workflow_tags_def FOREIGN KEY (workflow_name, version)
        REFERENCES dbo.workflow_defs(name, version)
);

-- ===========================================================================
-- Table: dbo.workflow_routing
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_routing') AND type = N'U')
CREATE TABLE dbo.workflow_routing (
    id              UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID(),
    workflow_name   NVARCHAR(255)    NOT NULL,
    target_version  INT              NOT NULL,
    weight          FLOAT(53)        NOT NULL DEFAULT 1.0,
    created_at      DATETIMEOFFSET   NOT NULL DEFAULT SYSUTCDATETIME(),
    tenant_id       UNIQUEIDENTIFIER NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    CONSTRAINT pk_workflow_routing PRIMARY KEY (id),
    CONSTRAINT fk_workflow_routing_def FOREIGN KEY (workflow_name, target_version)
        REFERENCES dbo.workflow_defs(name, version),
    CONSTRAINT ck_workflow_routing_weight CHECK (weight >= 0 AND weight <= 1)
);

-- ===========================================================================
-- Row-Level Security: apply the existing tenant filter to the new tables.
-- The fn_tenant_filter function is already defined in 002_constraints.sql.
-- ===========================================================================

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Tags')
    DROP SECURITY POLICY dbo.TenantFilter_Tags;

IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'TenantFilter_Routing')
    DROP SECURITY POLICY dbo.TenantFilter_Routing;

CREATE SECURITY POLICY dbo.TenantFilter_Tags
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_tags
    WITH (STATE = ON);

CREATE SECURITY POLICY dbo.TenantFilter_Routing
    ADD FILTER PREDICATE dbo.fn_tenant_filter(tenant_id) ON dbo.workflow_routing
    WITH (STATE = ON);
