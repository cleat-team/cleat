package blobstore

import "github.com/rcownie/cleat/internal/plugin"

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
		{
			Version: 2,
			Up: `
				ALTER TABLE blob_content ADD COLUMN IF NOT EXISTS storage_backend TEXT NOT NULL DEFAULT 'memory';
				ALTER TABLE blob_content ADD COLUMN IF NOT EXISTS s3_key TEXT;
				ALTER TABLE blob_content ALTER COLUMN data DROP NOT NULL;
			`,
			Down: `
				ALTER TABLE blob_content ALTER COLUMN data SET NOT NULL;
				ALTER TABLE blob_content DROP COLUMN IF EXISTS s3_key;
				ALTER TABLE blob_content DROP COLUMN IF EXISTS storage_backend;
			`,
		},
		{
			Version: 3,
			Up: `
				CREATE TABLE IF NOT EXISTS workflow_blob_refs (
					workflow_id TEXT NOT NULL,
					sha256      BYTEA NOT NULL,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (workflow_id, sha256)
				);

				ALTER TABLE blob_index ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
			`,
			Down: `
				DROP TABLE IF EXISTS workflow_blob_refs;
				ALTER TABLE blob_index DROP COLUMN IF EXISTS deleted_at;
			`,
		},
	}
}
