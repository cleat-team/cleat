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
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS blob_content (
					sha256      LONGBLOB PRIMARY KEY,
					size        BIGINT NOT NULL,
					data        LONGBLOB NOT NULL,
					ref_count   INT NOT NULL DEFAULT 1,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE TABLE IF NOT EXISTS blob_index (
					` + "`key`" + `       VARCHAR(255) NOT NULL,
					tenant_id   CHAR(36) NOT NULL,
					sha256      LONGBLOB NOT NULL REFERENCES blob_content(sha256),
					size        BIGINT NOT NULL,
					content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
					tags        JSON NOT NULL DEFAULT ('{}'),
					expires_at  TIMESTAMP(6),
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (tenant_id, ` + "`key`" + `)
				);

				CREATE INDEX idx_blob_tenant_created ON blob_index(tenant_id, created_at DESC);
				CREATE INDEX idx_blob_expires ON blob_index(expires_at);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'blob_content')
				CREATE TABLE blob_content (
					sha256      VARBINARY(MAX) PRIMARY KEY,
					size        BIGINT NOT NULL,
					data        VARBINARY(MAX) NOT NULL,
					ref_count   INT NOT NULL DEFAULT 1,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'blob_index')
				CREATE TABLE blob_index (
					[key]        NVARCHAR(255) NOT NULL,
					tenant_id    UNIQUEIDENTIFIER NOT NULL,
					sha256       VARBINARY(MAX) NOT NULL REFERENCES blob_content(sha256),
					size         BIGINT NOT NULL,
					content_type NVARCHAR(MAX) NOT NULL DEFAULT 'application/octet-stream',
					tags         NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					expires_at   DATETIMEOFFSET,
					created_at   DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (tenant_id, [key])
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_blob_tenant_created' AND object_id = OBJECT_ID('blob_index'))
				CREATE INDEX idx_blob_tenant_created ON blob_index(tenant_id, created_at DESC);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_blob_expires' AND object_id = OBJECT_ID('blob_index'))
				CREATE INDEX idx_blob_expires ON blob_index(expires_at) WHERE expires_at IS NOT NULL;
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
			UpMySQL: `
				ALTER TABLE blob_content ADD COLUMN storage_backend TEXT NOT NULL DEFAULT 'memory';
				ALTER TABLE blob_content ADD COLUMN s3_key TEXT;
				ALTER TABLE blob_content MODIFY COLUMN data LONGBLOB NULL;
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('blob_content') AND name = 'storage_backend')
				ALTER TABLE blob_content ADD storage_backend NVARCHAR(MAX) NOT NULL DEFAULT 'memory';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('blob_content') AND name = 's3_key')
				ALTER TABLE blob_content ADD s3_key NVARCHAR(MAX);
				ALTER TABLE blob_content ALTER COLUMN data VARBINARY(MAX) NULL;
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
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS workflow_blob_refs (
					workflow_id VARCHAR(255) NOT NULL,
					sha256      LONGBLOB NOT NULL,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (workflow_id, sha256)
				);

				ALTER TABLE blob_index ADD COLUMN deleted_at TIMESTAMP(6);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'workflow_blob_refs')
				CREATE TABLE workflow_blob_refs (
					workflow_id NVARCHAR(255) NOT NULL,
					sha256      VARBINARY(MAX) NOT NULL,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (workflow_id, sha256)
				);

				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('blob_index') AND name = 'deleted_at')
				ALTER TABLE blob_index ADD deleted_at DATETIMEOFFSET;
			`,
			Down: `
				DROP TABLE IF EXISTS workflow_blob_refs;
				ALTER TABLE blob_index DROP COLUMN IF EXISTS deleted_at;
			`,
		},
	}
}
