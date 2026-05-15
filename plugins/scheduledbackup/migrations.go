package scheduledbackup

import "github.com/cleat-team/cleat/internal/plugin"

// Migrations returns the database schema for backup configuration and history.
// Tables are idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS backup_config (
					tenant_id      UUID NOT NULL,
					id             UUID PRIMARY KEY,
					name           TEXT,
					cron           TEXT,
					s3_bucket      TEXT,
					s3_prefix      TEXT,
					retention_days INTEGER DEFAULT 30,
					enabled        BOOLEAN DEFAULT true,
					last_run_at    TIMESTAMPTZ,
					next_run_at    TIMESTAMPTZ,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE TABLE IF NOT EXISTS backup_history (
					id             UUID PRIMARY KEY,
					config_id      UUID REFERENCES backup_config(id),
					tenant_id      UUID NOT NULL,
					filename       TEXT,
					size_bytes     BIGINT,
					status         TEXT,
					started_at     TIMESTAMPTZ,
					completed_at   TIMESTAMPTZ,
					error_message  TEXT,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_backup_config_tenant_enabled_next
					ON backup_config (tenant_id, enabled, next_run_at);

				CREATE INDEX IF NOT EXISTS idx_backup_history_tenant_config
					ON backup_history (tenant_id, config_id);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS backup_config (
					tenant_id      CHAR(36) NOT NULL,
					id             CHAR(36) PRIMARY KEY,
					` + "`name`" + `           TEXT,
					cron           TEXT,
					s3_bucket      TEXT,
					s3_prefix      TEXT,
					retention_days INT DEFAULT 30,
					enabled        TINYINT(1) DEFAULT 1,
					last_run_at    TIMESTAMP(6),
					next_run_at    TIMESTAMP(6),
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE TABLE IF NOT EXISTS backup_history (
					id             CHAR(36) PRIMARY KEY,
					config_id      CHAR(36) REFERENCES backup_config(id),
					tenant_id      CHAR(36) NOT NULL,
					filename       TEXT,
					size_bytes     BIGINT,
					` + "`status`" + `       TEXT,
					started_at     TIMESTAMP(6),
					completed_at   TIMESTAMP(6),
					error_message  TEXT,
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE INDEX idx_backup_config_tenant_enabled_next
					ON backup_config (tenant_id, enabled, next_run_at);

				CREATE INDEX idx_backup_history_tenant_config
					ON backup_history (tenant_id, config_id);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'backup_config')
				CREATE TABLE backup_config (
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					id             UNIQUEIDENTIFIER PRIMARY KEY,
					[name]         NVARCHAR(MAX),
					cron           NVARCHAR(MAX),
					s3_bucket      NVARCHAR(MAX),
					s3_prefix      NVARCHAR(MAX),
					retention_days INT DEFAULT 30,
					enabled        BIT DEFAULT 1,
					last_run_at    DATETIMEOFFSET,
					next_run_at    DATETIMEOFFSET,
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'backup_history')
				CREATE TABLE backup_history (
					id             UNIQUEIDENTIFIER PRIMARY KEY,
					config_id      UNIQUEIDENTIFIER REFERENCES backup_config(id),
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					filename       NVARCHAR(MAX),
					size_bytes     BIGINT,
					[status]       NVARCHAR(MAX),
					started_at     DATETIMEOFFSET,
					completed_at   DATETIMEOFFSET,
					error_message  NVARCHAR(MAX),
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_backup_config_tenant_enabled_next' AND object_id = OBJECT_ID('backup_config'))
				CREATE INDEX idx_backup_config_tenant_enabled_next
					ON backup_config (tenant_id, enabled, next_run_at);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_backup_history_tenant_config' AND object_id = OBJECT_ID('backup_history'))
				CREATE INDEX idx_backup_history_tenant_config
					ON backup_history (tenant_id, config_id);
			`,
			Down: `
				DROP INDEX IF EXISTS idx_backup_history_tenant_config;
				DROP INDEX IF EXISTS idx_backup_config_tenant_enabled_next;
				DROP TABLE IF EXISTS backup_history;
				DROP TABLE IF EXISTS backup_config;
			`,
		},
	}
}
