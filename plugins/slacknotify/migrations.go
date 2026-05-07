package slacknotify

import "github.com/rcownie/cleat/internal/plugin"

// Migrations returns the database schema for Slack config storage. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS slack_config (
					tenant_id       UUID NOT NULL,
					id              UUID PRIMARY KEY,
					name            TEXT NOT NULL,
					webhook_url     TEXT NOT NULL,
					default_channel TEXT,
					enabled         BOOLEAN NOT NULL DEFAULT true,
					created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_slack_config_tenant
					ON slack_config(tenant_id, created_at DESC);
			`,
			Down: `
				DROP TABLE IF EXISTS slack_config;
			`,
		},
	}
}
