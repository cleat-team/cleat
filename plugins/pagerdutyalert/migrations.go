package pagerdutyalert

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for PagerDuty config storage. Tables
// are idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS pd_config (
					tenant_id   UUID NOT NULL,
					id          UUID PRIMARY KEY,
					name        TEXT NOT NULL,
					routing_key TEXT NOT NULL,
					enabled     BOOLEAN NOT NULL DEFAULT true,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_pd_config_tenant
					ON pd_config(tenant_id, created_at DESC);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS pd_config (
					tenant_id   CHAR(36) NOT NULL,
					id          CHAR(36) PRIMARY KEY,
					` + "`name`" + `      TEXT NOT NULL,
					routing_key TEXT NOT NULL,
					enabled     TINYINT(1) NOT NULL DEFAULT 1,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					INDEX idx_pd_config_tenant (tenant_id, created_at DESC)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'pd_config')
				CREATE TABLE pd_config (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					id          UNIQUEIDENTIFIER PRIMARY KEY,
					[name]      NVARCHAR(MAX) NOT NULL,
					routing_key NVARCHAR(MAX) NOT NULL,
					enabled     BIT NOT NULL DEFAULT 1,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_pd_config_tenant' AND object_id = OBJECT_ID('pd_config'))
				CREATE INDEX idx_pd_config_tenant
					ON pd_config(tenant_id, created_at DESC);
			`,
			Down: `
				DROP TABLE IF EXISTS pd_config;
			`,
		},
		{
			// Tenant isolation for pd_config. cleat#1512.
			//
			// A new version rather than TenantScoped on v1: v1 is recorded
			// everywhere this plugin runs and a recorded migration never runs
			// again, so editing it would protect new databases and leave every
			// existing one open. Up is empty by design -- the runtime emits
			// ENABLE / FORCE / the policy from the declaration.			//
			// Health() is marked cross-tenant in plugin.go: it asks whether the
			// deployment has any enabled config at all, which belongs to no
			// tenant and runs on no request.
			Version:      2,
			TenantScoped: []string{"pd_config"},
		},
	}
}
