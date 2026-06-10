-- 008_workflow_tags_routing.sql: Deployment tags and traffic routing for workflow versioning.

CREATE TABLE IF NOT EXISTS workflow_tags (
    workflow_name VARCHAR(255) NOT NULL,
    version INTEGER NOT NULL,
    tag VARCHAR(255) NOT NULL,
    created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    tenant_id CHAR(36),
    PRIMARY KEY (workflow_name, tag),
    FOREIGN KEY (workflow_name, version) REFERENCES workflow_defs(name, version)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS workflow_routing (
    id CHAR(36) NOT NULL,
    workflow_name VARCHAR(255) NOT NULL,
    target_version INTEGER NOT NULL,
    weight DOUBLE NOT NULL DEFAULT 1.0,
    created_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    tenant_id CHAR(36),
    PRIMARY KEY (id),
    FOREIGN KEY (workflow_name, target_version) REFERENCES workflow_defs(name, version)
) ENGINE=InnoDB;
