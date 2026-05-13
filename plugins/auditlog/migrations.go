package auditlog

import "github.com/cleat-team/cleat/internal/plugin"

// Migrations returns the database schema for audit events.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS audit_events (
					id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
					tenant_id   UUID NOT NULL,
					timestamp   TIMESTAMPTZ NOT NULL DEFAULT now(),
					method      TEXT NOT NULL,
					path        TEXT NOT NULL,
					status_code INTEGER,
					user_id     TEXT,
					ip_address  TEXT,
					user_agent  TEXT,
					duration_ms INTEGER,
					metadata    JSONB DEFAULT '{}'
				);

				CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_ts
					ON audit_events (tenant_id, timestamp DESC);
				CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_path
					ON audit_events (tenant_id, path);
				CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_method
					ON audit_events (tenant_id, method);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS audit_events (
					id          CHAR(36) PRIMARY KEY,
					tenant_id   CHAR(36) NOT NULL,
					` + "`" + `timestamp` + "`" + ` TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					method      VARCHAR(255) NOT NULL,
					path        VARCHAR(700) NOT NULL,
					status_code INT,
					user_id     TEXT,
					ip_address  TEXT,
					user_agent  TEXT,
					duration_ms INT,
					metadata    JSON DEFAULT ('{}')
				);

				CREATE INDEX idx_audit_events_tenant_ts
					ON audit_events (tenant_id, ` + "`" + `timestamp` + "`" + ` DESC);
				CREATE INDEX idx_audit_events_tenant_path
					ON audit_events (tenant_id, path);
				CREATE INDEX idx_audit_events_tenant_method
					ON audit_events (tenant_id, method);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_events')
				CREATE TABLE audit_events (
					id          UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID(),
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					[timestamp] DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					method      NVARCHAR(255) NOT NULL,
					path        NVARCHAR(900) NOT NULL,
					status_code INT,
					user_id     NVARCHAR(MAX),
					ip_address  NVARCHAR(MAX),
					user_agent  NVARCHAR(MAX),
					duration_ms INT,
					metadata    NVARCHAR(MAX) DEFAULT ('{}')
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_audit_events_tenant_ts' AND object_id = OBJECT_ID('audit_events'))
				CREATE INDEX idx_audit_events_tenant_ts ON audit_events (tenant_id, [timestamp] DESC);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_audit_events_tenant_path' AND object_id = OBJECT_ID('audit_events'))
				CREATE INDEX idx_audit_events_tenant_path ON audit_events (tenant_id, path);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_audit_events_tenant_method' AND object_id = OBJECT_ID('audit_events'))
				CREATE INDEX idx_audit_events_tenant_method ON audit_events (tenant_id, method);
			`,
			Down: `
				DROP TABLE IF EXISTS audit_events;
			`,
		},
	}
}
