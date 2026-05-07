package eventtriggers

import "github.com/rcownie/cleat/internal/plugin"

// Migrations returns the database schema for event subscription management and
// event storage. Tables are idempotent (IF NOT EXISTS) and safe to run
// multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS event_subscriptions (
					id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
					tenant_id UUID NOT NULL,
					event_type TEXT NOT NULL,
					def_name TEXT NOT NULL,
					entry_point TEXT DEFAULT '',
					input_template JSONB DEFAULT '{}',
					filter_expr TEXT DEFAULT '',
					enabled BOOLEAN DEFAULT true,
					created_at TIMESTAMPTZ DEFAULT now()
				);
				CREATE TABLE IF NOT EXISTS ingested_events (
					id UUID PRIMARY KEY,
					tenant_id UUID NOT NULL,
					event_type TEXT NOT NULL,
					event_data JSONB DEFAULT '{}',
					received_at TIMESTAMPTZ DEFAULT now(),
					processed BOOLEAN DEFAULT false,
					error_msg TEXT
				);
				CREATE INDEX IF NOT EXISTS idx_event_subscriptions_type ON event_subscriptions(tenant_id, event_type);
				CREATE INDEX IF NOT EXISTS idx_ingested_events_unprocessed ON ingested_events(processed, received_at) WHERE NOT processed;
			`,
			Down: `
				DROP TABLE IF EXISTS ingested_events;
				DROP TABLE IF EXISTS event_subscriptions;
			`,
		},
		{
			Version: 2,
			Up: `
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS retry_count INTEGER DEFAULT 0;
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS last_retry_at TIMESTAMPTZ;
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS status TEXT DEFAULT 'pending';
				ALTER TABLE event_subscriptions ADD COLUMN IF NOT EXISTS max_retries INTEGER DEFAULT 3;
			`,
			Down: `
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS retry_count;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS last_retry_at;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS status;
				ALTER TABLE event_subscriptions DROP COLUMN IF EXISTS max_retries;
			`,
		},
		{
			Version: 3,
			Up: `
				CREATE TABLE IF NOT EXISTS event_awaiters (
					workflow_id TEXT NOT NULL,
					tenant_id UUID NOT NULL,
					event_type TEXT NOT NULL,
					created_at TIMESTAMPTZ DEFAULT now(),
					PRIMARY KEY (workflow_id, event_type)
				);
				CREATE INDEX IF NOT EXISTS idx_event_awaiters_type ON event_awaiters(tenant_id, event_type);
			`,
			Down: `
				DROP TABLE IF EXISTS event_awaiters;
			`,
		},
	}
}
