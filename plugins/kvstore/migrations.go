package kvstore

import "github.com/rcownie/cleat/internal/plugin"

// Migrations returns the database schema for the key-value store. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS kv_store (
					tenant_id   UUID NOT NULL,
					key         TEXT NOT NULL,
					value       JSONB NOT NULL DEFAULT 'null',
					version     INTEGER NOT NULL DEFAULT 1,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (tenant_id, key)
				);
			`,
			Down: `
				DROP TABLE IF EXISTS kv_store;
			`,
		},
	}
}
