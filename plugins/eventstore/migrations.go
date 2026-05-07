package eventstore

import "github.com/rcownie/cleat/internal/plugin"

// Migrations returns the database schema for event streams. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS event_stream (
					tenant_id   UUID NOT NULL,
					stream_id   TEXT NOT NULL,
					sequence    BIGINT NOT NULL,
					event       JSONB NOT NULL,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (tenant_id, stream_id, sequence)
				);

				CREATE INDEX IF NOT EXISTS idx_event_stream_lookup
					ON event_stream (tenant_id, stream_id, sequence);
			`,
			Down: `
				DROP TABLE IF EXISTS event_stream;
			`,
		},
	}
}
