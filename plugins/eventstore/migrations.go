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
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS event_stream (
					tenant_id   CHAR(36) NOT NULL,
					stream_id   VARCHAR(255) NOT NULL,
					sequence    BIGINT NOT NULL,
					event       JSON NOT NULL,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (tenant_id, stream_id, sequence)
				);

				CREATE INDEX idx_event_stream_lookup
					ON event_stream (tenant_id, stream_id, sequence);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'event_stream')
				CREATE TABLE event_stream (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					stream_id   NVARCHAR(255) NOT NULL,
					sequence    BIGINT NOT NULL,
					event       NVARCHAR(MAX) NOT NULL,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (tenant_id, stream_id, sequence)
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_event_stream_lookup' AND object_id = OBJECT_ID('event_stream'))
				CREATE INDEX idx_event_stream_lookup
					ON event_stream (tenant_id, stream_id, sequence);
			`,
			Down: `
				DROP TABLE IF EXISTS event_stream;
			`,
		},
	}
}
