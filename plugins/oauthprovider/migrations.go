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
	}
}
