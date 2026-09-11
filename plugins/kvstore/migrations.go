package kvstore

import "github.com/cleat-team/cleat/plugin"

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
		{
			// Tenant isolation for kv_store. cleat#1277.
			//
			// A separate version rather than a TenantScoped on v1, because
			// v1 is already recorded as applied everywhere kvstore runs and
			// a recorded migration never runs again -- editing it would
			// protect new databases and leave every existing one open.
			//
			// kvstore is the first plugin to take this, and it qualifies
			// because every one of its access sites is request-scoped: all
			// four handlers read the tenant from the request context and
			// already answer 401 "tenant required" without one. A plugin
			// that also sweeps across tenants from a background loop cannot
			// adopt this yet -- the policy fails closed and those sweeps
			// have no tenant in context.
			//
			// Up is empty on purpose: the policy is emitted by the runtime
			// from TenantScoped. On MySQL and SQL Server this version is
			// recorded and does nothing, which is what the field documents.
			Version:      2,
			TenantScoped: []string{"kv_store"},
		},
	}
}
