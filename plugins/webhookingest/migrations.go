package webhookingest

import "github.com/cleat-team/cleat/internal/plugin"

// Migrations returns the database schema for webhook source management and
// event storage. Tables are idempotent (IF NOT EXISTS) and safe to run
// multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS webhook_sources (
					tenant_id   UUID NOT NULL,
					id          UUID PRIMARY KEY,
					name        TEXT,
					source_type TEXT NOT NULL DEFAULT 'generic',
					secret      TEXT NOT NULL DEFAULT '',
					enabled     BOOLEAN NOT NULL DEFAULT true,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE TABLE IF NOT EXISTS webhook_events (
					id          UUID PRIMARY KEY,
					source_id   UUID NOT NULL REFERENCES webhook_sources(id),
					tenant_id   UUID NOT NULL,
					event_type  TEXT NOT NULL DEFAULT '',
					headers     JSONB NOT NULL DEFAULT '{}',
					payload     JSONB NOT NULL DEFAULT '{}',
					received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
					processed   BOOLEAN NOT NULL DEFAULT false
				);

				CREATE INDEX IF NOT EXISTS idx_webhook_sources_tenant ON webhook_sources(tenant_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_events_source ON webhook_events(source_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_events_tenant ON webhook_events(tenant_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_events_processed ON webhook_events(processed);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS webhook_sources (
					tenant_id   CHAR(36) NOT NULL,
					id          CHAR(36) PRIMARY KEY,
					` + "`name`" + `      VARCHAR(255),
					source_type VARCHAR(255) NOT NULL DEFAULT 'generic',
					secret      VARCHAR(255) NOT NULL DEFAULT '',
					enabled     TINYINT(1) NOT NULL DEFAULT 1,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE TABLE IF NOT EXISTS webhook_events (
					id          CHAR(36) PRIMARY KEY,
					source_id   CHAR(36) NOT NULL REFERENCES webhook_sources(id),
					tenant_id   CHAR(36) NOT NULL,
					event_type  VARCHAR(255) NOT NULL DEFAULT '',
					headers     JSON NOT NULL DEFAULT ('{}'),
					payload     JSON NOT NULL DEFAULT ('{}'),
					received_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					processed   TINYINT(1) NOT NULL DEFAULT 0
				);

				CREATE INDEX idx_webhook_sources_tenant ON webhook_sources(tenant_id);
				CREATE INDEX idx_webhook_events_source ON webhook_events(source_id);
				CREATE INDEX idx_webhook_events_tenant ON webhook_events(tenant_id);
				CREATE INDEX idx_webhook_events_processed ON webhook_events(processed);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'webhook_sources')
				CREATE TABLE webhook_sources (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					id          UNIQUEIDENTIFIER PRIMARY KEY,
					[name]      NVARCHAR(MAX),
					source_type NVARCHAR(MAX) NOT NULL DEFAULT 'generic',
					secret      NVARCHAR(MAX) NOT NULL DEFAULT '',
					enabled     BIT NOT NULL DEFAULT 1,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'webhook_events')
				CREATE TABLE webhook_events (
					id          UNIQUEIDENTIFIER PRIMARY KEY,
					source_id   UNIQUEIDENTIFIER NOT NULL REFERENCES webhook_sources(id),
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					event_type  NVARCHAR(MAX) NOT NULL DEFAULT '',
					headers     NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					payload     NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					received_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					processed   BIT NOT NULL DEFAULT 0
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_sources_tenant' AND object_id = OBJECT_ID('webhook_sources'))
				CREATE INDEX idx_webhook_sources_tenant ON webhook_sources(tenant_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_events_source' AND object_id = OBJECT_ID('webhook_events'))
				CREATE INDEX idx_webhook_events_source ON webhook_events(source_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_events_tenant' AND object_id = OBJECT_ID('webhook_events'))
				CREATE INDEX idx_webhook_events_tenant ON webhook_events(tenant_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_events_processed' AND object_id = OBJECT_ID('webhook_events'))
				CREATE INDEX idx_webhook_events_processed ON webhook_events(processed);
			`,
			Down: `
				DROP TABLE IF EXISTS webhook_events;
				DROP TABLE IF EXISTS webhook_sources;
			`,
		},
		{
			Version: 3,
			Up: `ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS signal_workflow_id TEXT;
	ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS signal_name TEXT NOT NULL DEFAULT 'webhook_received';`,
			UpMySQL: `ALTER TABLE webhook_sources ADD COLUMN signal_workflow_id VARCHAR(255);
	ALTER TABLE webhook_sources ADD COLUMN signal_name VARCHAR(255) NOT NULL DEFAULT 'webhook_received';`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'signal_workflow_id')
				ALTER TABLE webhook_sources ADD signal_workflow_id NVARCHAR(MAX);
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'signal_name')
				ALTER TABLE webhook_sources ADD signal_name NVARCHAR(MAX) NOT NULL DEFAULT 'webhook_received';
			`,
			Down: `ALTER TABLE webhook_sources DROP COLUMN IF EXISTS signal_workflow_id;
	ALTER TABLE webhook_sources DROP COLUMN IF EXISTS signal_name;`,
		},
		{
			Version: 4,
			Up: `
				ALTER TABLE webhook_events ADD COLUMN IF NOT EXISTS retry_count INTEGER DEFAULT 0;
				ALTER TABLE webhook_events ADD COLUMN IF NOT EXISTS last_retry_at TIMESTAMPTZ;
				ALTER TABLE webhook_events ADD COLUMN IF NOT EXISTS status TEXT DEFAULT 'pending';
				ALTER TABLE webhook_events ADD COLUMN IF NOT EXISTS error_msg TEXT;
			`,
			UpMySQL: `
				ALTER TABLE webhook_events ADD COLUMN retry_count INT DEFAULT 0;
				ALTER TABLE webhook_events ADD COLUMN last_retry_at TIMESTAMP(6);
				ALTER TABLE webhook_events ADD COLUMN ` + "`status`" + ` VARCHAR(255) DEFAULT 'pending';
				ALTER TABLE webhook_events ADD COLUMN error_msg TEXT;
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_events') AND name = 'retry_count')
				ALTER TABLE webhook_events ADD retry_count INT DEFAULT 0;
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_events') AND name = 'last_retry_at')
				ALTER TABLE webhook_events ADD last_retry_at DATETIMEOFFSET;
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_events') AND name = 'status')
				ALTER TABLE webhook_events ADD [status] NVARCHAR(MAX) DEFAULT 'pending';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_events') AND name = 'error_msg')
				ALTER TABLE webhook_events ADD error_msg NVARCHAR(MAX);
			`,
			Down: `
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS error_msg;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS status;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS last_retry_at;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS retry_count;
			`,
		},
	}
}
