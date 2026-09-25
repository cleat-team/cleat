package oauthprovider

import "github.com/cleat-team/cleat/plugin"

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
		{
			// Tenant isolation for both OAuth tables. cleat#1512.
			//
			// oauth_sessions holds session_token, token_hash, access_token and
			// refresh_token, which makes it the most sensitive table in this
			// rollout and the one where the naive conversion breaks login.
			//
			// THREE of its statements DERIVE the tenant and cannot be scoped:
			// the middleware's token-hash lookup, extractSession's, and the
			// callback's lookup by OAuth `state`. Each runs on a request that
			// has no tenant, because producing one is what the query is for. A
			// policy calling cleat.assert_tenant_set() makes all three RAISE,
			// so shipping this migration without marking them would have taken
			// session authentication down entirely. They are marked with
			// plugin.AcrossAllTenants and each says why.
			//
			// Everything else is scoped with plugin.ForTenant from the tenant
			// in hand rather than from the request context, because this
			// plugin's paths are frequently UNAUTHENTICATED -- handleLogin
			// accepts ?tenant_id= precisely because a login has no session yet
			// -- so the context carrier the policy reads is often empty even
			// though the tenant is known.
			Version:      3,
			TenantScoped: []string{"oauth_config", "oauth_sessions"},
		},
		{
			// A generic OIDC issuer, so a customer is not limited to the three
			// providers cleat happens to name. cleat#1582, implementing
			// docs/enterprise-identity-decision.md.
			//
			// TWO columns, and they are on different tables for different
			// reasons.
			//
			// oauth_config.issuer holds the issuer URL for provider 'oidc'.
			// It is empty for google/github/okta, whose endpoints stay in the
			// hardcoded table -- those become sugar over the same path rather
			// than a second mechanism.
			//
			// oauth_sessions.nonce is what makes an ID token checkable. The
			// nonce is minted at login, sent on the authorize request, and
			// must come back inside the signed token; without somewhere to
			// remember it between the two requests there is nothing to compare
			// against, and an unchecked nonce is a replay.
			//
			// Nullable with no default, because every row written before this
			// migration legitimately has neither. The callback treats absent
			// as "this flow predates the column" rather than as a mismatch.
			Version: 4,
			Up: `
				ALTER TABLE oauth_config ADD COLUMN IF NOT EXISTS issuer TEXT NOT NULL DEFAULT '';
				ALTER TABLE oauth_sessions ADD COLUMN IF NOT EXISTS nonce TEXT;
			`,
			UpMySQL: `
				ALTER TABLE oauth_config ADD COLUMN issuer VARCHAR(900) NOT NULL DEFAULT '';
				ALTER TABLE oauth_sessions ADD COLUMN nonce VARCHAR(255);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('oauth_config') AND name = 'issuer')
				ALTER TABLE oauth_config ADD issuer NVARCHAR(900) NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('oauth_sessions') AND name = 'nonce')
				ALTER TABLE oauth_sessions ADD nonce NVARCHAR(255);
			`,
			Down: `
				ALTER TABLE oauth_sessions DROP COLUMN IF EXISTS nonce;
				ALTER TABLE oauth_config DROP COLUMN IF EXISTS issuer;
			`,
		},
		{
			// oauth_config.client_secret moves into plugin.Secrets /
			// tenant_secrets, the same envelope-encrypted store
			// dd_config.api_key and pd_config.routing_key already use
			// (#2179). cleat#1992.
			//
			// ONE FIXED NAME PER (TENANT, PROVIDER), not per config id like
			// dd_config/pd_config: oauth_config's own primary key is
			// (tenant_id, provider), so a tenant can have at most one row
			// per provider already -- keying the secret name by provider
			// alone (OAuthClientSecretName, routes.go) preserves that, with
			// no risk of two configs colliding under one name.
			//
			// No backfill step: cleat#2058 (owner decision 3) settled that
			// 0.3.0 requires a fresh database, with no upgrade path from
			// v0.2.0, so no deployment ever has a plaintext value in this
			// column that needs to survive the DROP.
			//
			// oauth_sessions.session_token/access_token/refresh_token are
			// NOT touched by this migration. They move to plugin.Payloads
			// instead of plugin.Secrets (high-churn, one per session, minted
			// on every login rather than operator-set), which seals VALUES
			// rather than moving them to a different store -- so those
			// columns keep their existing TEXT/NVARCHAR(MAX)/VARCHAR(255)
			// types and hold base64(sealed) instead of plaintext from this
			// version onward. See finishLogin's own comment in routes.go for
			// the rotation caveat this implies for refresh_token
			// specifically.
			Version: 5,
			Up: `
				ALTER TABLE oauth_config DROP COLUMN IF EXISTS client_secret;
			`,
			UpMySQL: `
				ALTER TABLE oauth_config DROP COLUMN client_secret;
			`,
			UpMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('oauth_config') AND name = 'client_secret')
				ALTER TABLE oauth_config DROP COLUMN client_secret;
			`,
			// Down restores the SCHEMA, not the data -- the value is gone
			// from oauth_config the moment Up runs; it now lives in tenant
			// secrets, a different store. No NOT NULL: existing rows have
			// nothing to put there.
			Down: `
				ALTER TABLE oauth_config ADD COLUMN IF NOT EXISTS client_secret TEXT;
			`,
			DownMySQL: `
				ALTER TABLE oauth_config ADD COLUMN client_secret TEXT;
			`,
			DownMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('oauth_config') AND name = 'client_secret')
				ALTER TABLE oauth_config ADD client_secret NVARCHAR(MAX);
			`,
		},
	}
}
