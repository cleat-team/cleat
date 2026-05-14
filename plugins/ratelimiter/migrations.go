package ratelimiter

import "github.com/cleat-team/cleat/internal/plugin"

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
	}
}
