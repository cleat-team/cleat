package oauthprovider

import "github.com/rcownie/cleat/internal/plugin"

// Migrations returns the database schema for OAuth configuration and sessions.
// Tables are idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS oauth_config (
					tenant_id      UUID NOT NULL,
					provider       TEXT NOT NULL,
					client_id      TEXT NOT NULL,
					client_secret  TEXT NOT NULL,
					redirect_url   TEXT NOT NULL,
					domain         TEXT NOT NULL DEFAULT '',
					enabled        BOOLEAN NOT NULL DEFAULT true,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (tenant_id, provider)
				);

				CREATE TABLE IF NOT EXISTS oauth_sessions (
					id             UUID PRIMARY KEY,
					tenant_id      UUID NOT NULL,
					provider       TEXT NOT NULL,
					session_token  TEXT NOT NULL,
					user_email     TEXT,
					access_token   TEXT,
					refresh_token  TEXT,
					expires_at     TIMESTAMPTZ,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_oauth_sessions_tenant_user ON oauth_sessions(tenant_id, user_email);
				CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_sessions_token ON oauth_sessions(session_token);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS oauth_config (
					tenant_id      CHAR(36) NOT NULL,
					provider       VARCHAR(255) NOT NULL,
					client_id      TEXT NOT NULL,
					client_secret  TEXT NOT NULL,
					redirect_url   TEXT NOT NULL,
					domain         VARCHAR(900) NOT NULL DEFAULT '',
					enabled        TINYINT(1) NOT NULL DEFAULT 1,
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (tenant_id, provider)
				);

				CREATE TABLE IF NOT EXISTS oauth_sessions (
					id             CHAR(36) PRIMARY KEY,
					tenant_id      CHAR(36) NOT NULL,
					provider       TEXT NOT NULL,
					session_token  VARCHAR(255) NOT NULL,
					user_email     VARCHAR(255) NULL,
					access_token   TEXT NULL,
					refresh_token  TEXT NULL,
					expires_at     TIMESTAMP(6) NULL,
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE INDEX idx_oauth_sessions_tenant_user ON oauth_sessions(tenant_id, user_email);
				CREATE UNIQUE INDEX idx_oauth_sessions_token ON oauth_sessions(session_token);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'oauth_config')
				CREATE TABLE oauth_config (
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					provider       NVARCHAR(255) NOT NULL,
					client_id      NVARCHAR(MAX) NOT NULL,
					client_secret  NVARCHAR(MAX) NOT NULL,
					redirect_url   NVARCHAR(MAX) NOT NULL,
					domain         NVARCHAR(MAX) NOT NULL DEFAULT '',
					enabled        BIT NOT NULL DEFAULT 1,
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (tenant_id, provider)
				);

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'oauth_sessions')
				CREATE TABLE oauth_sessions (
					id             UNIQUEIDENTIFIER PRIMARY KEY,
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					provider       NVARCHAR(MAX) NOT NULL,
					session_token  NVARCHAR(255) NOT NULL,
					user_email     NVARCHAR(255),
					access_token   NVARCHAR(MAX),
					refresh_token  NVARCHAR(MAX),
					expires_at     DATETIMEOFFSET,
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_oauth_sessions_tenant_user' AND object_id = OBJECT_ID('oauth_sessions'))
				CREATE INDEX idx_oauth_sessions_tenant_user ON oauth_sessions(tenant_id, user_email);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_oauth_sessions_token' AND object_id = OBJECT_ID('oauth_sessions'))
				CREATE UNIQUE INDEX idx_oauth_sessions_token ON oauth_sessions(session_token);
			`,
			Down: `
				DROP TABLE IF EXISTS oauth_sessions;
				DROP TABLE IF EXISTS oauth_config;
			`,
		},
		{
			Version: 2,
			Up: `
				ALTER TABLE oauth_sessions ADD COLUMN IF NOT EXISTS state TEXT;
				ALTER TABLE oauth_sessions ADD COLUMN IF NOT EXISTS code_verifier TEXT;
				ALTER TABLE oauth_sessions ADD COLUMN IF NOT EXISTS token_hash TEXT;
				ALTER TABLE oauth_sessions ALTER COLUMN session_token DROP NOT NULL;
				DROP INDEX IF EXISTS idx_oauth_sessions_token;
				CREATE INDEX IF NOT EXISTS idx_oauth_sessions_state ON oauth_sessions(state);
				CREATE INDEX IF NOT EXISTS idx_oauth_sessions_token_hash ON oauth_sessions(token_hash);
			`,
			UpMySQL: `
				ALTER TABLE oauth_sessions ADD COLUMN state VARCHAR(255);
				ALTER TABLE oauth_sessions ADD COLUMN code_verifier TEXT;
				ALTER TABLE oauth_sessions ADD COLUMN token_hash VARCHAR(255);
				ALTER TABLE oauth_sessions MODIFY COLUMN session_token VARCHAR(255) NULL;
				DROP INDEX idx_oauth_sessions_token ON oauth_sessions;
				CREATE INDEX idx_oauth_sessions_state ON oauth_sessions(state);
				CREATE INDEX idx_oauth_sessions_token_hash ON oauth_sessions(token_hash);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('oauth_sessions') AND name = 'state')
				ALTER TABLE oauth_sessions ADD state NVARCHAR(255);
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('oauth_sessions') AND name = 'code_verifier')
				ALTER TABLE oauth_sessions ADD code_verifier NVARCHAR(MAX);
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('oauth_sessions') AND name = 'token_hash')
				ALTER TABLE oauth_sessions ADD token_hash NVARCHAR(255);
				ALTER TABLE oauth_sessions ALTER COLUMN session_token NVARCHAR(255) NULL;
				DROP INDEX idx_oauth_sessions_token ON oauth_sessions;
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_oauth_sessions_state' AND object_id = OBJECT_ID('oauth_sessions'))
				CREATE INDEX idx_oauth_sessions_state ON oauth_sessions(state);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_oauth_sessions_token_hash' AND object_id = OBJECT_ID('oauth_sessions'))
				CREATE INDEX idx_oauth_sessions_token_hash ON oauth_sessions(token_hash);
			`,
			Down: `
				DROP INDEX IF EXISTS idx_oauth_sessions_token_hash;
				DROP INDEX IF EXISTS idx_oauth_sessions_state;
				ALTER TABLE oauth_sessions DROP COLUMN IF EXISTS token_hash;
				ALTER TABLE oauth_sessions DROP COLUMN IF EXISTS code_verifier;
				ALTER TABLE oauth_sessions DROP COLUMN IF EXISTS state;
			`,
		},
	}
}
