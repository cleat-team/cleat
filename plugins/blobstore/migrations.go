package blobstore

import "github.com/rcownie/durable/internal/plugin"

// Migrations returns the database schema for blob storage. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS blob_content (
					sha256      BYTEA PRIMARY KEY,
					size        BIGINT NOT NULL,
					data        BYTEA NOT NULL,
					ref_count   INTEGER NOT NULL DEFAULT 1,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE TABLE IF NOT EXISTS blob_index (
					key         TEXT NOT NULL,
					tenant_id   UUID NOT NULL,
					sha256      BYTEA NOT NULL REFERENCES blob_content(sha256),
					size        BIGINT NOT NULL,
					content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
					tags        JSONB NOT NULL DEFAULT '{}',
					expires_at  TIMESTAMPTZ,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (tenant_id, key)
				);

				CREATE INDEX IF NOT EXISTS idx_blob_tags ON blob_index USING gin(tags);
				CREATE INDEX IF NOT EXISTS idx_blob_tenant_created ON blob_index(tenant_id, created_at DESC);
				CREATE INDEX IF NOT EXISTS idx_blob_expires ON blob_index(expires_at) WHERE expires_at IS NOT NULL;
			`,
			Down: `
				DROP TABLE IF EXISTS blob_index;
				DROP TABLE IF EXISTS blob_content;
			`,
		},
	}
}
