-- 010_workflow_tags_routing.sql: Deployment tags and traffic routing for workflow versioning.

CREATE TABLE IF NOT EXISTS workflow_tags (
    workflow_name TEXT NOT NULL,
    version INTEGER NOT NULL,
    tag TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID,
    PRIMARY KEY (workflow_name, tag),
    FOREIGN KEY (workflow_name, version) REFERENCES workflow_defs(name, version)
);

CREATE TABLE IF NOT EXISTS workflow_routing (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_name TEXT NOT NULL,
    target_version INTEGER NOT NULL,
    weight REAL NOT NULL DEFAULT 1.0 CHECK (weight >= 0 AND weight <= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id UUID,
    FOREIGN KEY (workflow_name, target_version) REFERENCES workflow_defs(name, version)
);

ALTER TABLE workflow_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_routing ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_tags ON workflow_tags;
CREATE POLICY tenant_isolation_tags ON workflow_tags
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());

DROP POLICY IF EXISTS tenant_isolation_routing ON workflow_routing;
CREATE POLICY tenant_isolation_routing ON workflow_routing
    FOR ALL USING (tenant_id = cleat.assert_tenant_set());
