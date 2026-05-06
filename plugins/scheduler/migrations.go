package scheduler

import "github.com/rcownie/durable/internal/plugin"

// Migrations returns the database schema for the schedules table.
// Tables are idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS schedules (
					tenant_id     UUID NOT NULL,
					id            UUID PRIMARY KEY,
					name          TEXT NOT NULL,
					cron          TEXT NOT NULL,
					workflow_name TEXT NOT NULL,
					input         JSONB DEFAULT '{}',
					enabled       BOOLEAN DEFAULT true,
					last_run_at   TIMESTAMPTZ,
					next_run_at   TIMESTAMPTZ,
					created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_schedules_tenant_enabled_next
					ON schedules (tenant_id, enabled, next_run_at);
			`,
			Down: `
				DROP INDEX IF EXISTS idx_schedules_tenant_enabled_next;
				DROP TABLE IF EXISTS schedules;
			`,
		},
	}
}
