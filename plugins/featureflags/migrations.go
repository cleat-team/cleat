package featureflags

import "github.com/rcownie/durable/internal/plugin"

// Migrations returns the database schema for feature flags. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS feature_flags (
					tenant_id          UUID NOT NULL,
					id                 UUID PRIMARY KEY,
					key                TEXT NOT NULL,
					name               TEXT,
					description        TEXT,
					enabled            BOOLEAN NOT NULL DEFAULT false,
					rules              JSONB NOT NULL DEFAULT '[]',
					rollout_percentage INTEGER NOT NULL DEFAULT 0,
					created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
					UNIQUE (tenant_id, key)
				);

				CREATE INDEX IF NOT EXISTS idx_feature_flags_tenant_key
					ON feature_flags (tenant_id, key);
			`,
			Down: `
				DROP TABLE IF EXISTS feature_flags;
			`,
		},
	}
}
