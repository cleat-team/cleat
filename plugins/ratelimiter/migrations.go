package ratelimiter

import "github.com/cleat-team/cleat/plugin"

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
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS rate_limits (
					tenant_id      CHAR(36) NOT NULL,
					limit_key      VARCHAR(255) NOT NULL,
					max_requests   INT NOT NULL,
					window_seconds INT NOT NULL,
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (tenant_id, limit_key)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'rate_limits')
				CREATE TABLE rate_limits (
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					limit_key      NVARCHAR(255) NOT NULL,
					max_requests   INT NOT NULL,
					window_seconds INT NOT NULL,
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (tenant_id, limit_key)
				);
			`,
			Down: `
				DROP TABLE IF EXISTS rate_limits;
			`,
		},
		{
			Version: 2,
			Up: `
				CREATE TABLE IF NOT EXISTS rate_counter (
					tenant_id    UUID NOT NULL,
					limit_key    TEXT NOT NULL,
					window_start TIMESTAMPTZ NOT NULL,
					count        INTEGER NOT NULL DEFAULT 0,
					PRIMARY KEY (tenant_id, limit_key, window_start)
				);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS rate_counter (
					tenant_id    CHAR(36) NOT NULL,
					limit_key    VARCHAR(255) NOT NULL,
					window_start TIMESTAMP(6) NOT NULL,
					count        INT NOT NULL DEFAULT 0,
					PRIMARY KEY (tenant_id, limit_key, window_start)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'rate_counter')
				CREATE TABLE rate_counter (
					tenant_id    UNIQUEIDENTIFIER NOT NULL,
					limit_key    NVARCHAR(255) NOT NULL,
					window_start DATETIMEOFFSET NOT NULL,
					count        INT NOT NULL DEFAULT 0,
					PRIMARY KEY (tenant_id, limit_key, window_start)
				);
			`,
			Down: `
				DROP TABLE IF EXISTS rate_counter;
			`,
		},
		{
			// Tenant isolation for both of this plugin's tables. cleat#1278.
			//
			// A separate version rather than a TenantScoped on v1 or v2: both
			// are already recorded as applied wherever ratelimiter runs, and a
			// recorded migration never runs again.
			//
			// NUMBERED 3 after reading the WHOLE list. The kafkaconnect
			// conversion numbered its migration 2 against a plugin that already
			// had one, because the version list was surveyed with a `head -10`
			// that truncated it. A duplicate version is silently SKIPPED rather
			// than refused, so the policy is never created and the failure
			// surfaces as "the policy does not filter" -- pointing at RLS
			// rather than at the migration that never ran.
			//
			// BOTH tables, and they are scoped for different reasons. rate_limits
			// is configuration: one tenant's operator setting their own limits.
			// rate_counter is per-tenant state keyed (tenant_id, limit_key,
			// window_start). Neither is a global-by-design table like
			// datadogexport's plugin_lease, which has no tenant_id at all.
			//
			// Up is empty on purpose: the runtime emits both policies from
			// TenantScoped.
			Version:      3,
			TenantScoped: []string{"rate_limits", "rate_counter"},
		},
	}
}
