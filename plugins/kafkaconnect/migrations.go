package kafkaconnect

import "github.com/rcownie/durable/internal/plugin"

// Migrations returns the database schema for Kafka config storage. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS kafka_config (
					tenant_id       UUID NOT NULL,
					id              UUID PRIMARY KEY,
					name            TEXT NOT NULL,
					brokers         TEXT NOT NULL,
					topic           TEXT NOT NULL,
					consumer_group  TEXT NOT NULL DEFAULT 'cleat-consumer',
					enabled         BOOLEAN NOT NULL DEFAULT true,
					created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_kafka_config_tenant
					ON kafka_config(tenant_id, created_at DESC);
			`,
			Down: `
				DROP TABLE IF EXISTS kafka_config;
			`,
		},
	}
}
