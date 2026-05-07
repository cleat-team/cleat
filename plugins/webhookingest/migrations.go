package webhookingest

import "github.com/rcownie/cleat/internal/plugin"

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
			Down: `
				DROP TABLE IF EXISTS webhook_events;
				DROP TABLE IF EXISTS webhook_sources;
			`,
		},
		{
			Version: 3,
			Up: `ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS signal_workflow_id TEXT;
ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS signal_name TEXT NOT NULL DEFAULT 'webhook_received';`,
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
			Down: `
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS error_msg;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS status;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS last_retry_at;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS retry_count;
			`,
		},
	}
}
