package scheduledbackup

import "github.com/rcownie/durable/internal/plugin"

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
			Down: `
				DROP INDEX IF EXISTS idx_backup_history_tenant_config;
				DROP INDEX IF EXISTS idx_backup_config_tenant_enabled_next;
				DROP TABLE IF EXISTS backup_history;
				DROP TABLE IF EXISTS backup_config;
			`,
		},
	}
}
