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
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS dd_config (
					tenant_id      CHAR(36) NOT NULL,
					id             CHAR(36) PRIMARY KEY,
					` + "`name`" + `         TEXT,
					api_key        TEXT NOT NULL,
					site           VARCHAR(255) DEFAULT 'datadoghq.com',
					metrics_prefix VARCHAR(255) DEFAULT 'cleat',
					enabled        TINYINT(1) DEFAULT 1,
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'dd_config')
				CREATE TABLE dd_config (
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					id             UNIQUEIDENTIFIER PRIMARY KEY,
					[name]         NVARCHAR(MAX),
					api_key        NVARCHAR(MAX) NOT NULL,
					site           NVARCHAR(MAX) DEFAULT 'datadoghq.com',
					metrics_prefix NVARCHAR(MAX) DEFAULT 'cleat',
					enabled        BIT DEFAULT 1,
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);
			`,
			Down: `
				DROP TABLE IF EXISTS dd_config;
			`,
		},
		{
			Version: 2,
			Up: `
				CREATE TABLE IF NOT EXISTS plugin_lease (
					name       TEXT PRIMARY KEY,
					holder     TEXT NOT NULL,
					expires_at TIMESTAMPTZ NOT NULL
				);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS plugin_lease (
					name       VARCHAR(255) PRIMARY KEY,
					holder     VARCHAR(255) NOT NULL,
					expires_at TIMESTAMP(6) NOT NULL
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'plugin_lease')
				CREATE TABLE plugin_lease (
					name       NVARCHAR(255) PRIMARY KEY,
					holder     NVARCHAR(255) NOT NULL,
					expires_at DATETIMEOFFSET NOT NULL
				);
			`,
			Down: `
				DROP TABLE IF EXISTS plugin_lease;
			`,
		},
	}
}
