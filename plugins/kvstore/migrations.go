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
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS kv_store (
					tenant_id   CHAR(36) NOT NULL,
					` + "`key`" + `       VARCHAR(255) NOT NULL,
					value       JSON NOT NULL DEFAULT ('null'),
					version     INT NOT NULL DEFAULT 1,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (tenant_id, ` + "`key`" + `)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'kv_store')
				CREATE TABLE kv_store (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					[key]       NVARCHAR(255) NOT NULL,
					value       NVARCHAR(MAX) NOT NULL DEFAULT ('null'),
					version     INT NOT NULL DEFAULT 1,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (tenant_id, [key])
				);
			`,
			Down: `
				DROP TABLE IF EXISTS kv_store;
			`,
		},
	}
}
