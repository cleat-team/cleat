package notifications

import "github.com/cleat-team/cleat/plugin"

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
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS webhook_config (
					tenant_id   CHAR(36) NOT NULL,
					id          CHAR(36) PRIMARY KEY,
					url         TEXT NOT NULL,
					secret      VARCHAR(900) NOT NULL DEFAULT '',
					events      JSON NOT NULL DEFAULT ('[]'),
					enabled     TINYINT(1) NOT NULL DEFAULT 1,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE TABLE IF NOT EXISTS webhook_delivery (
					id               CHAR(36) PRIMARY KEY,
					webhook_id       CHAR(36) NOT NULL REFERENCES webhook_config(id),
					event_type       TEXT NOT NULL,
					payload          JSON NOT NULL DEFAULT ('{}'),
					` + "`status`" + `         VARCHAR(255) NOT NULL DEFAULT 'pending',
					attempt_count    INT NOT NULL DEFAULT 0,
					last_attempt_at  TIMESTAMP(6) NULL,
					next_attempt_at  TIMESTAMP(6) NULL,
					delivered_at     TIMESTAMP(6) NULL,
					response_code    INT,
					response_body    TEXT,
					created_at       TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE INDEX idx_webhook_config_tenant ON webhook_config(tenant_id);
				CREATE INDEX idx_webhook_delivery_webhook ON webhook_delivery(webhook_id);
				CREATE INDEX idx_webhook_delivery_status ON webhook_delivery(` + "`status`" + `, next_attempt_at);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'webhook_config')
				CREATE TABLE webhook_config (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					id          UNIQUEIDENTIFIER PRIMARY KEY,
					url         NVARCHAR(MAX) NOT NULL,
					secret      NVARCHAR(MAX) NOT NULL DEFAULT '',
					events      NVARCHAR(MAX) NOT NULL DEFAULT ('[]'),
					enabled     BIT NOT NULL DEFAULT 1,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'webhook_delivery')
				CREATE TABLE webhook_delivery (
					id               UNIQUEIDENTIFIER PRIMARY KEY,
					webhook_id       UNIQUEIDENTIFIER NOT NULL REFERENCES webhook_config(id),
					event_type       NVARCHAR(MAX) NOT NULL,
					payload          NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					[status]         NVARCHAR(255) NOT NULL DEFAULT 'pending',
					attempt_count    INT NOT NULL DEFAULT 0,
					last_attempt_at  DATETIMEOFFSET,
					next_attempt_at  DATETIMEOFFSET,
					delivered_at     DATETIMEOFFSET,
					response_code    INT,
					response_body    NVARCHAR(MAX),
					created_at       DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_config_tenant' AND object_id = OBJECT_ID('webhook_config'))
				CREATE INDEX idx_webhook_config_tenant ON webhook_config(tenant_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_delivery_webhook' AND object_id = OBJECT_ID('webhook_delivery'))
				CREATE INDEX idx_webhook_delivery_webhook ON webhook_delivery(webhook_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_delivery_status' AND object_id = OBJECT_ID('webhook_delivery'))
				CREATE INDEX idx_webhook_delivery_status ON webhook_delivery([status], next_attempt_at);
			`,
			Down: `
				DROP TABLE IF EXISTS webhook_delivery;
				DROP TABLE IF EXISTS webhook_config;
			`,
		},
		{
			// Tenant isolation for webhook_config. cleat#1512.
			//
			// A NEW VERSION, NEVER AN EDIT TO v1. A recorded migration never
			// runs again, so editing v1 would protect databases created after
			// this lands and leave every existing one open.
			//
			// Up and Down are empty on purpose. The runtime emits ENABLE /
			// FORCE / the policy from TenantScoped (plugin.applyTenantScoping),
			// using cleat.tenant_row_is_visible so a sweep that named itself
			// through plugin.AcrossAllTenants is admitted and an unmarked one
			// still fails closed. On MySQL and SQL Server this version is
			// recorded and installs nothing, which is what the field means.
			//
			// webhook_delivery IS NOT HERE, AND CANNOT BE. It has no tenant_id
			// column -- it is scoped through webhook_id REFERENCES
			// webhook_config(id) -- and the policy this field emits is
			// cleat.tenant_row_is_visible(tenant_id), so declaring it would
			// fail the migration outright on "column tenant_id does not exist".
			// Its isolation therefore still rests entirely on the joins and
			// predicates in hand-written SQL, which is the thing cleat#1512
			// exists to stop relying on. That is a gap this change does not
			// close and does not widen; it needs a tenant_id column, which is a
			// data migration rather than a policy, and it is filed separately
			// rather than smuggled in here.
			Version:      2,
			TenantScoped: []string{"webhook_config"},
		},
	}
}
