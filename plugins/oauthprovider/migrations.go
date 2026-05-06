package oauthprovider

import "github.com/rcownie/durable/internal/plugin"

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
