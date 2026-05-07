package auditlog

import "github.com/rcownie/cleat/internal/plugin"

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
			Down: `
				DROP TABLE IF EXISTS audit_events;
			`,
		},
	}
}
