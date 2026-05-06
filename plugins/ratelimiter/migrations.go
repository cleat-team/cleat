package ratelimiter

import "github.com/rcownie/durable/internal/plugin"

// Migrations returns the database schema for rate limit storage. The table is
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS rate_limits (
					tenant_id      UUID NOT NULL,
					limit_key      TEXT NOT NULL,
					max_requests   INTEGER NOT NULL,
					window_seconds INTEGER NOT NULL,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (tenant_id, limit_key)
				);
			`,
			Down: `
				DROP TABLE IF EXISTS rate_limits;
			`,
		},
	}
}
