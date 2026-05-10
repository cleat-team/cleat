package scheduler

import "github.com/rcownie/cleat/internal/plugin"

// Migrations returns the database schema for the schedules table.
// Tables are idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS schedules (
					tenant_id     UUID NOT NULL,
					id            UUID PRIMARY KEY,
					name          TEXT NOT NULL,
					cron          TEXT NOT NULL,
					workflow_name TEXT NOT NULL,
					input         JSONB DEFAULT '{}',
					enabled       BOOLEAN DEFAULT true,
					last_run_at   TIMESTAMPTZ,
					next_run_at   TIMESTAMPTZ,
					created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_schedules_tenant_enabled_next
					ON schedules (tenant_id, enabled, next_run_at);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS schedules (
					tenant_id     CHAR(36) NOT NULL,
					id            CHAR(36) PRIMARY KEY,
					` + "`name`" + `        VARCHAR(255) NOT NULL,
					cron          VARCHAR(255) NOT NULL,
					workflow_name VARCHAR(255) NOT NULL,
					input         JSON DEFAULT ('{}'),
					enabled       TINYINT(1) DEFAULT 1,
					last_run_at   TIMESTAMP(6),
					next_run_at   TIMESTAMP(6),
					created_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE INDEX idx_schedules_tenant_enabled_next
					ON schedules (tenant_id, enabled, next_run_at);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'schedules')
				CREATE TABLE schedules (
					tenant_id     UNIQUEIDENTIFIER NOT NULL,
					id            UNIQUEIDENTIFIER PRIMARY KEY,
					[name]        NVARCHAR(MAX) NOT NULL,
					cron          NVARCHAR(MAX) NOT NULL,
					workflow_name NVARCHAR(MAX) NOT NULL,
					input         NVARCHAR(MAX) DEFAULT ('{}'),
					enabled       BIT DEFAULT 1,
					last_run_at   DATETIMEOFFSET,
					next_run_at   DATETIMEOFFSET,
					created_at    DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at    DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_schedules_tenant_enabled_next' AND object_id = OBJECT_ID('schedules'))
				CREATE INDEX idx_schedules_tenant_enabled_next
					ON schedules (tenant_id, enabled, next_run_at);
			`,
			Down: `
				DROP INDEX IF EXISTS idx_schedules_tenant_enabled_next;
				DROP TABLE IF EXISTS schedules;
			`,
		},
	}
}
