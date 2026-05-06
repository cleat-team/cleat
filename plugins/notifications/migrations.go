package notifications

import "github.com/rcownie/durable/internal/plugin"

// Migrations returns the database schema for webhook storage and delivery
// tracking. Tables are idempotent (IF NOT EXISTS) and safe to run multiple
// times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS webhook_config (
					tenant_id   UUID NOT NULL,
					id          UUID PRIMARY KEY,
					url         TEXT NOT NULL,
					secret      TEXT NOT NULL DEFAULT '',
					events      JSONB NOT NULL DEFAULT '[]',
					enabled     BOOLEAN NOT NULL DEFAULT true,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE TABLE IF NOT EXISTS webhook_delivery (
					id               UUID PRIMARY KEY,
					webhook_id       UUID NOT NULL REFERENCES webhook_config(id),
					event_type       TEXT NOT NULL,
					payload          JSONB NOT NULL DEFAULT '{}',
					status           TEXT NOT NULL DEFAULT 'pending',
					attempt_count    INTEGER NOT NULL DEFAULT 0,
					last_attempt_at  TIMESTAMPTZ,
					next_attempt_at  TIMESTAMPTZ,
					delivered_at     TIMESTAMPTZ,
					response_code    INTEGER,
					response_body    TEXT,
					created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_webhook_config_tenant ON webhook_config(tenant_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_delivery_webhook ON webhook_delivery(webhook_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_delivery_status ON webhook_delivery(status, next_attempt_at);
			`,
			Down: `
				DROP TABLE IF EXISTS webhook_delivery;
				DROP TABLE IF EXISTS webhook_config;
			`,
		},
	}
}
