package datadogexport

import "github.com/rcownie/cleat/internal/plugin"

// Migrations returns the database schema for Datadog export configs. The
// table is idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS dd_config (
					tenant_id      UUID NOT NULL,
					id             UUID PRIMARY KEY,
					name           TEXT,
					api_key        TEXT NOT NULL,
					site           TEXT DEFAULT 'datadoghq.com',
					metrics_prefix TEXT DEFAULT 'cleat',
					enabled        BOOLEAN DEFAULT true,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
				);
			`,
			Down: `
				DROP TABLE IF EXISTS dd_config;
			`,
		},
	}
}
